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
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
					return "", errors.New("bucket full")
				}
				return digest, nil
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
