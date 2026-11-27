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
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
)

func TestSetReleasedPreservesConcurrentCleanup(t *testing.T) {
	environment := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory: getFirstFoundEnvTestBinaryDir(),
	}
	config, err := environment.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, environment.Stop()) })
	apiClient, err := client.NewWithWatch(config, client.Options{Scheme: testscheme})
	require.NoError(t, err)
	ctx := context.Background()
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "release-conflict-"}}
	require.NoError(t, apiClient.Create(ctx, namespace))
	t.Cleanup(func() { require.NoError(t, apiClient.Delete(ctx, namespace)) })

	for _, name := range []string{"stale-input", "write-conflict", "stale-cache"} {
		beforeRead := name == "stale-input"
		t.Run(name, func(t *testing.T) {
			allocation, err := marshalJSON(&sandboxAllocation{Pods: []string{"pod-1"}})
			require.NoError(t, err)
			sandbox := &sandboxv1alpha1.BatchSandbox{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace.Name,
					Finalizers:  []string{finalizerPoolAllocation, finalizerTaskCleanup, "example.com/retained"},
					Annotations: map[string]string{annoAllocStatusKey: allocation}},
				Spec: sandboxv1alpha1.BatchSandboxSpec{Replicas: ptr.To(int32(1))},
			}
			require.NoError(t, apiClient.Create(ctx, sandbox))
			require.NoError(t, apiClient.Delete(ctx, sandbox))
			require.NoError(t, apiClient.Get(ctx, client.ObjectKeyFromObject(sandbox), sandbox))
			cached := sandbox.DeepCopy()
			removeTaskFinalizer := func() {
				latest := &sandboxv1alpha1.BatchSandbox{}
				require.NoError(t, apiClient.Get(ctx, client.ObjectKeyFromObject(sandbox), latest))
				latest.Finalizers = []string{finalizerPoolAllocation, "example.com/retained"}
				latest.Annotations["example.com/concurrent"] = "preserved"
				require.NoError(t, apiClient.Update(ctx, latest))
			}
			if beforeRead {
				removeTaskFinalizer()
			}
			patches, cachedReads := 0, 0
			conflictClient := interceptor.NewClient(apiClient, interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if name == "stale-cache" {
						cachedReads++
						cached.DeepCopyInto(obj.(*sandboxv1alpha1.BatchSandbox))
						return nil
					}
					return c.Get(ctx, key, obj, opts...)
				},
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					patches++
					if !beforeRead && patches == 1 {
						removeTaskFinalizer()
					}
					return c.Patch(ctx, obj, patch, opts...)
				},
			})
			syncer := NewDefaultAllocator(conflictClient, apiClient).(*defaultAllocator).syncer
			require.NoError(t, syncer.SetReleased(ctx, cached, &allocationReleased{Pods: []string{"pod-1"}}))
			stored := &sandboxv1alpha1.BatchSandbox{}
			require.NoError(t, apiClient.Get(ctx, client.ObjectKeyFromObject(sandbox), stored))
			require.Equal(t, []string{"example.com/retained"}, stored.Finalizers)
			require.Equal(t, "preserved", stored.Annotations["example.com/concurrent"])
			released, err := syncer.GetReleased(ctx, stored)
			require.NoError(t, err)
			require.Equal(t, []string{"pod-1"}, released.Pods)
			if !beforeRead {
				require.GreaterOrEqual(t, patches, 2, "a conflicting patch must reread and retry")
			}
			require.Zero(t, cachedReads, "retry reads must bypass the informer cache")
			stored.Finalizers = nil
			require.NoError(t, apiClient.Update(ctx, stored))
		})
	}
	t.Run("allocation-changes-on-retry", func(t *testing.T) {
		allocation, err := marshalJSON(&sandboxAllocation{Pods: []string{"pod-1"}})
		require.NoError(t, err)
		sandbox := &sandboxv1alpha1.BatchSandbox{
			ObjectMeta: metav1.ObjectMeta{Name: "allocation-changes", Namespace: namespace.Name,
				Finalizers:  []string{finalizerPoolAllocation, "example.com/retained"},
				Annotations: map[string]string{annoAllocStatusKey: allocation}},
			Spec: sandboxv1alpha1.BatchSandboxSpec{Replicas: ptr.To(int32(1))},
		}
		require.NoError(t, apiClient.Create(ctx, sandbox))
		require.NoError(t, apiClient.Delete(ctx, sandbox))
		patches := 0
		conflictClient := interceptor.NewClient(apiClient, interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				patches++
				if patches == 1 {
					latest := &sandboxv1alpha1.BatchSandbox{}
					require.NoError(t, apiClient.Get(ctx, client.ObjectKeyFromObject(sandbox), latest))
					latest.Annotations[annoAllocStatusKey], err = marshalJSON(&sandboxAllocation{Pods: []string{"pod-1", "pod-2"}})
					require.NoError(t, err)
					require.NoError(t, apiClient.Update(ctx, latest))
				}
				return c.Patch(ctx, obj, patch, opts...)
			},
		})
		syncer := &annoAllocationSyncer{client: conflictClient, apiReader: apiClient}
		require.NoError(t, syncer.SetReleased(ctx, sandbox, &allocationReleased{Pods: []string{"pod-1"}}))
		require.GreaterOrEqual(t, patches, 2)
		stored := &sandboxv1alpha1.BatchSandbox{}
		require.NoError(t, apiClient.Get(ctx, client.ObjectKeyFromObject(sandbox), stored))
		require.Contains(t, stored.Finalizers, finalizerPoolAllocation, "newly allocated pods must be released before dropping the guard")
		stored.Finalizers = nil
		require.NoError(t, apiClient.Update(ctx, stored))
	})

	t.Run("recreated-name", func(t *testing.T) {
		replacement := &sandboxv1alpha1.BatchSandbox{
			ObjectMeta: metav1.ObjectMeta{Name: "recreated", Namespace: namespace.Name},
			Spec:       sandboxv1alpha1.BatchSandboxSpec{Replicas: ptr.To(int32(1))},
		}
		require.NoError(t, apiClient.Create(ctx, replacement))
		stale := replacement.DeepCopy()
		stale.UID = "previous-sandbox"
		syncer := &annoAllocationSyncer{client: apiClient, apiReader: apiClient}
		require.NoError(t, syncer.SetReleased(ctx, stale, &allocationReleased{Pods: []string{"old-pod"}}))
		stored := &sandboxv1alpha1.BatchSandbox{}
		require.NoError(t, apiClient.Get(ctx, client.ObjectKeyFromObject(replacement), stored))
		require.Empty(t, stored.Annotations[annoAllocReleasedKey], "release state from a deleted sandbox must not touch its replacement")
		require.NoError(t, apiClient.Delete(ctx, stored))
	})

	t.Run("already-deleted", func(t *testing.T) {
		syncer := &annoAllocationSyncer{client: apiClient, apiReader: apiClient}
		gone := &sandboxv1alpha1.BatchSandbox{ObjectMeta: metav1.ObjectMeta{Name: "gone", Namespace: namespace.Name}}
		require.True(t, apierrors.IsNotFound(apiClient.Get(ctx, client.ObjectKeyFromObject(gone), &sandboxv1alpha1.BatchSandbox{})))
		require.NoError(t, syncer.SetReleased(ctx, gone, &allocationReleased{}))
	})
}
