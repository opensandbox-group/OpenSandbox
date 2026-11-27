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
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils/expectations"
)

// TestExpiredReconcileAgainstAPIServer runs the expiry decision against a real
// API server, because the delete preconditions this fix depends on are
// evaluated by the server, not by a client-side stub. The informer copy is
// still served stale through an intercepted read, so the cases exercised here
// are exactly the ones the controller meets in a cluster.
func TestExpiredReconcileAgainstAPIServer(t *testing.T) {
	testEnvironment := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory: getFirstFoundEnvTestBinaryDir(),
	}
	config, err := testEnvironment.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, testEnvironment.Stop()) })

	apiClient, err := client.NewWithWatch(config, client.Options{Scheme: testscheme})
	require.NoError(t, err)
	ctx := context.Background()

	newExpiryReconciler := func(cl client.WithWatch) *BatchSandboxReconciler {
		return &BatchSandboxReconciler{
			Client:              cl,
			APIReader:           apiClient,
			Scheme:              testscheme,
			Recorder:            record.NewFakeRecorder(10),
			StatusRVExpectation: expectations.NewResourceVersionExpectation(),
		}
	}

	t.Run("renewed sandbox survives a stale expiry decision", func(t *testing.T) {
		sandbox := &sandboxv1alpha1.BatchSandbox{
			ObjectMeta: metav1.ObjectMeta{Name: "renewed", Namespace: "default"},
			Spec: sandboxv1alpha1.BatchSandboxSpec{
				Replicas:   ptr.To(int32(1)),
				ExpireTime: &metav1.Time{Time: time.Now().Add(-time.Minute)},
			},
		}
		require.NoError(t, apiClient.Create(ctx, sandbox))
		key := client.ObjectKeyFromObject(sandbox)

		stale := sandbox.DeepCopy()
		renewed := sandbox.DeepCopy()
		renewed.Spec.ExpireTime = &metav1.Time{Time: time.Now().Add(time.Hour)}
		require.NoError(t, apiClient.Update(ctx, renewed))
		require.NotEqual(t, stale.ResourceVersion, renewed.ResourceVersion,
			"the renewal must advance the resourceVersion so the cached copy is outdated")

		// The informer cache has not observed the renewal yet and still serves
		// the expired copy. The API server holds the renewed object.
		cachedClient := interceptor.NewClient(apiClient, interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, k client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				cached, ok := obj.(*sandboxv1alpha1.BatchSandbox)
				if !ok || k != key {
					return c.Get(ctx, k, obj, opts...)
				}
				stale.DeepCopyInto(cached)
				return nil
			},
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				options := &client.DeleteOptions{}
				options.ApplyOptions(opts)
				require.NotNil(t, options.Preconditions, "the expiry delete must carry preconditions")
				require.Equal(t, stale.ResourceVersion, *options.Preconditions.ResourceVersion,
					"only the object version judged expired may be deleted")
				return c.Delete(ctx, obj, opts...)
			},
		})

		result, err := newExpiryReconciler(cachedClient).Reconcile(ctx, ctrl.Request{NamespacedName: key})
		require.NoError(t, err)
		require.Positive(t, result.RequeueAfter, "a stale expiry delete must be retried after the cache catches up")

		current := &sandboxv1alpha1.BatchSandbox{}
		require.NoError(t, apiClient.Get(ctx, key, current),
			"expiry must not delete a sandbox whose renewal is already in the API server")
		require.Equal(t, renewed.Spec.ExpireTime.Time.UTC(), current.Spec.ExpireTime.Time.UTC())
		require.NoError(t, apiClient.Delete(ctx, current))
	})

	t.Run("unchanged expired sandbox is still deleted", func(t *testing.T) {
		sandbox := &sandboxv1alpha1.BatchSandbox{
			ObjectMeta: metav1.ObjectMeta{Name: "expired", Namespace: "default"},
			Spec: sandboxv1alpha1.BatchSandboxSpec{
				Replicas:   ptr.To(int32(1)),
				ExpireTime: &metav1.Time{Time: time.Now().Add(-time.Minute)},
			},
		}
		require.NoError(t, apiClient.Create(ctx, sandbox))
		key := client.ObjectKeyFromObject(sandbox)

		inspectedClient := interceptor.NewClient(apiClient, interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				options := &client.DeleteOptions{}
				options.ApplyOptions(opts)
				require.NotNil(t, options.Preconditions, "the expiry delete must carry preconditions")
				require.Equal(t, sandbox.UID, *options.Preconditions.UID)
				require.Equal(t, sandbox.ResourceVersion, *options.Preconditions.ResourceVersion)
				return c.Delete(ctx, obj, opts...)
			},
		})

		_, err := newExpiryReconciler(inspectedClient).Reconcile(ctx, ctrl.Request{NamespacedName: key})
		require.NoError(t, err)

		current := &sandboxv1alpha1.BatchSandbox{}
		require.NoError(t, apiClient.Get(ctx, key, current))
		require.NotNil(t, current.DeletionTimestamp,
			"an unchanged expired sandbox must still be deleted")
		require.Contains(t, current.Finalizers, metav1.FinalizerDeleteDependents,
			"the expiry delete must use the foreground policy")

		// Envtest runs no garbage collector, so perform its cleanup explicitly:
		// drop the foreground finalizer and the object disappears.
		controllerutil.RemoveFinalizer(current, metav1.FinalizerDeleteDependents)
		require.NoError(t, apiClient.Update(ctx, current))
		require.True(t, apierrors.IsNotFound(apiClient.Get(ctx, key, &sandboxv1alpha1.BatchSandbox{})),
			"an unchanged expired sandbox must still be removed")
	})
}
