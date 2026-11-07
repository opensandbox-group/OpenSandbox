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

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils/expectations"
)

func TestBatchSandboxReconcileClearsDeletedStatusExpectation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		present   bool
		finalizer string
		wantErr   bool
	}{
		{name: "already gone"},
		{name: "terminating without task cleanup", present: true, finalizer: "test/keep-alive"},
		{name: "terminating with cleanup retry", present: true, finalizer: finalizerTaskCleanup, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stale := &sandboxv1alpha1.BatchSandbox{ObjectMeta: metav1.ObjectMeta{
				Namespace: "default", Name: "deleted", UID: types.UID("deleted-uid"), ResourceVersion: "1",
			}}
			patched := stale.DeepCopy()
			patched.ResourceVersion = "2"
			expectation := expectations.NewResourceVersionExpectation()
			expectation.Expect(patched)
			satisfied, _ := expectation.IsSatisfied(stale)
			require.False(t, satisfied)

			// Cleanup must not discard another namespace's same-named object.
			other := patched.DeepCopy()
			other.Namespace = "other"
			other.UID = types.UID("other-uid")
			expectation.Expect(other)
			other.ResourceVersion = "1"

			builder := fake.NewClientBuilder().WithScheme(testscheme)
			if tc.present {
				terminating := stale.DeepCopy()
				now := metav1.Now()
				terminating.DeletionTimestamp = &now
				terminating.Finalizers = []string{tc.finalizer}
				builder.WithObjects(terminating)
			}
			r := &BatchSandboxReconciler{Client: builder.Build(), StatusRVExpectation: expectation}
			_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(stale)})
			if tc.wantErr {
				require.ErrorContains(t, err, "uncached API reader is not configured")
			} else {
				require.NoError(t, err)
			}
			satisfied, _ = expectation.IsSatisfied(stale)
			require.True(t, satisfied, "deleted sandbox must not retain its last status expectation")
			satisfied, _ = expectation.IsSatisfied(other)
			require.False(t, satisfied, "another sandbox's expectation must remain")
		})
	}
}
