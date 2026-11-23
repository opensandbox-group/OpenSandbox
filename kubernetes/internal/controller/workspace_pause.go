// Copyright 2026 The OpenSandbox Authors
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
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"path"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	kubeexec "k8s.io/client-go/util/exec"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	api "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
)

const (
	workspaceArchiveAnnotation = "sandbox.opensandbox.io/workspace-archive"
	workspaceClaimAnnotation   = "sandbox.opensandbox.io/workspace-claim"
	workspacePauseAnnotation   = "sandbox.opensandbox.io/workspace-pause"
	storeMount                 = "/mnt/opensandbox-checkpoints"
	workspaceMount             = "/workspace"
	restoreName                = "opensandbox-workspace-restore"
)

//go:embed workspace_archive.py
var workspaceScript string

// limitedOutput drains the exec stream while retaining only a short diagnostic.
type limitedOutput struct {
	data  []byte
	limit int
}

func (o *limitedOutput) Write(p []byte) (int, error) {
	n := min(len(p), o.limit-len(o.data))
	o.data = append(o.data, p[:n]...)
	return len(p), nil
}

type workspaceExport func(context.Context, *corev1.Pod, string, string) (string, error)

func newWorkspaceExport(config *rest.Config) (workspaceExport, error) {
	cs, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, pod *corev1.Pod, container, archive string) (string, error) {
		req := cs.CoreV1().RESTClient().Post().Resource("pods").Name(pod.Name).Namespace(pod.Namespace).SubResource("exec").
			VersionedParams(&corev1.PodExecOptions{Container: container, Command: []string{"python3", "-I", "-c", workspaceScript, "save", workspaceMount, path.Join(storeMount, archive)}, Stdout: true, Stderr: true}, scheme.ParameterCodec)
		executor, err := remotecommand.NewSPDYExecutor(config, "POST", req.URL())
		if err != nil {
			return "", err
		}
		out := limitedOutput{limit: 128}
		diagnostics := limitedOutput{limit: 1024}
		if err := executor.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &out, Stderr: &diagnostics}); err != nil {
			return "", fmt.Errorf("%w (stderr %q)", err, diagnostics.data)
		}
		if len(out.data) == out.limit {
			return "", fmt.Errorf("%w: oversized export response", errInvalidWorkspaceReport)
		}
		return string(out.data), nil
	}, nil
}

func validWorkspaceDigest(value string) bool {
	_, err := hex.DecodeString(value)
	return len(value) == 64 && err == nil
}

var (
	errRetryWorkspaceExport   = errors.New("retry workspace export")
	errInvalidWorkspaceReport = errors.New("invalid workspace export result")
)

// API and exec transport errors can clear on retry. A remote exit status
// means the helper actually ran and rejected the export.
func workspaceRequestError(err error) error {
	var exit kubeexec.ExitError
	if errors.As(err, &exit) || errors.Is(err, errInvalidWorkspaceReport) ||
		apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) || apierrors.IsBadRequest(err) || apierrors.IsInvalid(err) {
		return err
	}
	return fmt.Errorf("%w: %w", errRetryWorkspaceExport, err)
}

func workspaceTarget(spec *corev1.PodSpec, cfg api.WorkspacePauseSpec) (*corev1.Container, string, error) {
	var claim string
	for _, v := range spec.Volumes {
		if v.Name == cfg.VolumeName && v.PersistentVolumeClaim != nil && !v.PersistentVolumeClaim.ReadOnly {
			claim = v.PersistentVolumeClaim.ClaimName
		}
	}
	if claim == "" || cfg.VolumeName == restoreName {
		return nil, "", fmt.Errorf("workspace pause requires a dedicated writable checkpoint PVC")
	}
	for i := range spec.Containers {
		c := &spec.Containers[i]
		if c.Name != cfg.ContainerName {
			continue
		}
		mounted := false
		for _, m := range c.VolumeMounts {
			if m.Name == cfg.VolumeName && m.MountPath == storeMount && !m.ReadOnly && m.SubPath == "" && m.SubPathExpr == "" {
				mounted = true
			}
			if strings.HasPrefix(m.MountPath, workspaceMount+"/") || (m.MountPath == workspaceMount && (m.Name != restoreName || m.SubPath != "data")) {
				return nil, "", fmt.Errorf("workspace pause requires a plain /workspace without nested mounts")
			}
		}
		if mounted {
			return c, claim, nil
		}
	}
	return nil, "", fmt.Errorf("mount checkpoint volume %q at %s in container %q", cfg.VolumeName, storeMount, cfg.ContainerName)
}

func prepareWorkspaceRestore(template *corev1.PodTemplateSpec, w *api.WorkspaceCheckpoint) error {
	if w == nil || !validWorkspaceDigest(w.SHA256) {
		return fmt.Errorf("workspace checkpoint SHA-256 must contain 64 hexadecimal digits")
	}
	return configureWorkspaceRestore(template, w)
}

// Capture also checks the restore configuration on a template copy before the archive has a digest.
func configureWorkspaceRestore(template *corev1.PodTemplateSpec, w *api.WorkspaceCheckpoint) error {
	if template == nil || !validArchiveName(w.Archive) {
		return fmt.Errorf("invalid workspace restore metadata")
	}
	app, claim, err := workspaceTarget(&template.Spec, w.WorkspacePauseSpec)
	if err != nil {
		return err
	}
	if claim != w.ClaimName {
		return fmt.Errorf("checkpoint PVC changed since pause")
	}
	if template.Annotations == nil {
		template.Annotations = make(map[string]string)
	}
	template.Annotations[workspaceArchiveAnnotation] = w.Archive
	template.Annotations[workspaceClaimAnnotation] = claim
	var volume *corev1.EmptyDirVolumeSource
	for i := range template.Spec.Volumes {
		v := &template.Spec.Volumes[i]
		if v.Name == restoreName {
			if v.EmptyDir == nil {
				return fmt.Errorf("reserved workspace restore volume already exists")
			}
			volume = v.EmptyDir
		}
	}
	init := corev1.Container{
		Name:                     restoreName,
		Image:                    app.Image,
		ImagePullPolicy:          app.ImagePullPolicy,
		SecurityContext:          app.SecurityContext.DeepCopy(),
		Resources:                *app.Resources.DeepCopy(),
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
		Command:                  []string{"python3", "-I", "-c", workspaceScript, "restore", workspaceMount, path.Join(storeMount, w.Archive), strings.ToLower(w.SHA256)},
		VolumeMounts:             []corev1.VolumeMount{{Name: w.VolumeName, MountPath: storeMount, ReadOnly: true}, {Name: restoreName, MountPath: workspaceMount}},
	}
	hasVolume := volume != nil
	if volume == nil {
		volume = &corev1.EmptyDirVolumeSource{}
	}
	if err := sizeWorkspaceRestore(volume, &init.Resources, w.SizeBytes); err != nil {
		return err
	}
	hasMount := false
	for _, m := range app.VolumeMounts {
		if m.Name == restoreName && m.MountPath == workspaceMount && m.SubPath == "data" {
			hasMount = true
		}
	}
	for i, c := range template.Spec.InitContainers {
		if c.Name != restoreName {
			continue
		}
		if !hasVolume || !hasMount || len(c.Command) != len(init.Command) || c.Command[4] != "restore" || c.Command[5] != workspaceMount {
			return fmt.Errorf("reserved workspace restore init container already exists")
		}
		template.Spec.InitContainers[i] = init
		return nil
	}
	if hasVolume || hasMount {
		return fmt.Errorf("reserved workspace restore volume already exists")
	}
	for _, c := range template.Spec.Containers {
		if c.Name == restoreName {
			return fmt.Errorf("reserved workspace restore container already exists")
		}
	}
	template.Spec.Volumes = append(template.Spec.Volumes, corev1.Volume{Name: restoreName, VolumeSource: corev1.VolumeSource{EmptyDir: volume}})
	app.VolumeMounts = append(app.VolumeMounts, corev1.VolumeMount{Name: restoreName, MountPath: workspaceMount, SubPath: "data"})
	template.Spec.InitContainers = append(template.Spec.InitContainers, init)
	return nil
}

func sizeWorkspaceRestore(volume *corev1.EmptyDirVolumeSource, resources *corev1.ResourceRequirements, size int64) error {
	if size == 0 {
		return nil // Pre-size checkpoints and pre-export template validation.
	}
	if size < 0 {
		return fmt.Errorf("invalid workspace archive size %d", size)
	}
	headroom := max(size/4, 64<<20)
	if size > math.MaxInt64-headroom {
		return fmt.Errorf("workspace archive size exceeds representable restore capacity")
	}
	archive := resource.NewQuantity(size, resource.BinarySI)
	budget := *resource.NewQuantity(size+headroom, resource.BinarySI)
	if requested := resources.Requests.StorageEphemeral(); requested.Cmp(budget) > 0 {
		budget = requested.DeepCopy()
	}
	volumeBudget := budget.DeepCopy()
	if limit, ok := resources.Limits[corev1.ResourceEphemeralStorage]; ok && !limit.IsZero() {
		if limit.Cmp(*archive) < 0 {
			return fmt.Errorf("workspace archive %s exceeds ephemeral-storage limit %s", archive, &limit)
		}
		volumeBudget = limit.DeepCopy()
		if budget.Cmp(limit) > 0 {
			budget = limit.DeepCopy()
		}
	}
	volume.SizeLimit = &volumeBudget
	if resources.Requests == nil {
		resources.Requests = corev1.ResourceList{}
	}
	resources.Requests[corev1.ResourceEphemeralStorage] = budget
	return nil
}

func validArchiveName(name string) bool {
	// Pre-prefix checkpoints had a single flat key.
	if prefix, leaf, found := strings.Cut(name, "/"); found {
		if len(validation.IsDNS1123Label(prefix)) != 0 {
			return false
		}
		name = leaf
	}
	return len(name) == 40 && strings.HasSuffix(name, ".tar") && strings.Trim(name[:36], "0123456789abcdef-") == ""
}

// The injected mount also identifies restored workspaces created before checkpoint annotations existed.
func hasRestoredWorkspace(spec *corev1.PodSpec) bool {
	for _, container := range spec.Containers {
		for _, mount := range container.VolumeMounts {
			if mount.Name == restoreName && mount.MountPath == workspaceMount && mount.SubPath == "data" {
				return true
			}
		}
	}
	return false
}

func workspaceModeChanged(bs *api.BatchSandbox, snapshot *api.SandboxSnapshot) bool {
	workspace := bs.Spec.WorkspacePause != nil
	return (!workspace && bs.Spec.Template != nil && hasRestoredWorkspace(&bs.Spec.Template.Spec)) ||
		(snapshot != nil && workspace != (snapshot.Annotations[workspacePauseAnnotation] == "true"))
}

const workspaceModeChangeMessage = "pause strategy changed; restore the prior workspacePause configuration or migrate the workspace to a new sandbox"

func workspaceExportPending(s *api.SandboxSnapshot) bool {
	return s.Annotations[workspacePauseAnnotation] == "true" && s.DeletionTimestamp.IsZero() &&
		controllerutil.ContainsFinalizer(s, sandboxSnapshotFinalizer) &&
		s.Status.Phase == api.SandboxSnapshotPhasePending && s.Status.ObservedGeneration == s.Generation
}

// Pending snapshots are replayed by the watch when a new leader starts.
func (r *SandboxSnapshotReconciler) reconcileWorkspaceExport(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	s := &api.SandboxSnapshot{}
	if err := r.APIReader.Get(ctx, req.NamespacedName, s); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !workspaceExportPending(s) {
		return ctrl.Result{}, nil
	}
	parent := &api.BatchSandbox{}
	err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: s.Spec.SandboxName}, parent)
	if err != nil && !apierrors.IsNotFound(err) {
		err = workspaceRequestError(err)
	}
	reason := "BatchSandboxLookupFailed"
	if err == nil {
		if metav1.IsControlledBy(s, parent) && parent.Status.Phase == api.BatchSandboxPhasePausing && parent.Spec.WorkspacePause == nil {
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		reason = "WorkspaceExportFailed"
		err = r.captureWorkspace(ctx, s, parent)
	}
	if ctx.Err() != nil {
		return ctrl.Result{}, ctx.Err()
	}
	if err != nil {
		if errors.Is(err, errRetryWorkspaceExport) && (s.CreationTimestamp.IsZero() || time.Since(s.CreationTimestamp.Time) < r.getCommitJobTimeout()) {
			return ctrl.Result{}, err
		}
		s.Status.Phase = api.SandboxSnapshotPhaseFailed
		applySnapshotPhaseConditions(&s.Status, reason, err.Error())
	}
	// Retry publishing the verified result without rerunning the export. Recheck
	// identity and ownership in case a pause was deleted and recreated in flight.
	return ctrl.Result{}, retry.OnError(retry.DefaultBackoff, func(err error) bool {
		return ctx.Err() == nil && errors.Is(workspaceRequestError(err), errRetryWorkspaceExport)
	}, func() error {
		latest := &api.SandboxSnapshot{}
		if err := r.APIReader.Get(ctx, req.NamespacedName, latest); err != nil {
			return client.IgnoreNotFound(err)
		}
		if latest.UID != s.UID || latest.Generation != s.Generation || !workspaceExportPending(latest) {
			return nil
		}
		if !equality.Semantic.DeepEqual(latest.OwnerReferences, s.OwnerReferences) {
			return nil
		}
		latest.Status = s.Status
		return r.Status().Update(ctx, latest)
	})
}

func (r *SandboxSnapshotReconciler) captureWorkspace(ctx context.Context, s *api.SandboxSnapshot, bs *api.BatchSandbox) error {
	if bs.Spec.WorkspacePause == nil || bs.Spec.Replicas == nil || *bs.Spec.Replicas != 1 || bs.UID == "" || !metav1.IsControlledBy(s, bs) || s.Name != internalPauseSnapshotName(bs.Name) || bs.Spec.Pause == nil || !*bs.Spec.Pause || bs.Status.Phase != api.BatchSandboxPhasePausing || bs.Spec.PoolRef != "" || bs.Spec.Template == nil {
		return fmt.Errorf("workspace capture requires an active pause on a dedicated BatchSandbox Pod")
	}
	// Read the controller-owned Pod directly, independent of user-supplied allocation annotations.
	pod := &corev1.Pod{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: bs.Namespace, Name: fmt.Sprintf("%s-%d", bs.Name, batchSandboxFirstPodIndex)}, pod); err != nil {
		return workspaceRequestError(err)
	}
	if !metav1.IsControlledBy(pod, bs) || pod.DeletionTimestamp != nil || pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
		return fmt.Errorf("source workspace Pod is unavailable")
	}
	if pod.Status.Phase != corev1.PodRunning {
		return fmt.Errorf("%w: source Pod %s is waiting to run", errRetryWorkspaceExport, pod.Name)
	}
	cfg := *bs.Spec.WorkspacePause
	_, claim, err := workspaceTarget(&pod.Spec, cfg)
	if err != nil {
		return err
	}
	w := &api.WorkspaceCheckpoint{WorkspacePauseSpec: cfg, ClaimName: claim, Archive: path.Join(string(bs.UID), string(uuid.NewUUID())+".tar"), SourcePodUID: pod.UID}
	if err := configureWorkspaceRestore(bs.Spec.Template.DeepCopy(), w); err != nil {
		return err
	}
	exportCtx, cancel := context.WithTimeout(ctx, r.getCommitJobTimeout())
	defer cancel()
	report, err := r.exportWorkspace(exportCtx, pod, cfg.ContainerName, w.Archive)
	if err != nil {
		return fmt.Errorf("workspace export: %w", workspaceRequestError(err))
	}
	fields := strings.Fields(report)
	if len(fields) != 2 || !validWorkspaceDigest(fields[0]) {
		return errInvalidWorkspaceReport
	}
	w.SizeBytes, err = strconv.ParseInt(fields[1], 10, 64)
	if err != nil || w.SizeBytes <= 0 {
		return fmt.Errorf("%w: invalid archive size", errInvalidWorkspaceReport)
	}
	w.SHA256 = strings.ToLower(fields[0])
	s.Status.SourcePodName = pod.Name
	s.Status.Workspace = w
	s.Status.Format = api.SandboxSnapshotFormatWorkspaceV1
	s.Status.Phase = api.SandboxSnapshotPhaseSucceed
	applySnapshotPhaseConditions(&s.Status, "", "")
	return nil
}
