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
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	api "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
)

func TestWorkspacePauseLifecycle(t *testing.T) {
	require.NoError(t, batchv1.AddToScheme(testscheme))
	for _, fail := range []bool{false, true} {
		name := "success"
		if fail {
			name = "export failure"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			template := &corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name: "sandbox", Image: "python:3.12",
					VolumeMounts: []corev1.VolumeMount{{Name: "bucket", MountPath: storeMount}},
				}},
				Volumes: []corev1.Volume{{
					Name:         "bucket",
					VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "my-bucket"}},
				}},
			}}
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
			sr := &SandboxSnapshotReconciler{Client: cached, APIReader: r.APIReader, exportWorkspace: func(_ context.Context, src *corev1.Pod, container, archive string) (string, error) {
				require.Equal(t, pod.UID, src.UID)
				require.Equal(t, "sandbox", container)
				require.True(t, validArchiveName(archive))
				if fail {
					return "", errors.New("bucket full")
				}
				return digest, nil
			}}
			_, err = sr.handlePending(ctx, snap)
			require.NoError(t, err)
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
			changed := *snap.Status.Workspace
			changed.ClaimName = "other"
			require.ErrorContains(t, prepareWorkspaceRestore(tmpl, &changed), "PVC changed")
		})
	}
}
