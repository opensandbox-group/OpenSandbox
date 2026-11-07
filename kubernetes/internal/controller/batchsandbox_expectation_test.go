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
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils/expectations"
)

// statusExpectationKept reports whether r still holds a status expectation
// for uid, by asking about a version older than any recorded one.
func statusExpectationKept(r *BatchSandboxReconciler, uid types.UID) bool {
	satisfied, _ := r.StatusRVExpectation.IsSatisfied(&metav1.ObjectMeta{UID: uid, ResourceVersion: "0"})
	return !satisfied
}

// The BatchSandbox is deleted after its status patch commits but before the
// patch response returns, so the delete notification can be handled before
// updateStatus records the expectation. The next reconcile of the key still
// drops it.
func TestDeletionDuringStatusPatchDropsStatusExpectation(t *testing.T) {
	bs := &sandboxv1alpha1.BatchSandbox{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "bs", UID: "uid-1"}}
	fakeClient := fake.NewClientBuilder().WithScheme(testscheme).
		WithStatusSubresource(&sandboxv1alpha1.BatchSandbox{}).WithObjects(bs.DeepCopy()).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, c client.Client, subResource string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				if err := c.SubResource(subResource).Patch(ctx, obj, patch, opts...); err != nil {
					return err
				}
				return c.Delete(ctx, obj.DeepCopyObject().(client.Object))
			},
		}).Build()
	r := &BatchSandboxReconciler{Client: fakeClient, StatusRVExpectation: expectations.NewResourceVersionExpectation()}
	ctx := context.Background()

	require.NoError(t, r.updateStatus(ctx, bs, &sandboxv1alpha1.BatchSandboxStatus{Phase: sandboxv1alpha1.BatchSandboxPhasePending}))
	require.True(t, statusExpectationKept(r, bs.UID))

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(bs)})
	require.NoError(t, err)
	require.False(t, statusExpectationKept(r, bs.UID), "the deleted BatchSandbox's status expectation was kept")
}

// A deletion followed by recreation under the same name can reach the
// informer as a single update, with no delete notification for the old UID.
func TestReplacedBatchSandboxDropsOldStatusExpectation(t *testing.T) {
	replacement := &sandboxv1alpha1.BatchSandbox{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "bs", UID: "uid-new"}}
	fakeClient := fake.NewClientBuilder().WithScheme(testscheme).
		WithStatusSubresource(&sandboxv1alpha1.BatchSandbox{}).WithObjects(replacement.DeepCopy()).Build()
	r := &BatchSandboxReconciler{Client: fakeClient, StatusRVExpectation: expectations.NewResourceVersionExpectation()}
	ctx := context.Background()
	r.StatusRVExpectation.Expect(&metav1.ObjectMeta{Namespace: "default", Name: "bs", UID: "uid-old", ResourceVersion: "5"})
	r.StatusRVExpectation.Expect(&metav1.ObjectMeta{Namespace: "default", Name: "bs", UID: replacement.UID, ResourceVersion: "9"})

	_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(replacement)})

	require.False(t, statusExpectationKept(r, "uid-old"), "the replaced BatchSandbox's status expectation was kept")
	require.True(t, statusExpectationKept(r, replacement.UID), "the replacement's status expectation was dropped")
}
