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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
	poolassign "github.com/alibaba/OpenSandbox/sandbox-k8s/internal/controller/poolassign"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils/expectations"
)

func TestReconcilePublishesAutoPoolCapacityCondition(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, sandboxv1alpha1.AddToScheme(scheme))
	sandbox := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx", Namespace: "ns"},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			PoolRef:  poolAutoAssignRef,
			Replicas: ptr.To(int32(1)),
			Template: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Image: "nginx"}}},
			},
		},
	}
	pool := &sandboxv1alpha1.Pool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "ns"},
		Spec: sandboxv1alpha1.PoolSpec{
			Template: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Image: "nginx"}}},
			},
			CapacitySpec: sandboxv1alpha1.CapacitySpec{PoolMax: 2},
		},
		Status: sandboxv1alpha1.PoolStatus{Allocated: 2},
	}
	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&sandboxv1alpha1.BatchSandbox{}, &sandboxv1alpha1.Pool{}).
		WithObjects(sandbox, pool).
		Build()
	reconciler := &BatchSandboxReconciler{
		Client:              client,
		Scheme:              scheme,
		Recorder:            record.NewFakeRecorder(10),
		ProfileStore:        poolassign.NewProfileStore(),
		StatusRVExpectation: expectations.NewResourceVersionExpectation(),
	}

	result, err := reconciler.Reconcile(
		context.Background(),
		ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "sbx"}},
	)

	require.NoError(t, err)
	assert.Equal(t, poolAllocationRetryTime, result.RequeueAfter)
	updated := &sandboxv1alpha1.BatchSandbox{}
	require.NoError(t, client.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "sbx"}, updated))
	require.Len(t, updated.Status.Conditions, 1)
	assert.Equal(t, sandboxv1alpha1.BatchSandboxConditionPoolAllocationPending, updated.Status.Conditions[0].Type)
	assert.Equal(t, sandboxv1alpha1.ConditionTrue, updated.Status.Conditions[0].Status)
	assert.Equal(t, poolCapacityExhaustedReason, updated.Status.Conditions[0].Reason)

	pool.Status.Allocated = 1
	require.NoError(t, client.Status().Update(context.Background(), pool))
	result, err = reconciler.Reconcile(
		context.Background(),
		ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "sbx"}},
	)

	require.NoError(t, err)
	assert.Zero(t, result.RequeueAfter)
	require.NoError(t, client.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "sbx"}, updated))
	assert.Equal(t, "pool-a", updated.Spec.PoolRef)
	assert.Empty(t, updated.Status.Conditions)
}

func TestReconcileClearsAutoPoolCapacityConditionOnNonCapacityFailure(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, sandboxv1alpha1.AddToScheme(scheme))
	sandbox := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx", Namespace: "ns"},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			PoolRef:  poolAutoAssignRef,
			Replicas: ptr.To(int32(1)),
			Template: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Image: "nginx"}}},
			},
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			Conditions: []sandboxv1alpha1.BatchSandboxCondition{
				{
					Type:   sandboxv1alpha1.BatchSandboxConditionPoolAllocationPending,
					Status: sandboxv1alpha1.ConditionTrue,
					Reason: poolCapacityExhaustedReason,
				},
			},
		},
	}
	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&sandboxv1alpha1.BatchSandbox{}).
		WithObjects(sandbox).
		Build()
	reconciler := &BatchSandboxReconciler{
		Client:              client,
		Scheme:              scheme,
		Recorder:            record.NewFakeRecorder(10),
		ProfileStore:        poolassign.NewProfileStore(),
		StatusRVExpectation: expectations.NewResourceVersionExpectation(),
	}

	_, err := reconciler.Reconcile(
		context.Background(),
		ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "sbx"}},
	)

	require.Error(t, err)
	updated := &sandboxv1alpha1.BatchSandbox{}
	require.NoError(t, client.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "sbx"}, updated))
	assert.Empty(t, updated.Status.Conditions)
}

func TestReconcileRequeuesOnlyForFixedPoolCapacityWait(t *testing.T) {
	tests := []struct {
		name        string
		allocated   int32
		wantRequeue time.Duration
	}{
		{
			name:        "pool has headroom",
			allocated:   1,
			wantRequeue: 0,
		},
		{
			name:        "pool is capacity blocked",
			allocated:   2,
			wantRequeue: poolAllocationRetryTime,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, corev1.AddToScheme(scheme))
			require.NoError(t, sandboxv1alpha1.AddToScheme(scheme))
			sandbox := &sandboxv1alpha1.BatchSandbox{
				ObjectMeta: metav1.ObjectMeta{Name: "sbx", Namespace: "ns"},
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					PoolRef:  "pool-a",
					Replicas: ptr.To(int32(1)),
				},
				Status: sandboxv1alpha1.BatchSandboxStatus{
					Phase: sandboxv1alpha1.BatchSandboxPhasePending,
				},
			}
			pool := &sandboxv1alpha1.Pool{
				ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "ns"},
				Spec: sandboxv1alpha1.PoolSpec{
					CapacitySpec: sandboxv1alpha1.CapacitySpec{PoolMax: 2},
				},
				Status: sandboxv1alpha1.PoolStatus{Allocated: tt.allocated},
			}
			client := fake.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(&sandboxv1alpha1.BatchSandbox{}, &sandboxv1alpha1.Pool{}).
				WithObjects(sandbox, pool).
				Build()
			reconciler := &BatchSandboxReconciler{
				Client:              client,
				Scheme:              scheme,
				Recorder:            record.NewFakeRecorder(10),
				StatusRVExpectation: expectations.NewResourceVersionExpectation(),
			}

			result, err := reconciler.Reconcile(
				context.Background(),
				ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "sbx"}},
			)

			require.NoError(t, err)
			assert.Equal(t, tt.wantRequeue, result.RequeueAfter)
		})
	}
}

func TestApplyFixedPoolCapacityCondition(t *testing.T) {
	newReconciler := func(t *testing.T, allocated int32) *BatchSandboxReconciler {
		t.Helper()
		scheme := runtime.NewScheme()
		require.NoError(t, sandboxv1alpha1.AddToScheme(scheme))
		pool := &sandboxv1alpha1.Pool{
			ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "ns"},
			Spec: sandboxv1alpha1.PoolSpec{
				CapacitySpec: sandboxv1alpha1.CapacitySpec{PoolMax: 2},
			},
			Status: sandboxv1alpha1.PoolStatus{Allocated: allocated},
		}
		return &BatchSandboxReconciler{
			Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(pool).Build(),
		}
	}
	newSandbox := func() *sandboxv1alpha1.BatchSandbox {
		return &sandboxv1alpha1.BatchSandbox{
			ObjectMeta: metav1.ObjectMeta{Name: "sbx", Namespace: "ns"},
			Spec: sandboxv1alpha1.BatchSandboxSpec{
				PoolRef:  "pool-a",
				Replicas: ptr.To(int32(1)),
			},
		}
	}

	t.Run("sets condition when fixed pool is full", func(t *testing.T) {
		reconciler := newReconciler(t, 2)
		status := &sandboxv1alpha1.BatchSandboxStatus{Allocated: 0}

		pending, err := reconciler.applyFixedPoolCapacityCondition(
			context.Background(), newSandbox(), status,
		)

		require.NoError(t, err)
		assert.True(t, pending)
		require.Len(t, status.Conditions, 1)
		assert.Equal(t, sandboxv1alpha1.BatchSandboxConditionPoolAllocationPending, status.Conditions[0].Type)
		assert.Equal(t, sandboxv1alpha1.ConditionTrue, status.Conditions[0].Status)
		assert.Equal(t, poolCapacityExhaustedReason, status.Conditions[0].Reason)
	})

	t.Run("clears stale condition when headroom is available", func(t *testing.T) {
		reconciler := newReconciler(t, 1)
		status := &sandboxv1alpha1.BatchSandboxStatus{
			Allocated: 0,
			Conditions: []sandboxv1alpha1.BatchSandboxCondition{
				{
					Type:   sandboxv1alpha1.BatchSandboxConditionPoolAllocationPending,
					Status: sandboxv1alpha1.ConditionTrue,
					Reason: poolCapacityExhaustedReason,
				},
			},
		}

		pending, err := reconciler.applyFixedPoolCapacityCondition(
			context.Background(), newSandbox(), status,
		)

		require.NoError(t, err)
		assert.False(t, pending)
		assert.Empty(t, status.Conditions)
	})

	t.Run("allocated sandbox is never reported as capacity blocked", func(t *testing.T) {
		reconciler := newReconciler(t, 2)
		status := &sandboxv1alpha1.BatchSandboxStatus{
			Allocated: 1,
			Conditions: []sandboxv1alpha1.BatchSandboxCondition{
				{
					Type:   sandboxv1alpha1.BatchSandboxConditionPoolAllocationPending,
					Status: sandboxv1alpha1.ConditionTrue,
					Reason: poolCapacityExhaustedReason,
				},
			},
		}

		pending, err := reconciler.applyFixedPoolCapacityCondition(
			context.Background(), newSandbox(), status,
		)

		require.NoError(t, err)
		assert.False(t, pending)
		assert.Empty(t, status.Conditions)
	})

	t.Run("paused sandbox does not inspect pool capacity", func(t *testing.T) {
		scheme := runtime.NewScheme()
		require.NoError(t, sandboxv1alpha1.AddToScheme(scheme))
		reconciler := &BatchSandboxReconciler{
			Client: fake.NewClientBuilder().WithScheme(scheme).Build(),
		}
		status := &sandboxv1alpha1.BatchSandboxStatus{
			Phase: sandboxv1alpha1.BatchSandboxPhasePaused,
		}

		pending, err := reconciler.applyFixedPoolCapacityCondition(
			context.Background(), newSandbox(), status,
		)

		require.NoError(t, err)
		assert.False(t, pending)
		assert.Empty(t, status.Conditions)
	})

	t.Run("missing fixed pool stays non-capacity pending", func(t *testing.T) {
		scheme := runtime.NewScheme()
		require.NoError(t, sandboxv1alpha1.AddToScheme(scheme))
		reconciler := &BatchSandboxReconciler{
			Client: fake.NewClientBuilder().WithScheme(scheme).Build(),
		}
		status := &sandboxv1alpha1.BatchSandboxStatus{
			Conditions: []sandboxv1alpha1.BatchSandboxCondition{
				{
					Type:   sandboxv1alpha1.BatchSandboxConditionPoolAllocationPending,
					Status: sandboxv1alpha1.ConditionTrue,
					Reason: poolCapacityExhaustedReason,
				},
			},
		}

		pending, err := reconciler.applyFixedPoolCapacityCondition(
			context.Background(), newSandbox(), status,
		)

		require.NoError(t, err)
		assert.False(t, pending)
		assert.Empty(t, status.Conditions)
	})
}

func TestInitialUnallocatedSandboxPersistsPoolCapacityCondition(t *testing.T) {
	sandbox := &sandboxv1alpha1.BatchSandbox{
		Spec: sandboxv1alpha1.BatchSandboxSpec{Replicas: ptr.To(int32(1))},
	}
	view := runtimeView{status: &sandboxv1alpha1.BatchSandboxStatus{
		Conditions: []sandboxv1alpha1.BatchSandboxCondition{
			{
				Type:   sandboxv1alpha1.BatchSandboxConditionPoolAllocationPending,
				Status: sandboxv1alpha1.ConditionTrue,
			},
		},
	}}

	assert.False(t, isInitialUnallocatedSandbox(sandbox, view))
}

func TestSetPoolAllocationPendingPreservesConcurrentLifecycleConditions(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, sandboxv1alpha1.AddToScheme(scheme))
	latest := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx", Namespace: "ns"},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			Conditions: []sandboxv1alpha1.BatchSandboxCondition{
				{
					Type:    sandboxv1alpha1.BatchSandboxConditionResumeFailed,
					Status:  sandboxv1alpha1.ConditionTrue,
					Reason:  "PodStartFailed",
					Message: "image pull failed",
				},
			},
		},
	}
	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&sandboxv1alpha1.BatchSandbox{}).
		WithObjects(latest).
		Build()
	reconciler := &BatchSandboxReconciler{
		Client:              client,
		StatusRVExpectation: expectations.NewResourceVersionExpectation(),
	}
	stale := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx", Namespace: "ns"},
	}

	err := reconciler.setPoolAllocationPending(
		context.Background(), stale, true, "Pool pool-a is at capacity",
	)

	require.NoError(t, err)
	updated := &sandboxv1alpha1.BatchSandbox{}
	require.NoError(t, client.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "sbx"}, updated))
	require.Len(t, updated.Status.Conditions, 2)
	conditions := make(map[sandboxv1alpha1.BatchSandboxConditionType]sandboxv1alpha1.BatchSandboxCondition)
	for _, condition := range updated.Status.Conditions {
		conditions[condition.Type] = condition
	}
	assert.Equal(t, sandboxv1alpha1.ConditionTrue, conditions[sandboxv1alpha1.BatchSandboxConditionResumeFailed].Status)
	assert.Equal(t, "PodStartFailed", conditions[sandboxv1alpha1.BatchSandboxConditionResumeFailed].Reason)
	assert.Equal(t, sandboxv1alpha1.ConditionTrue, conditions[sandboxv1alpha1.BatchSandboxConditionPoolAllocationPending].Status)
	assert.Equal(t, poolCapacityExhaustedReason, conditions[sandboxv1alpha1.BatchSandboxConditionPoolAllocationPending].Reason)
}
