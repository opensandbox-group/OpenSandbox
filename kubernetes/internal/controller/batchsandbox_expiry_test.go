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
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
)

func TestExpiredReconcileKeepsSandboxRenewedInAPIServer(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	sandbox := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "renewed", Namespace: "default"},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Replicas:   ptr.To(int32(1)),
			ExpireTime: &metav1.Time{Time: now.Add(-time.Minute)},
		},
	}
	r := newTestReconciler(sandbox)
	live := r.Client.(client.WithWatch)
	key := client.ObjectKeyFromObject(sandbox)

	stale := &sandboxv1alpha1.BatchSandbox{}
	require.NoError(t, live.Get(ctx, key, stale))
	staleRV := stale.ResourceVersion

	renewed := stale.DeepCopy()
	renewed.Spec.ExpireTime = &metav1.Time{Time: now.Add(time.Hour)}
	require.NoError(t, live.Update(ctx, renewed))
	require.NotEqual(t, staleRV, renewed.ResourceVersion)

	// The informer cache has not observed the renewal yet and still serves the
	// expired copy. The API server holds the renewed object.
	r.Client = interceptor.NewClient(live, interceptor.Funcs{
		Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
			batchSandbox, ok := obj.(*sandboxv1alpha1.BatchSandbox)
			if !ok {
				return live.Get(ctx, key, obj)
			}
			stale.DeepCopyInto(batchSandbox)
			return nil
		},
	})
	r.APIReader = live

	t.Cleanup(func() { _ = durationStore.Pop(key.String()) })
	result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	require.NoError(t, err)

	current := &sandboxv1alpha1.BatchSandbox{}
	require.NoError(t, live.Get(ctx, key, current), "expiry must not delete a sandbox whose renewal is already in the API server")
	require.Equal(t, renewed.Spec.ExpireTime.Time.UTC(), current.Spec.ExpireTime.Time.UTC())
	require.Positive(t, result.RequeueAfter, "a stale expiry decision must retry after the cache catches up")
}

func TestExpiredReconcileDeletesUnchangedSandbox(t *testing.T) {
	ctx := context.Background()
	sandbox := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "expired", Namespace: "default"},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Replicas:   ptr.To(int32(1)),
			ExpireTime: &metav1.Time{Time: time.Now().Add(-time.Minute)},
		},
	}
	r := newTestReconciler(sandbox)
	live := r.Client.(client.WithWatch)
	key := client.ObjectKeyFromObject(sandbox)

	cached := &sandboxv1alpha1.BatchSandbox{}
	require.NoError(t, live.Get(ctx, key, cached))
	r.Client = interceptor.NewClient(live, interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			options := &client.DeleteOptions{}
			options.ApplyOptions(opts)
			require.NotNil(t, options.Preconditions)
			require.Equal(t, cached.UID, *options.Preconditions.UID)
			require.Equal(t, cached.ResourceVersion, *options.Preconditions.ResourceVersion)
			return c.Delete(ctx, obj, opts...)
		},
	})
	t.Cleanup(func() { _ = durationStore.Pop(key.String()) })

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	require.NoError(t, err)
	require.True(t, apierrors.IsNotFound(live.Get(ctx, key, &sandboxv1alpha1.BatchSandbox{})),
		"an unchanged expired sandbox must still be deleted")
}
