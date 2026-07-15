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
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/controller/strategy"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils"
	controllerutils "github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils/controller"
)

const internalPauseSnapshotSuffix = "-pause"
const supportedPauseReplicas int32 = 1

func internalPauseSnapshotName(batchSandboxName string) string {
	// TODO: handle Kubernetes resource name length limits for long BatchSandbox names.
	return batchSandboxName + internalPauseSnapshotSuffix
}

func unsupportedPauseReplicasMessage(replicas *int32) string {
	if replicas == nil {
		return "pause/resume currently supports only BatchSandbox spec.replicas=1; spec.replicas is unset"
	}
	return fmt.Sprintf("pause/resume currently supports only BatchSandbox spec.replicas=1; got spec.replicas=%d", *replicas)
}

func ensureImagePullSecret(template *corev1.PodTemplateSpec, secretName string) {
	if template == nil || secretName == "" {
		return
	}
	for _, secret := range template.Spec.ImagePullSecrets {
		if secret.Name == secretName {
			return
		}
	}
	template.Spec.ImagePullSecrets = append(template.Spec.ImagePullSecrets, corev1.LocalObjectReference{Name: secretName})
}

func snapshotFailureMessage(snapshot *sandboxv1alpha1.SandboxSnapshot) string {
	for _, cond := range snapshot.Status.Conditions {
		if cond.Type == sandboxv1alpha1.SandboxSnapshotConditionFailed && cond.Status == sandboxv1alpha1.ConditionTrue && cond.Message != "" {
			return cond.Message
		}
	}
	return "snapshot failed"
}

func copyPodTemplateMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	copied := make(map[string]string, len(values))
	for key, value := range values {
		copied[key] = value
	}
	if len(copied) == 0 {
		return nil
	}
	return copied
}

func sourcePodTemplateForPause(pod *corev1.Pod) *corev1.PodTemplateSpec {
	if pod == nil {
		return nil
	}
	labels := copyPodTemplateMap(pod.Labels)
	delete(labels, LabelPoolName)
	delete(labels, LabelPoolRevision)
	delete(labels, LabelBatchSandboxNameKey)
	delete(labels, LabelBatchSandboxPodIndexKey)

	spec := *pod.Spec.DeepCopy()
	spec.NodeName = ""

	return &corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels:      labels,
			Annotations: copyPodTemplateMap(pod.Annotations),
		},
		Spec: spec,
	}
}

func (r *BatchSandboxReconciler) deleteInternalPauseSnapshot(ctx context.Context, bs *sandboxv1alpha1.BatchSandbox) error {
	log := logf.FromContext(ctx)
	snapshot := &sandboxv1alpha1.SandboxSnapshot{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: bs.Namespace, Name: internalPauseSnapshotName(bs.Name)}, snapshot); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if snapshot.Status.Phase != sandboxv1alpha1.SandboxSnapshotPhaseSucceed {
		return nil
	}
	if err := r.Delete(ctx, snapshot); err != nil && !errors.IsNotFound(err) {
		return err
	}
	log.Info("Deleted SandboxSnapshot after successful resume", "snapshot", snapshot.Name)
	return nil
}

func (r *BatchSandboxReconciler) hasReadyResumePod(ctx context.Context, bs *sandboxv1alpha1.BatchSandbox) (bool, error) {
	poolStrategy := strategy.NewPoolStrategy(bs)
	if poolStrategy.IsPooledMode() {
		alloc, err := parseSandboxAllocation(bs)
		if err != nil {
			return false, err
		}
		for _, podName := range alloc.Pods {
			pod := &corev1.Pod{}
			if err := r.Get(ctx, types.NamespacedName{Namespace: bs.Namespace, Name: podName}, pod); err != nil {
				if errors.IsNotFound(err) {
					continue
				}
				return false, err
			}
			if utils.IsPodReady(pod) {
				return true, nil
			}
		}
	}

	podList := &corev1.PodList{}
	if err := r.List(ctx, podList,
		client.InNamespace(bs.Namespace),
		client.MatchingLabels{LabelBatchSandboxNameKey: bs.Name},
	); err != nil {
		return false, err
	}
	for i := range podList.Items {
		if utils.IsPodReady(&podList.Items[i]) {
			return true, nil
		}
	}

	pod := &corev1.Pod{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: bs.Namespace, Name: fmt.Sprintf("%s-%d", bs.Name, batchSandboxFirstPodIndex)}, pod); err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return utils.IsPodReady(pod), nil
}

// dispatchPauseResume implements the 5-case dispatch table from the design doc.
// Returns (result, handled, error). If handled=true, the caller should return immediately.
func (r *BatchSandboxReconciler) dispatchPauseResume(ctx context.Context, bs *sandboxv1alpha1.BatchSandbox) (ctrl.Result, bool, error) {
	log := logf.FromContext(ctx)
	generation := bs.Generation
	pauseObservedGen := bs.Status.PauseObservedGeneration
	pause := bs.Spec.Pause

	// In-progress phases take priority over generation checks so that controller-owned
	// spec mutations (template solidification, image replacement, pool detachment) do not
	// get mistaken for new external pause/resume requests.
	log.Info("Dispatch: checking phase", "currentPhase", bs.Status.Phase, "generation", generation, "pauseObservedGen", pauseObservedGen, "pause", pause)
	if bs.Status.Phase == sandboxv1alpha1.BatchSandboxPhaseResuming {
		log.Info("Dispatch: phase is Resuming, continuing resume")
		result, err := r.continueResume(ctx, bs)
		if err != nil {
			return result, true, err
		}
		// Return handled=false to let normal flow update phase from Resuming to Succeed.
		return result, false, nil
	}
	if bs.Status.Phase == sandboxv1alpha1.BatchSandboxPhasePausing {
		log.Info("Dispatch: phase is Pausing, syncing pause state")
		result, err := r.syncPauseOrClear(ctx, bs)
		return result, true, err
	}

	if generation > pauseObservedGen {
		if pause != nil {
			if *pause {
				log.Info("Dispatch: handlePause", "generation", generation, "pauseObservedGeneration", pauseObservedGen)
				result, err := r.handlePause(ctx, bs)
				return result, true, err
			}
			log.Info("Dispatch: handleResume", "generation", generation, "pauseObservedGeneration", pauseObservedGen, "currentPhase", bs.Status.Phase)
			result, err := r.handleResume(ctx, bs)
			return result, true, err
		}
		// No pause intent — skip the dedicated ACK API call. The normal flow's
		// persistRuntimeView will update PauseObservedGeneration in its status patch.
		log.Info("Dispatch: no pause intent, deferring ACK to status patch", "generation", generation, "pauseObservedGeneration", pauseObservedGen)
		return ctrl.Result{}, false, nil
	}

	return ctrl.Result{}, false, nil
}

// handlePause implements the pause flow:
// 1. ACK (pauseObservedGeneration + phase=Pausing)
// 2. Stop task-executor tasks, if any, while keeping the source Pod allocated
// 3. Create SandboxSnapshot child resource
func (r *BatchSandboxReconciler) handlePause(ctx context.Context, bs *sandboxv1alpha1.BatchSandbox) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// The pause snapshot format records one source pod's container images, so this
	// controller only supports rootfs pause/resume for single-replica sandboxes.
	if bs.Spec.Replicas == nil || *bs.Spec.Replicas != supportedPauseReplicas {
		msg := unsupportedPauseReplicasMessage(bs.Spec.Replicas)
		log.Info("Rejecting pause for unsupported replica count", "message", msg)
		phase := bs.Status.Phase
		if phase == "" {
			phase = sandboxv1alpha1.BatchSandboxPhaseSucceed
		}
		if err := r.ackPauseWithPhase(ctx, bs, phase, ""); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.setCondition(ctx, bs, sandboxv1alpha1.BatchSandboxConditionPauseFailed, sandboxv1alpha1.ConditionTrue, "UnsupportedReplicas", msg); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	snapshot := &sandboxv1alpha1.SandboxSnapshot{}
	snapshotName := internalPauseSnapshotName(bs.Name)
	err := r.Get(ctx, types.NamespacedName{Namespace: bs.Namespace, Name: snapshotName}, snapshot)
	if err == nil && snapshot.DeletionTimestamp != nil {
		log.Info("Waiting for stale SandboxSnapshot deletion before retrying pause", "snapshot", snapshotName)
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	if err == nil && (snapshot.Status.Phase == sandboxv1alpha1.SandboxSnapshotPhaseFailed || snapshot.Status.Phase == sandboxv1alpha1.SandboxSnapshotPhaseSucceed) {
		log.Info("Deleting terminal SandboxSnapshot for retry", "snapshot", snapshotName, "phase", snapshot.Status.Phase)
		if err := r.Delete(ctx, snapshot); err != nil && !errors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	if err != nil && !errors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	_ = r.setCondition(ctx, bs, sandboxv1alpha1.BatchSandboxConditionPauseFailed, sandboxv1alpha1.ConditionFalse, "", "")

	if err := r.ackPauseWithPhase(ctx, bs, sandboxv1alpha1.BatchSandboxPhasePausing, ""); err != nil {
		return ctrl.Result{}, err
	}

	if errors.IsNotFound(err) {
		if created, err := r.ensureInternalPauseSnapshot(ctx, bs, snapshotName); err != nil {
			return ctrl.Result{}, err
		} else if !created {
			log.Info("Waiting for task cleanup before creating SandboxSnapshot", "snapshot", snapshotName)
		}
	}

	return ctrl.Result{RequeueAfter: time.Second}, nil
}

func (r *BatchSandboxReconciler) ensureInternalPauseSnapshot(ctx context.Context, bs *sandboxv1alpha1.BatchSandbox, snapshotName string) (bool, error) {
	log := logf.FromContext(ctx)

	tasksStopped, err := r.stopTasksBeforePause(ctx, bs)
	if err != nil {
		return false, err
	}
	if !tasksStopped {
		return false, nil
	}

	snapshot := &sandboxv1alpha1.SandboxSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      snapshotName,
			Namespace: bs.Namespace,
		},
		Spec: sandboxv1alpha1.SandboxSnapshotSpec{
			SandboxName: bs.Name,
		},
	}
	if err := controllerutil.SetControllerReference(bs, snapshot, r.Scheme); err != nil {
		return false, err
	}
	if err := r.Create(ctx, snapshot); err != nil {
		if errors.IsAlreadyExists(err) {
			return true, nil
		}
		return false, err
	}
	log.Info("Created SandboxSnapshot", "snapshot", snapshotName)
	return true, nil
}

func (r *BatchSandboxReconciler) stopTasksBeforePause(ctx context.Context, bs *sandboxv1alpha1.BatchSandbox) (bool, error) {
	log := logf.FromContext(ctx)
	taskStrategy := strategy.NewTaskSchedulingStrategy(bs)
	if !taskStrategy.NeedTaskScheduling() {
		return true, nil
	}

	poolStrategy := strategy.NewPoolStrategy(bs)
	pods, err := r.listPods(ctx, poolStrategy, bs)
	if err != nil {
		return false, err
	}
	sch, err := r.getTaskScheduler(ctx, bs, pods)
	if err != nil {
		return false, err
	}

	stoppingTasks := sch.StopTask()
	if len(stoppingTasks) > 0 {
		log.Info("Stopping tasks before pause", "count", len(stoppingTasks))
	}

	if err := sch.Schedule(); err != nil {
		return false, fmt.Errorf("failed to stop tasks before pause: %w", err)
	}
	unfinishedTasks := r.getTasksCleanupUnfinished(bs, sch)
	if len(unfinishedTasks) > 0 {
		log.Info("Task cleanup before pause is unfinished", "unfinishedCount", len(unfinishedTasks))
		return false, nil
	}
	log.Info("Task cleanup before pause is finished")
	return true, nil
}

// handleResume implements the resume flow:
// 1. ACK (pauseObservedGeneration + phase=Resuming)
// 2. Requeue so a subsequent pass can consume the snapshot result
func (r *BatchSandboxReconciler) handleResume(ctx context.Context, bs *sandboxv1alpha1.BatchSandbox) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	_ = r.setCondition(ctx, bs, sandboxv1alpha1.BatchSandboxConditionResumeFailed, sandboxv1alpha1.ConditionFalse, "", "")

	log.Info("ACK Resuming phase")
	if err := r.ackPauseWithPhase(ctx, bs, sandboxv1alpha1.BatchSandboxPhaseResuming, ""); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: time.Second}, nil
}

// syncPauseOrClear waits for the internal pause snapshot to finish and transitions the
// BatchSandbox into Paused or a retryable/terminal failure state.
func (r *BatchSandboxReconciler) syncPauseOrClear(ctx context.Context, bs *sandboxv1alpha1.BatchSandbox) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	snapshot := &sandboxv1alpha1.SandboxSnapshot{}
	err := r.Get(ctx, types.NamespacedName{Namespace: bs.Namespace, Name: internalPauseSnapshotName(bs.Name)}, snapshot)
	if errors.IsNotFound(err) {
		snapshotName := internalPauseSnapshotName(bs.Name)
		if created, createErr := r.ensureInternalPauseSnapshot(ctx, bs, snapshotName); createErr != nil {
			return ctrl.Result{}, createErr
		} else if !created {
			log.Info("SandboxSnapshot not created yet; waiting for task cleanup", "snapshot", snapshotName)
		}
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	switch snapshot.Status.Phase {
	case sandboxv1alpha1.SandboxSnapshotPhaseSucceed:
		log.Info("SandboxSnapshot Succeed, completing pause")
		if err := r.completePause(ctx, bs); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Second}, nil
	case sandboxv1alpha1.SandboxSnapshotPhaseFailed:
		msg := snapshotFailureMessage(snapshot)
		log.Info("SandboxSnapshot Failed", "message", msg)

		phase := sandboxv1alpha1.BatchSandboxPhaseSucceed
		reason := "SnapshotFailed"
		if _, podErr := r.findPodForSandbox(ctx, bs); podErr != nil {
			phase = sandboxv1alpha1.BatchSandboxPhaseFailed
			reason = "PodNotFound"
		}

		if err := r.ackPauseWithPhase(ctx, bs, phase, ""); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.setCondition(ctx, bs, sandboxv1alpha1.BatchSandboxConditionPauseFailed, sandboxv1alpha1.ConditionTrue, reason, msg); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	case sandboxv1alpha1.SandboxSnapshotPhasePending, sandboxv1alpha1.SandboxSnapshotPhaseCommitting:
		log.Info("SandboxSnapshot in progress", "phase", snapshot.Status.Phase)
		return ctrl.Result{RequeueAfter: time.Second}, nil
	default:
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
}

// completePause finalizes the pause operation:
//  1. Normal mode: delete all Pods (cascade via OwnerRef)
//  2. Pool mode: solidify the source Pod template and clear poolRef. Pool Controller GC
//     then releases and deletes the old allocated pool Pod.
//  3. Set phase=Paused only after pod deletion/release is accepted.
//  4. spec.pause remains unchanged; the next external request (or server retry bridge)
//     is responsible for changing it.
func (r *BatchSandboxReconciler) completePause(ctx context.Context, bs *sandboxv1alpha1.BatchSandbox) error {
	log := logf.FromContext(ctx)
	poolStrategy := strategy.NewPoolStrategy(bs)
	wasPooled := poolStrategy.IsPooledMode()

	pods, err := r.listPods(ctx, poolStrategy, bs)
	if err != nil {
		return err
	}

	var pooledTemplate *corev1.PodTemplateSpec
	if wasPooled {
		if len(pods) == 0 {
			return fmt.Errorf("no allocated pods found for pooled BatchSandbox %s/%s", bs.Namespace, bs.Name)
		}
		pooledTemplate = sourcePodTemplateForPause(pods[0])
		if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
			latest := &sandboxv1alpha1.BatchSandbox{}
			if err := r.Get(ctx, types.NamespacedName{Namespace: bs.Namespace, Name: bs.Name}, latest); err != nil {
				return err
			}
			patch := client.MergeFrom(latest.DeepCopy())
			latest.Spec.Template = pooledTemplate.DeepCopy()
			latest.Spec.PoolRef = ""
			controllerutil.RemoveFinalizer(latest, FinalizerPoolAllocation)
			if latest.Annotations != nil {
				delete(latest.Annotations, AnnoAllocReleaseKey)
			}
			return r.Patch(ctx, latest, patch)
		}); err != nil {
			return err
		}
		bs.Spec.Template = pooledTemplate.DeepCopy()
		bs.Spec.PoolRef = ""
		controllerutil.RemoveFinalizer(bs, FinalizerPoolAllocation)
		if bs.Annotations != nil {
			delete(bs.Annotations, AnnoAllocReleaseKey)
		}
		log.Info("Detached pooled BatchSandbox after pause", "sourcePod", pods[0].Name)
	}

	controllerKey := controllerutils.GetControllerKey(bs)
	BatchSandboxScaleExpectations.DeleteExpectations(controllerKey)
	log.Info("Cleared scale expectations before pod deletion", "controllerKey", controllerKey)

	if !wasPooled {
		for _, pod := range pods {
			if err := r.Delete(ctx, pod); err != nil && !errors.IsNotFound(err) {
				log.Error(err, "Failed to delete pod during pause", "pod", pod.Name)
				return err
			}
			log.Info("Deleted pod during pause", "pod", pod.Name)
		}
	}

	r.deleteTaskScheduler(ctx, bs)

	var latest *sandboxv1alpha1.BatchSandbox
	if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest = &sandboxv1alpha1.BatchSandbox{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: bs.Namespace, Name: bs.Name}, latest); err != nil {
			return err
		}
		latest.Status.Phase = sandboxv1alpha1.BatchSandboxPhasePaused
		// Pool pause rewrites spec.template and clears spec.poolRef, which advances generation.
		// Acknowledge that controller-owned spec change only while pause is still requested,
		// so spec.pause=true is not replayed and a queued resume request is still observed.
		if latest.Spec.Pause != nil && *latest.Spec.Pause {
			latest.Status.PauseObservedGeneration = latest.Generation
		}
		applyBatchSandboxPhaseConditions(&latest.Status)
		return r.Status().Update(ctx, latest)
	}); err != nil {
		return err
	}
	r.StatusRVExpectation.Expect(latest)

	return nil
}

// continueResume continues the resume flow:
//  1. Read SandboxSnapshot status for image URIs
//  2. Replace template container images
//  3. Pool mode: clear poolRef
//  4. Leave spec.pause and spec.replicas untouched; normal reconciliation recreates
//     the single supported runtime replica from the rewritten template.
func (r *BatchSandboxReconciler) continueResume(ctx context.Context, bs *sandboxv1alpha1.BatchSandbox) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	snapshot := &sandboxv1alpha1.SandboxSnapshot{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: bs.Namespace, Name: internalPauseSnapshotName(bs.Name)}, snapshot); err != nil {
		if errors.IsNotFound(err) {
			readyPodExists, readyErr := r.hasReadyResumePod(ctx, bs)
			if readyErr != nil {
				return ctrl.Result{}, readyErr
			}
			if readyPodExists {
				log.Info("SandboxSnapshot missing for resume, but a ready pod already exists; treating resume as complete")
				_ = r.ackPauseWithPhase(ctx, bs, sandboxv1alpha1.BatchSandboxPhaseSucceed, "")
				_ = r.setCondition(ctx, bs, sandboxv1alpha1.BatchSandboxConditionResumeFailed, sandboxv1alpha1.ConditionFalse, "", "")
				_ = r.setCondition(ctx, bs, sandboxv1alpha1.BatchSandboxConditionPodFailed, sandboxv1alpha1.ConditionFalse, "", "")
				return ctrl.Result{}, nil
			}
			log.Info("SandboxSnapshot not found for resume, rolling back to Paused")
			_ = r.ackPauseWithPhase(ctx, bs, sandboxv1alpha1.BatchSandboxPhasePaused, "")
			_ = r.setCondition(ctx, bs, sandboxv1alpha1.BatchSandboxConditionResumeFailed, sandboxv1alpha1.ConditionTrue, "SnapshotNotFound", "SandboxSnapshot not found")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if snapshot.Status.Phase != sandboxv1alpha1.SandboxSnapshotPhaseSucceed {
		msg := fmt.Sprintf("snapshot not ready: phase=%s", snapshot.Status.Phase)
		log.Error(nil, msg)
		_ = r.ackPauseWithPhase(ctx, bs, sandboxv1alpha1.BatchSandboxPhasePaused, "")
		_ = r.setCondition(ctx, bs, sandboxv1alpha1.BatchSandboxConditionResumeFailed, sandboxv1alpha1.ConditionTrue, "SnapshotNotReady", msg)
		return ctrl.Result{}, nil
	}

	imageMap := make(map[string]string)
	for _, c := range snapshot.Status.Containers {
		imageMap[c.ContainerName] = c.ImageURI
	}

	var patched *sandboxv1alpha1.BatchSandbox
	if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &sandboxv1alpha1.BatchSandbox{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: bs.Namespace, Name: bs.Name}, latest); err != nil {
			return err
		}
		patch := client.MergeFrom(latest.DeepCopy())

		if latest.Spec.Template != nil {
			for i := range latest.Spec.Template.Spec.Containers {
				if img, ok := imageMap[latest.Spec.Template.Spec.Containers[i].Name]; ok {
					latest.Spec.Template.Spec.Containers[i].Image = img
				}
			}
			ensureImagePullSecret(latest.Spec.Template, r.ResumePullSecret)
		}

		if latest.Spec.PoolRef != "" {
			latest.Spec.PoolRef = ""
			controllerutil.RemoveFinalizer(latest, FinalizerPoolAllocation)
		}

		if err := r.Patch(ctx, latest, patch); err != nil {
			return err
		}
		patched = latest.DeepCopy()
		return nil
	}); err != nil {
		return ctrl.Result{}, err
	}

	if patched != nil {
		bs.ObjectMeta = patched.ObjectMeta
		bs.Spec = patched.Spec
	}

	return ctrl.Result{RequeueAfter: time.Second}, nil
}

func (r *BatchSandboxReconciler) ackPauseGeneration(ctx context.Context, bs *sandboxv1alpha1.BatchSandbox) error {
	var latest *sandboxv1alpha1.BatchSandbox
	if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest = &sandboxv1alpha1.BatchSandbox{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: bs.Namespace, Name: bs.Name}, latest); err != nil {
			return err
		}
		latest.Status.PauseObservedGeneration = latest.Generation
		applyBatchSandboxPhaseConditions(&latest.Status)
		return r.Status().Update(ctx, latest)
	}); err != nil {
		return err
	}
	r.StatusRVExpectation.Expect(latest)
	bs.Status.PauseObservedGeneration = bs.Generation
	applyBatchSandboxPhaseConditions(&bs.Status)
	return nil
}

func (r *BatchSandboxReconciler) ackPauseWithPhase(ctx context.Context, bs *sandboxv1alpha1.BatchSandbox, phase sandboxv1alpha1.BatchSandboxPhase, _ string) error {
	var latest *sandboxv1alpha1.BatchSandbox
	if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest = &sandboxv1alpha1.BatchSandbox{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: bs.Namespace, Name: bs.Name}, latest); err != nil {
			return err
		}
		latest.Status.PauseObservedGeneration = latest.Generation
		latest.Status.Phase = phase
		applyBatchSandboxPhaseConditions(&latest.Status)
		return r.Status().Update(ctx, latest)
	}); err != nil {
		return err
	}
	r.StatusRVExpectation.Expect(latest)
	bs.Status.PauseObservedGeneration = bs.Generation
	bs.Status.Phase = phase
	applyBatchSandboxPhaseConditions(&bs.Status)
	return nil
}

// findPodForSandbox finds the Pod associated with a BatchSandbox.
func (r *BatchSandboxReconciler) findPodForSandbox(ctx context.Context, bs *sandboxv1alpha1.BatchSandbox) (*corev1.Pod, error) {
	poolStrategy := strategy.NewPoolStrategy(bs)
	pods, err := r.listPods(ctx, poolStrategy, bs)
	if err != nil {
		return nil, err
	}
	if len(pods) == 0 {
		return nil, fmt.Errorf("no pods found for BatchSandbox %s/%s", bs.Namespace, bs.Name)
	}
	return pods[0], nil
}

func (r *BatchSandboxReconciler) setCondition(
	ctx context.Context,
	bs *sandboxv1alpha1.BatchSandbox,
	conditionType sandboxv1alpha1.BatchSandboxConditionType,
	status string,
	reason string,
	message string,
) error {
	var latest *sandboxv1alpha1.BatchSandbox
	if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest = &sandboxv1alpha1.BatchSandbox{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: bs.Namespace, Name: bs.Name}, latest); err != nil {
			return err
		}

		var conditions []sandboxv1alpha1.BatchSandboxCondition
		found := false
		for _, c := range latest.Status.Conditions {
			if c.Type == conditionType {
				if status == sandboxv1alpha1.ConditionFalse {
					continue
				}
				c.Status = status
				c.Reason = reason
				c.Message = message
				c.LastTransitionTime = ptr.To(metav1.Now())
				found = true
			}
			conditions = append(conditions, c)
		}

		if !found && status == sandboxv1alpha1.ConditionTrue {
			conditions = append(conditions, sandboxv1alpha1.BatchSandboxCondition{
				Type:               conditionType,
				Status:             status,
				Reason:             reason,
				Message:            message,
				LastTransitionTime: ptr.To(metav1.Now()),
			})
		}

		latest.Status.Conditions = conditions
		return r.Status().Update(ctx, latest)
	}); err != nil {
		return err
	}
	r.StatusRVExpectation.Expect(latest)
	return nil
}
