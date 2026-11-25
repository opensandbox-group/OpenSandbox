// Copyright 2025 The OpenSandbox Authors
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
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeexec "k8s.io/client-go/util/exec"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	api "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
)

func workspacePauseTestTemplate() *corev1.PodTemplateSpec {
	return &corev1.PodTemplateSpec{Spec: corev1.PodSpec{
		Containers: []corev1.Container{{
			Name: "sandbox", Image: "python:3.12",
			VolumeMounts: []corev1.VolumeMount{{Name: "bucket", MountPath: storeMount}},
		}},
		Volumes: []corev1.Volume{{
			Name:         "bucket",
			VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "my-bucket"}},
		}},
	}}
}

func TestWorkspacePauseLifecycle(t *testing.T) {
	require.NoError(t, batchv1.AddToScheme(testscheme))
	for _, fail := range []bool{false, true} {
		name := "success"
		if fail {
			name = "export failure"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			template := workspacePauseTestTemplate()
			bs := &api.BatchSandbox{
				ObjectMeta: metav1.ObjectMeta{Name: "workspace", Namespace: "default", UID: "sandbox-uid", Generation: 2},
				Spec: api.BatchSandboxSpec{
					Replicas: ptr.To(int32(1)), Pause: ptr.To(true), Template: template,
					WorkspacePause: &api.WorkspacePauseSpec{ContainerName: "sandbox", VolumeName: "bucket"},
				},
			}
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: bs.Name + "-0", Namespace: bs.Namespace, UID: "source-pod",
					OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(bs, api.GroupVersion.WithKind("BatchSandbox"))},
				},
				Spec: *template.Spec.DeepCopy(), Status: corev1.PodStatus{Phase: corev1.PodRunning},
			}
			r := newTestReconciler(bs, pod)
			_, err := r.handlePause(ctx, bs)
			require.NoError(t, err)
			snap := &api.SandboxSnapshot{}
			key := client.ObjectKey{Namespace: bs.Namespace, Name: internalPauseSnapshotName(bs.Name)}
			require.NoError(t, r.Get(ctx, key, snap))
			digest := strings.Repeat("3", 64)
			// The snapshot informer sees the new snapshot before the parent informer sees Pausing.
			cached := interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
				Get: func(ctx context.Context, live client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					err := live.Get(ctx, key, obj, opts...)
					if parent, ok := obj.(*api.BatchSandbox); ok && err == nil {
						parent.Status.Phase = api.BatchSandboxPhaseSucceed
					}
					return err
				},
			})
			started := make(chan struct{})
			release := make(chan struct{})
			sr := &SandboxSnapshotReconciler{Client: cached, APIReader: r.APIReader, exportWorkspace: func(exportCtx context.Context, src *corev1.Pod, container, archive string) (string, error) {
				close(started)
				select {
				case <-release:
				case <-exportCtx.Done():
					return "", exportCtx.Err()
				}
				if src.UID != pod.UID || container != "sandbox" || !validArchiveName(archive) || !strings.HasPrefix(archive, string(bs.UID)+"/") {
					return "", errors.New("wrong workspace export target")
				}
				if fail {
					return "", kubeexec.CodeExitError{Err: errors.New("bucket full"), Code: 1}
				}
				return digest + " 10240", nil
			}}
			snap.Generation = 1
			require.NoError(t, r.Update(ctx, snap))
			req := ctrl.Request{NamespacedName: key}
			// The main queue installs the finalizer and acknowledges the generation.
			for range 2 {
				_, err = sr.Reconcile(ctx, req)
				require.NoError(t, err)
			}
			done := make(chan error, 1)
			go func() {
				_, exportErr := sr.reconcileWorkspaceExport(ctx, req)
				done <- exportErr
			}()
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("export did not start")
			}
			// An unrelated rootfs snapshot must reach a terminal state while export is blocked.
			other := &api.SandboxSnapshot{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: bs.Namespace, Finalizers: []string{sandboxSnapshotFinalizer}}}
			require.NoError(t, r.Create(ctx, other))
			_, err = sr.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(other)})
			require.NoError(t, err)
			require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(other), other))
			require.Equal(t, api.SandboxSnapshotPhaseFailed, other.Status.Phase)
			close(release)
			require.NoError(t, <-done)
			require.NoError(t, r.Get(ctx, key, snap))
			_, err = r.syncPauseOrClear(ctx, bs)
			require.NoError(t, err)
			var jobs batchv1.JobList
			require.NoError(t, r.List(ctx, &jobs))
			require.Empty(t, jobs.Items)
			require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(bs), bs))
			podErr := r.Get(ctx, client.ObjectKeyFromObject(pod), pod)
			if fail {
				require.NoError(t, podErr)
				require.Equal(t, api.SandboxSnapshotPhaseFailed, snap.Status.Phase)
				require.Contains(t, snap.Status.Conditions[0].Message, "bucket full")
				require.Equal(t, api.BatchSandboxPhaseSucceed, bs.Status.Phase)
				return
			}
			require.True(t, apierrors.IsNotFound(podErr))
			require.Equal(t, api.BatchSandboxPhasePaused, bs.Status.Phase)
			require.Equal(t, digest, snap.Status.Workspace.SHA256)
			require.EqualValues(t, 10240, snap.Status.Workspace.SizeBytes)
			bs.Spec.Pause = ptr.To(false)
			require.NoError(t, r.Update(ctx, bs))
			_, err = r.handleResume(ctx, bs)
			require.NoError(t, err)
			for range 2 {
				_, err = r.continueResume(ctx, bs)
				require.NoError(t, err)
			}
			tmpl := bs.Spec.Template
			require.Equal(t, "python:3.12", tmpl.Spec.Containers[0].Image)
			require.Len(t, tmpl.Spec.InitContainers, 1)
			require.Len(t, tmpl.Spec.Volumes, 2)
			init := tmpl.Spec.InitContainers[0]
			require.Equal(t, digest, init.Command[len(init.Command)-1])
			require.True(t, init.VolumeMounts[0].ReadOnly)
			require.Equal(t, int64(64<<20)+10240, init.Resources.Requests.StorageEphemeral().Value())
			require.Equal(t, init.Resources.Requests.StorageEphemeral().Value(), tmpl.Spec.Volumes[1].EmptyDir.SizeLimit.Value())
			require.Equal(t, "data", tmpl.Spec.Containers[0].VolumeMounts[1].SubPath)
			require.Equal(t, snap.Status.Workspace.Archive, tmpl.Annotations[workspaceArchiveAnnotation])
			require.Equal(t, "my-bucket", tmpl.Annotations[workspaceClaimAnnotation])
			changed := *snap.Status.Workspace
			changed.ClaimName = "other"
			require.ErrorContains(t, prepareWorkspaceRestore(tmpl, &changed), "PVC changed")
			// Removing the strategy must fail before creating a rootfs snapshot or releasing the live Pod.
			bs.Spec.WorkspacePause = nil
			bs.Spec.Pause = ptr.To(true)
			require.NoError(t, r.Update(ctx, bs))
			livePod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace, OwnerReferences: pod.OwnerReferences}, Spec: tmpl.Spec}
			require.NoError(t, r.Create(ctx, livePod))
			_, err = r.handlePause(ctx, bs)
			require.NoError(t, err)
			require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(livePod), livePod))
			require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(bs), bs))
			require.Contains(t, fmt.Sprint(bs.Status.Conditions), "WorkspaceConfigChanged")
		})
	}
}

func TestContinueResumeWorkspaceDigest(t *testing.T) {
	const digest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	for _, tc := range []struct {
		name, stored string
		valid        bool
	}{
		{"lowercase", digest, true},
		{"uppercase", strings.ToUpper(digest), true},
		{"mixed case", "0123456789ABCDEF" + digest[16:], true},
		{"missing", "", false},
		{"too short", digest[:62], false},
		{"too long", digest + "00", false},
		{"non-hex", digest[:63] + "z", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := api.WorkspacePauseSpec{ContainerName: "sandbox", VolumeName: "bucket"}
			bs := &api.BatchSandbox{
				ObjectMeta: metav1.ObjectMeta{Name: "sandbox", Namespace: "default"},
				Spec:       api.BatchSandboxSpec{Pause: ptr.To(false), WorkspacePause: &cfg, Template: workspacePauseTestTemplate()},
				Status:     api.BatchSandboxStatus{Phase: api.BatchSandboxPhaseResuming},
			}
			snapshot := &api.SandboxSnapshot{
				ObjectMeta: metav1.ObjectMeta{Name: internalPauseSnapshotName(bs.Name), Namespace: bs.Namespace, Annotations: map[string]string{workspacePauseAnnotation: "true"}},
				Status: api.SandboxSnapshotStatus{
					Phase: api.SandboxSnapshotPhaseSucceed, Format: api.SandboxSnapshotFormatWorkspaceV1,
					Workspace: &api.WorkspaceCheckpoint{WorkspacePauseSpec: cfg, ClaimName: "my-bucket", Archive: "da09f148-fb28-4338-8e6f-9b5ec908ae78.tar", SHA256: tc.stored},
				},
			}
			template := bs.Spec.Template.DeepCopy()
			r := newTestReconciler(bs, snapshot)
			ctx := t.Context()
			_, err := r.continueResume(ctx, bs)
			require.NoError(t, err)
			require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(bs), bs))
			if tc.valid {
				require.Len(t, bs.Spec.Template.Spec.InitContainers, 1)
				command := bs.Spec.Template.Spec.InitContainers[0].Command
				require.Equal(t, digest, command[len(command)-1])
				return
			}
			require.Equal(t, template, bs.Spec.Template)
			require.Equal(t, api.BatchSandboxPhasePaused, bs.Status.Phase)
			index := slices.IndexFunc(bs.Status.Conditions, func(c api.BatchSandboxCondition) bool {
				return c.Type == api.BatchSandboxConditionResumeFailed
			})
			require.NotEqual(t, -1, index)
			condition := bs.Status.Conditions[index]
			require.Equal(t, api.ConditionTrue, condition.Status)
			require.Equal(t, "InvalidWorkspaceRestore", condition.Reason)
			require.Contains(t, condition.Message, "SHA-256")
		})
	}
}

func TestWorkspaceModeEnabledDuringRootfsPause(t *testing.T) {
	ctx := context.Background()
	bs := &api.BatchSandbox{ObjectMeta: metav1.ObjectMeta{Name: "sandbox", Namespace: "default", UID: "sandbox-uid"},
		Spec:   api.BatchSandboxSpec{WorkspacePause: &api.WorkspacePauseSpec{}},
		Status: api.BatchSandboxStatus{Phase: api.BatchSandboxPhasePausing}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-0", Namespace: bs.Namespace,
		OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(bs, api.GroupVersion.WithKind("BatchSandbox"))}}}
	snapshot := &api.SandboxSnapshot{ObjectMeta: metav1.ObjectMeta{Name: internalPauseSnapshotName(bs.Name), Namespace: bs.Namespace},
		Status: api.SandboxSnapshotStatus{Phase: api.SandboxSnapshotPhaseSucceed}}
	r := newTestReconciler(bs, pod, snapshot)
	_, err := r.syncPauseOrClear(ctx, bs)
	require.NoError(t, err)
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(pod), pod))
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(bs), bs))
	require.Contains(t, fmt.Sprint(bs.Status.Conditions), "WorkspaceConfigChanged")
	require.Equal(t, api.BatchSandboxPhasePausing, bs.Status.Phase)
	bs.Spec.WorkspacePause = nil
	require.NoError(t, r.Update(ctx, bs))
	_, err = r.syncPauseOrClear(ctx, bs)
	require.NoError(t, err)
	require.True(t, apierrors.IsNotFound(r.Get(ctx, client.ObjectKeyFromObject(pod), pod)))
}

func TestWorkspaceArchiveKeys(t *testing.T) {
	const legacy = "da09f148-fb28-4338-8e6f-9b5ec908ae78.tar"
	require.True(t, validArchiveName(legacy))
	require.True(t, validArchiveName("sandbox-uid/"+legacy))
	for _, prefix := range []string{"../", "/absolute/", "nested/deeper/"} {
		require.False(t, validArchiveName(prefix+legacy))
	}
}

func TestWorkspaceRestoreStorage(t *testing.T) {
	tmpl := workspacePauseTestTemplate()
	app := &tmpl.Spec.Containers[0]
	app.Resources.Requests = corev1.ResourceList{corev1.ResourceEphemeralStorage: resource.MustParse("4Gi")}
	app.Resources.Limits = corev1.ResourceList{corev1.ResourceEphemeralStorage: resource.MustParse("8Gi")}
	w := &api.WorkspaceCheckpoint{WorkspacePauseSpec: api.WorkspacePauseSpec{ContainerName: "sandbox", VolumeName: "bucket"},
		Archive: "da09f148-fb28-4338-8e6f-9b5ec908ae78.tar", SHA256: strings.Repeat("a", 64), ClaimName: "my-bucket"}
	require.NoError(t, prepareWorkspaceRestore(tmpl, w))
	require.Nil(t, tmpl.Spec.Volumes[1].EmptyDir.SizeLimit, "legacy checkpoints keep their storage contract")
	for _, tc := range []struct{ size, wantRequest int64 }{{1 << 30, 4 << 30}, {6 << 30, 7680 << 20}, {8 << 30, 8 << 30}} {
		w.SizeBytes = tc.size
		require.NoError(t, prepareWorkspaceRestore(tmpl, w))
		require.EqualValues(t, 8<<30, tmpl.Spec.Volumes[1].EmptyDir.SizeLimit.Value())
		require.Equal(t, tc.wantRequest, tmpl.Spec.InitContainers[0].Resources.Requests.StorageEphemeral().Value())
		require.EqualValues(t, 4<<30, app.Resources.Requests.StorageEphemeral().Value())
		require.Len(t, tmpl.Spec.InitContainers, 1)
	}
	w.SizeBytes++
	require.ErrorContains(t, prepareWorkspaceRestore(tmpl, w), "exceeds ephemeral-storage limit")
	w.SizeBytes = math.MaxInt64
	require.ErrorContains(t, prepareWorkspaceRestore(tmpl, w), "representable")
}

func workspacePauseObjects() (*api.BatchSandbox, *corev1.Pod, *api.SandboxSnapshot) {
	cfg := api.WorkspacePauseSpec{ContainerName: "sandbox", VolumeName: "bucket"}
	bs := &api.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sandbox", Namespace: "default", UID: "sandbox-uid"},
		Spec:       api.BatchSandboxSpec{Pause: ptr.To(true), Replicas: ptr.To(int32(1)), WorkspacePause: &cfg, Template: workspacePauseTestTemplate()},
		Status:     api.BatchSandboxStatus{Phase: api.BatchSandboxPhasePausing},
	}
	owner := []metav1.OwnerReference{*metav1.NewControllerRef(bs, api.GroupVersion.WithKind("BatchSandbox"))}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: bs.Name + "-0", Namespace: bs.Namespace, UID: "pod-uid", OwnerReferences: owner},
		Spec: *bs.Spec.Template.Spec.DeepCopy(), Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	s := &api.SandboxSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: internalPauseSnapshotName(bs.Name), Namespace: bs.Namespace, UID: "snapshot-uid", OwnerReferences: owner,
			CreationTimestamp: metav1.Now(), Finalizers: []string{sandboxSnapshotFinalizer}, Annotations: map[string]string{workspacePauseAnnotation: "true"}},
		Spec: api.SandboxSnapshotSpec{SandboxName: bs.Name}, Status: api.SandboxSnapshotStatus{Phase: api.SandboxSnapshotPhasePending},
	}
	return bs, pod, s
}

func TestWorkspaceExportRetries(t *testing.T) {
	bs, pod, snapshot := workspacePauseObjects()
	pod.Status.Phase = corev1.PodPending
	r := newTestReconciler(bs, pod, snapshot)
	writes, exports := 0, 0
	c := interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, name string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			writes++
			if writes == 1 {
				return apierrors.NewServiceUnavailable("apiserver restarting")
			}
			return c.SubResource(name).Update(ctx, obj, opts...)
		},
	})
	unavailable := true
	reader := interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*corev1.Pod); ok && unavailable {
				unavailable = false
				return apierrors.NewServiceUnavailable("source read unavailable")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	sr := &SandboxSnapshotReconciler{Client: c, APIReader: reader, exportWorkspace: func(context.Context, *corev1.Pod, string, string) (string, error) {
		exports++
		if exports == 1 {
			return "", io.ErrUnexpectedEOF
		}
		return strings.Repeat("a", 64) + " 10240", nil
	}}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(snapshot)}
	_, err := sr.reconcileWorkspaceExport(t.Context(), req)
	require.True(t, apierrors.IsServiceUnavailable(err))
	_, err = sr.reconcileWorkspaceExport(t.Context(), req)
	require.ErrorIs(t, err, errRetryWorkspaceExport)
	require.Zero(t, exports)
	pod.Status.Phase = corev1.PodRunning
	require.NoError(t, r.Status().Update(t.Context(), pod))
	_, err = sr.reconcileWorkspaceExport(t.Context(), req)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	require.NoError(t, r.Get(t.Context(), req.NamespacedName, snapshot))
	require.Equal(t, api.SandboxSnapshotPhasePending, snapshot.Status.Phase)
	_, err = sr.reconcileWorkspaceExport(t.Context(), req)
	require.NoError(t, err)
	require.NoError(t, r.Get(t.Context(), req.NamespacedName, snapshot))
	require.Equal(t, api.SandboxSnapshotPhaseSucceed, snapshot.Status.Phase)
	require.Equal(t, 2, exports)
	require.Equal(t, 2, writes)
}

func TestWorkspaceExportRetryDeadline(t *testing.T) {
	bs, pod, s := workspacePauseObjects()
	s.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Hour))
	pod.Status.Phase = corev1.PodPending
	r := newTestReconciler(bs, pod, s)
	sr := &SandboxSnapshotReconciler{Client: r.Client, APIReader: r.APIReader}
	key := client.ObjectKeyFromObject(s)
	_, err := sr.reconcileWorkspaceExport(t.Context(), ctrl.Request{NamespacedName: key})
	require.NoError(t, err)
	require.NoError(t, r.Get(t.Context(), key, s))
	require.Equal(t, api.SandboxSnapshotPhaseFailed, s.Status.Phase)
	require.Contains(t, s.Status.Conditions[0].Message, "waiting to run")
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(pod), pod))
}

func TestWorkspacePausePreservesSourceOnFailure(t *testing.T) {
	for _, reason := range []string{"SourcePodChanged", "InvalidWorkspaceCheckpoint", "WorkspaceConfigChanged", "InvalidWorkspaceRestore"} {
		t.Run(reason, func(t *testing.T) {
			bs, pod, s := workspacePauseObjects()
			s.Status.Phase, s.Status.Format = api.SandboxSnapshotPhaseSucceed, api.SandboxSnapshotFormatWorkspaceV1
			w := &api.WorkspaceCheckpoint{WorkspacePauseSpec: *bs.Spec.WorkspacePause, ClaimName: "my-bucket", SourcePodUID: pod.UID,
				Archive: "da09f148-fb28-4338-8e6f-9b5ec908ae78.tar", SHA256: strings.Repeat("c", 64), SizeBytes: 2 << 30}
			s.Status.Workspace = w
			switch reason {
			case "SourcePodChanged":
				w.SourcePodUID = "previous-pod"
			case "InvalidWorkspaceCheckpoint":
				w.SHA256 = ""
			case "WorkspaceConfigChanged":
				w.ContainerName = "previous-container"
			case "InvalidWorkspaceRestore":
				bs.Spec.Template.Spec.Containers[0].Resources.Limits = corev1.ResourceList{corev1.ResourceEphemeralStorage: resource.MustParse("1Gi")}
			}
			r := newTestReconciler(bs, pod, s)
			_, err := r.syncPauseOrClear(t.Context(), bs)
			require.NoError(t, err)
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(pod), pod))
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(bs), bs))
			require.Equal(t, api.BatchSandboxPhaseSucceed, bs.Status.Phase)
			require.True(t, slices.ContainsFunc(bs.Status.Conditions, func(c api.BatchSandboxCondition) bool {
				return c.Type == api.BatchSandboxConditionPauseFailed && c.Status == api.ConditionTrue && c.Reason == reason
			}))
		})
	}
}

func TestWorkspaceSnapshotDeletionOnlyRemovesItsFinalizer(t *testing.T) {
	_, _, s := workspacePauseObjects()
	s.DeletionTimestamp = ptr.To(metav1.Now())
	s.Finalizers = append(s.Finalizers, "example.org/other")
	sr := &SandboxSnapshotReconciler{}
	require.NoError(t, batchv1.AddToScheme(testscheme))
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: sr.getJobName(s), Namespace: s.Namespace}}
	claim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "my-bucket", Namespace: s.Namespace}}
	r := newTestReconciler(s, job, claim)
	sr.Client = r.Client
	_, err := sr.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(s)})
	require.NoError(t, err)
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(s), s))
	require.Equal(t, []string{"example.org/other"}, s.Finalizers)
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(job), job))
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(claim), claim))
}

func TestWorkspaceRestoreFailureIsVisibleAndRecovers(t *testing.T) {
	bs, pod, _ := workspacePauseObjects()
	bs.Status.Phase = api.BatchSandboxPhaseResuming
	pod.Spec.Containers[0].VolumeMounts = append(pod.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{Name: restoreName, MountPath: workspaceMount, SubPath: "data"})
	pod.Status.Phase = corev1.PodPending
	exit := &corev1.ContainerStateTerminated{ExitCode: 1, Message: "workspace digest mismatch"}
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: restoreName,
		State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
		LastTerminationState: corev1.ContainerState{Terminated: exit}}}
	view := buildRuntimeView(bs, []*corev1.Pod{pod})
	require.Equal(t, api.BatchSandboxPhaseResuming, view.status.Phase)
	index := slices.IndexFunc(view.status.Conditions, func(c api.BatchSandboxCondition) bool { return c.Type == api.BatchSandboxConditionProgressing })
	require.NotEqual(t, -1, index)
	require.Equal(t, "WorkspaceRestoreRetrying", view.status.Conditions[index].Reason)
	require.Contains(t, view.status.Conditions[index].Message, exit.Message)
	bs.Status = *view.status
	require.Equal(t, bs.Status.Conditions, buildRuntimeView(bs, []*corev1.Pod{pod}).status.Conditions)
	pod.Status.InitContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}
	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	view = buildRuntimeView(bs, []*corev1.Pod{pod})
	require.True(t, view.resumeCompleted)
	require.False(t, slices.ContainsFunc(view.status.Conditions, func(c api.BatchSandboxCondition) bool { return c.Reason == "WorkspaceRestoreRetrying" }))
}
