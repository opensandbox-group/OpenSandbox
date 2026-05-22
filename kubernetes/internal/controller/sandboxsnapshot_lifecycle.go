// Copyright 2025 Alibaba Group Holding Ltd.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils"
)

// handlePending resolves the source Pod and creates the commit Job.
func (r *SandboxSnapshotReconciler) handlePending(ctx context.Context, snapshot *sandboxv1alpha1.SandboxSnapshot) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if r.SnapshotRegistry == "" {
		msg := "snapshot-registry not configured in controller manager"
		log.Error(nil, msg)
		_ = r.updateSnapshotStatus(ctx, snapshot, sandboxv1alpha1.SandboxSnapshotPhaseFailed, "RegistryNotConfigured", msg)
		return ctrl.Result{}, nil
	}

	bs := &sandboxv1alpha1.BatchSandbox{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      snapshot.Spec.SandboxName,
		Namespace: snapshot.Namespace,
	}, bs); err != nil {
		msg := fmt.Sprintf("failed to get BatchSandbox %s: %v", snapshot.Spec.SandboxName, err)
		_ = r.updateSnapshotStatus(ctx, snapshot, sandboxv1alpha1.SandboxSnapshotPhaseFailed, "BatchSandboxLookupFailed", msg)
		return ctrl.Result{}, nil
	}

	pod, err := r.findPodForSandbox(ctx, bs, snapshot.Namespace)
	if err != nil {
		msg := fmt.Sprintf("source pod not found: %v", err)
		log.Error(err, msg)
		_ = r.updateSnapshotStatus(ctx, snapshot, sandboxv1alpha1.SandboxSnapshotPhaseFailed, "SourcePodNotFound", msg)
		return ctrl.Result{}, nil
	}

	sourcePodName := pod.Name
	sourceNodeName := pod.Spec.NodeName

	sourceContainers := pod.Spec.Containers
	if bs.Spec.Template != nil {
		sourceContainers = bs.Spec.Template.Spec.Containers
	}

	var containers []sandboxv1alpha1.ContainerSnapshot
	for _, c := range sourceContainers {
		imageURI := r.snapshotImageURI(snapshot, bs, c.Name)
		containers = append(containers, sandboxv1alpha1.ContainerSnapshot{
			ContainerName: c.Name,
			ImageURI:      imageURI,
		})
	}
	if len(containers) == 0 {
		msg := fmt.Sprintf("no containers found in BatchSandbox %s template", bs.Name)
		_ = r.updateSnapshotStatus(ctx, snapshot, sandboxv1alpha1.SandboxSnapshotPhaseFailed, "NoContainers", msg)
		return ctrl.Result{}, nil
	}

	if err := r.persistResolvedData(ctx, snapshot, sourcePodName, sourceNodeName, containers); err != nil {
		return ctrl.Result{}, err
	}
	snapshot.Status.SourcePodName = sourcePodName
	snapshot.Status.SourceNodeName = sourceNodeName
	snapshot.Status.Containers = containers

	job, err := r.buildCommitJob(snapshot)
	if err != nil {
		msg := fmt.Sprintf("failed to build commit job: %v", err)
		_ = r.updateSnapshotStatus(ctx, snapshot, sandboxv1alpha1.SandboxSnapshotPhaseFailed, "BuildCommitJobFailed", msg)
		return ctrl.Result{}, nil
	}

	existingJob := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: job.Namespace, Name: job.Name}, existingJob); err == nil {
		log.Info("Commit job already exists", "job", job.Name)
		_ = r.updateSnapshotStatus(ctx, snapshot, sandboxv1alpha1.SandboxSnapshotPhaseCommitting, "Committing", "Commit job already exists")
		return ctrl.Result{RequeueAfter: time.Second}, nil
	} else if !errors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	if err := r.Create(ctx, job); err != nil {
		log.Error(err, "Failed to create commit job")
		r.Recorder.Eventf(snapshot, corev1.EventTypeWarning, "FailedCreateJob", "Failed to create commit job: %v", err)
		return ctrl.Result{}, err
	}

	log.Info("Created commit job", "job", job.Name)
	r.Recorder.Eventf(snapshot, corev1.EventTypeNormal, "CreatedJob", "Created commit job: %s", job.Name)
	_ = r.updateSnapshotStatus(ctx, snapshot, sandboxv1alpha1.SandboxSnapshotPhaseCommitting, "Committing", "Commit job created")

	return ctrl.Result{RequeueAfter: time.Second}, nil
}

// handleCommitting checks the commit Job status and transitions to Succeed or Failed.
func (r *SandboxSnapshotReconciler) handleCommitting(ctx context.Context, snapshot *sandboxv1alpha1.SandboxSnapshot) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	jobName := r.getJobName(snapshot)
	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: snapshot.Namespace, Name: jobName}, job); err != nil {
		if errors.IsNotFound(err) {
			log.Info("Commit job not found, re-creating", "job", jobName)
			return r.handlePending(ctx, snapshot)
		}
		return ctrl.Result{}, err
	}

	if job.Status.Succeeded > 0 {
		log.Info("Commit job succeeded", "job", jobName)
		if err := r.updateSnapshotStatusFromSucceededCommitJob(ctx, snapshot, job); err != nil {
			return ctrl.Result{}, err
		}
		r.Recorder.Eventf(snapshot, corev1.EventTypeNormal, "JobSucceeded", "Commit job succeeded")
		return ctrl.Result{}, nil
	}

	if failedCond := findJobCondition(job.Status.Conditions, batchv1.JobFailed); failedCond != nil {
		message := "Commit job failed"
		if failedCond.Message != "" {
			message = failedCond.Message
		}
		log.Info("Commit job failed", "job", jobName, "message", message)
		if err := r.ensureUnpauseJob(ctx, snapshot); err != nil {
			log.Error(err, "Failed to create best-effort unpause job")
		}
		r.Recorder.Eventf(snapshot, corev1.EventTypeWarning, "JobFailed", "Commit job failed")
		_ = r.updateSnapshotStatus(ctx, snapshot, sandboxv1alpha1.SandboxSnapshotPhaseFailed, "CommitJobFailed", message)
		return ctrl.Result{}, nil
	}

	log.Info("Commit job still running", "job", jobName)
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

func findJobCondition(conditions []batchv1.JobCondition, conditionType batchv1.JobConditionType) *batchv1.JobCondition {
	for i := range conditions {
		if conditions[i].Type == conditionType && conditions[i].Status == corev1.ConditionTrue {
			return &conditions[i]
		}
	}
	return nil
}

// handleDeletion cleans up the commit job and removes the finalizer.
func (r *SandboxSnapshotReconciler) handleDeletion(ctx context.Context, snapshot *sandboxv1alpha1.SandboxSnapshot) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	jobName := r.getJobName(snapshot)
	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: snapshot.Namespace, Name: jobName}, job); err == nil {
		if deleteErr := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); deleteErr != nil && !errors.IsNotFound(deleteErr) {
			return ctrl.Result{}, deleteErr
		}
		log.Info("Deleted commit job", "job", jobName)
	}

	unpauseJobName := r.getUnpauseJobName(snapshot)
	unpauseJob := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: snapshot.Namespace, Name: unpauseJobName}, unpauseJob); err == nil {
		if deleteErr := r.Delete(ctx, unpauseJob, client.PropagationPolicy(metav1.DeletePropagationBackground)); deleteErr != nil && !errors.IsNotFound(deleteErr) {
			return ctrl.Result{}, deleteErr
		}
		log.Info("Deleted unpause job", "job", unpauseJobName)
	}

	if controllerutil.ContainsFinalizer(snapshot, SandboxSnapshotFinalizer) {
		if err := utils.UpdateFinalizer(r.Client, snapshot, utils.RemoveFinalizerOpType, SandboxSnapshotFinalizer); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

// findPodForSandbox finds the running pod belonging to a BatchSandbox.
func (r *SandboxSnapshotReconciler) findPodForSandbox(ctx context.Context, bs *sandboxv1alpha1.BatchSandbox, namespace string) (*corev1.Pod, error) {
	alloc, err := parseSandboxAllocation(bs)
	if err == nil && len(alloc.Pods) > 0 {
		for _, podName := range alloc.Pods {
			pod := &corev1.Pod{}
			if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: podName}, pod); err == nil {
				if pod.Status.Phase == corev1.PodRunning {
					return pod, nil
				}
			}
		}
	}

	podList := &corev1.PodList{}
	if err := r.List(ctx, podList,
		client.InNamespace(namespace),
		client.MatchingLabels{LabelBatchSandboxNameKey: bs.Name},
	); err != nil {
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}
	for i := range podList.Items {
		if podList.Items[i].Status.Phase == corev1.PodRunning {
			return &podList.Items[i], nil
		}
	}

	podName := fmt.Sprintf("%s-%d", bs.Name, batchSandboxFirstPodIndex)
	pod := &corev1.Pod{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: podName}, pod); err == nil {
		if pod.Status.Phase == corev1.PodRunning {
			return pod, nil
		}
	}

	return nil, fmt.Errorf("no running pod found for BatchSandbox %s", bs.Name)
}

func (r *SandboxSnapshotReconciler) snapshotImageURI(
	snapshot *sandboxv1alpha1.SandboxSnapshot,
	bs *sandboxv1alpha1.BatchSandbox,
	containerName string,
) string {
	return fmt.Sprintf(
		"%s/%s-%s:%s",
		r.SnapshotRegistry,
		bs.Name,
		containerName,
		snapshotImageTag(snapshot, bs),
	)
}

func snapshotImageTag(snapshot *sandboxv1alpha1.SandboxSnapshot, bs *sandboxv1alpha1.BatchSandbox) string {
	if hasBatchSandboxControllerOwner(snapshot) {
		return fmt.Sprintf("snap-gen%d", bs.Generation)
	}
	return publicSnapshotImageTag(snapshot.Name)
}

func hasBatchSandboxControllerOwner(snapshot *sandboxv1alpha1.SandboxSnapshot) bool {
	for _, owner := range snapshot.OwnerReferences {
		if owner.Kind != "BatchSandbox" {
			continue
		}
		if owner.Controller != nil && *owner.Controller {
			return true
		}
	}
	return false
}

func publicSnapshotImageTag(snapshotName string) string {
	const publicSnapshotNamePrefix = "osb-snap-"
	if strings.HasPrefix(snapshotName, publicSnapshotNamePrefix) {
		suffix := strings.TrimPrefix(snapshotName, publicSnapshotNamePrefix)
		if isLowerHex(suffix) && len(suffix) == 32 {
			return "snap-" + suffix
		}
	}

	sum := sha256.Sum256([]byte(snapshotName))
	return fmt.Sprintf("snap-%x", sum)[:37]
}

func isLowerHex(value string) bool {
	for _, ch := range value {
		if (ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') {
			continue
		}
		return false
	}
	return true
}

func (r *SandboxSnapshotReconciler) imageCommitterImage() string {
	if r.ImageCommitterImage != "" {
		return r.ImageCommitterImage
	}
	return "image-committer:dev"
}

func (r *SandboxSnapshotReconciler) containerdSocketPath() string {
	if r.ContainerdSocketPath != "" {
		return r.ContainerdSocketPath
	}
	return ContainerdSocketPath
}

func commitJobSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		RunAsUser:                ptrToInt64(0),
		AllowPrivilegeEscalation: ptrToBool(false),
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
		},
	}
}

func (r *SandboxSnapshotReconciler) buildCommitJob(snapshot *sandboxv1alpha1.SandboxSnapshot) (*batchv1.Job, error) {
	jobName := r.getJobName(snapshot)
	imageCommitterImage := r.imageCommitterImage()

	volumeMounts := []corev1.VolumeMount{
		{Name: "containerd-sock", MountPath: ContainerdSocketPath},
	}
	volumes := []corev1.Volume{
		{
			Name: "containerd-sock",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: r.containerdSocketPath()},
			},
		},
	}

	if r.SnapshotPushSecret != "" {
		volumes = append(volumes, corev1.Volume{
			Name: "registry-creds",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: r.SnapshotPushSecret,
					Items: []corev1.KeyToPath{
						{Key: ".dockerconfigjson", Path: "config.json"},
					},
				},
			},
		})
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name: "registry-creds", MountPath: "/var/run/opensandbox/registry", ReadOnly: true,
		})
	}

	var containerSpecs []string
	for _, cs := range snapshot.Status.Containers {
		containerSpecs = append(containerSpecs, fmt.Sprintf("%s:%s", cs.ContainerName, cs.ImageURI))
	}
	args := append([]string{snapshot.Status.SourcePodName, snapshot.Namespace}, containerSpecs...)
	env := []corev1.EnvVar{{Name: "CONTAINERD_SOCKET", Value: ContainerdSocketPath}}
	if r.SnapshotRegistryInsecure {
		env = append(env, corev1.EnvVar{Name: "SNAPSHOT_REGISTRY_INSECURE", Value: "true"})
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: snapshot.Namespace,
			Labels: map[string]string{
				LabelSandboxSnapshotName:  snapshot.Name,
				LabelPrivilegedNodeAccess: "true",
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptrToInt32(DefaultCommitJobBackoffLimit),
			TTLSecondsAfterFinished: ptrToInt32(int32(DefaultTTLSecondsAfterFinished)),
			ActiveDeadlineSeconds:   ptrToInt64(int64(r.getCommitJobTimeout().Seconds())),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:            CommitJobContainerName,
							Image:           imageCommitterImage,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Command:         []string{"/usr/local/bin/image-committer"},
							Args:            args,
							VolumeMounts:    volumeMounts,
							Env:             env,
							SecurityContext: commitJobSecurityContext(),
						},
					},
					Volumes:  volumes,
					NodeName: snapshot.Status.SourceNodeName,
				},
			},
		},
	}

	if err := ctrl.SetControllerReference(snapshot, job, r.Scheme); err != nil {
		return nil, fmt.Errorf("failed to set controller reference: %w", err)
	}
	return job, nil
}

func (r *SandboxSnapshotReconciler) ensureUnpauseJob(ctx context.Context, snapshot *sandboxv1alpha1.SandboxSnapshot) error {
	if snapshot.Status.SourcePodName == "" || snapshot.Status.SourceNodeName == "" || len(snapshot.Status.Containers) == 0 {
		return nil
	}

	jobName := r.getUnpauseJobName(snapshot)
	existingJob := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: snapshot.Namespace, Name: jobName}, existingJob); err == nil {
		return nil
	} else if !errors.IsNotFound(err) {
		return err
	}

	job, err := r.buildUnpauseJob(snapshot)
	if err != nil {
		return err
	}
	return r.Create(ctx, job)
}

func (r *SandboxSnapshotReconciler) buildUnpauseJob(snapshot *sandboxv1alpha1.SandboxSnapshot) (*batchv1.Job, error) {
	var containerNames []string
	for _, cs := range snapshot.Status.Containers {
		containerNames = append(containerNames, cs.ContainerName)
	}
	args := append([]string{"unpause", snapshot.Status.SourcePodName, snapshot.Namespace}, containerNames...)

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      r.getUnpauseJobName(snapshot),
			Namespace: snapshot.Namespace,
			Labels: map[string]string{
				LabelSandboxSnapshotName:  snapshot.Name,
				LabelPrivilegedNodeAccess: "true",
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptrToInt32(0),
			TTLSecondsAfterFinished: ptrToInt32(int32(DefaultTTLSecondsAfterFinished)),
			ActiveDeadlineSeconds:   ptrToInt64(int64(r.getCommitJobTimeout().Seconds())),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:            CommitJobContainerName,
							Image:           r.imageCommitterImage(),
							ImagePullPolicy: corev1.PullIfNotPresent,
							Command:         []string{"/usr/local/bin/image-committer"},
							Args:            args,
							VolumeMounts: []corev1.VolumeMount{
								{Name: "containerd-sock", MountPath: ContainerdSocketPath},
							},
							Env: []corev1.EnvVar{
								{Name: "CONTAINERD_SOCKET", Value: ContainerdSocketPath},
							},
							SecurityContext: commitJobSecurityContext(),
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "containerd-sock",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{Path: r.containerdSocketPath()},
							},
						},
					},
					NodeName: snapshot.Status.SourceNodeName,
				},
			},
		},
	}

	if err := ctrl.SetControllerReference(snapshot, job, r.Scheme); err != nil {
		return nil, fmt.Errorf("failed to set controller reference: %w", err)
	}
	return job, nil
}

type commitJobResult struct {
	Containers []commitJobContainerResult `json:"containers"`
}

type commitJobContainerResult struct {
	Name   string `json:"name"`
	Image  string `json:"image"`
	Digest string `json:"digest"`
}

func (r *SandboxSnapshotReconciler) updateSnapshotStatusFromSucceededCommitJob(ctx context.Context, snapshot *sandboxv1alpha1.SandboxSnapshot, job *batchv1.Job) error {
	result, found, err := r.getCommitJobResult(ctx, snapshot.Namespace, job.Name)
	if err != nil {
		return err
	}
	digests := map[string]string{}
	if found {
		digests = make(map[string]string, len(result.Containers))
		for _, container := range result.Containers {
			if container.Name == "" || container.Digest == "" {
				continue
			}
			digests[container.Name] = container.Digest
		}
	}

	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &sandboxv1alpha1.SandboxSnapshot{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: snapshot.Namespace, Name: snapshot.Name}, latest); err != nil {
			return err
		}
		if isTerminalSnapshotPhase(latest.Status.Phase) && latest.Status.Phase != sandboxv1alpha1.SandboxSnapshotPhaseSucceed {
			return nil
		}
		for i := range latest.Status.Containers {
			if digest, ok := digests[latest.Status.Containers[i].ContainerName]; ok {
				latest.Status.Containers[i].ImageDigest = digest
			}
		}
		latest.Status.Phase = sandboxv1alpha1.SandboxSnapshotPhaseSucceed
		applySnapshotPhaseConditions(&latest.Status, "", "")
		return r.Status().Update(ctx, latest)
	})
}

func (r *SandboxSnapshotReconciler) getCommitJobResult(ctx context.Context, namespace, jobName string) (*commitJobResult, bool, error) {
	for _, labelKey := range []string{"job-name", "batch.kubernetes.io/job-name"} {
		podList := &corev1.PodList{}
		if err := r.List(ctx, podList,
			client.InNamespace(namespace),
			client.MatchingLabels{labelKey: jobName},
		); err != nil {
			return nil, false, err
		}
		for i := range podList.Items {
			if result, found, err := snapshotResultFromPod(&podList.Items[i]); found || err != nil {
				return result, found, err
			}
		}
	}
	return nil, false, nil
}

func snapshotResultFromPod(pod *corev1.Pod) (*commitJobResult, bool, error) {
	if pod == nil {
		return nil, false, nil
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name != CommitJobContainerName || status.State.Terminated == nil {
			continue
		}
		if status.State.Terminated.ExitCode != 0 {
			continue
		}
		message := strings.TrimSpace(status.State.Terminated.Message)
		if message == "" {
			return nil, false, nil
		}
		var result commitJobResult
		if err := json.Unmarshal([]byte(message), &result); err != nil {
			return nil, false, fmt.Errorf("failed to parse commit job termination message from pod %s: %w", pod.Name, err)
		}
		return &result, true, nil
	}
	return nil, false, nil
}

func (r *SandboxSnapshotReconciler) getJobName(snapshot *sandboxv1alpha1.SandboxSnapshot) string {
	return fmt.Sprintf("%s-commit", snapshot.Name)
}

func (r *SandboxSnapshotReconciler) getUnpauseJobName(snapshot *sandboxv1alpha1.SandboxSnapshot) string {
	return fmt.Sprintf("%s-unpause", snapshot.Name)
}
