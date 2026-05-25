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
	"encoding/json"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/controller/algorithm"
)

// newTestAllocator creates an Allocator backed by mock store and syncer.
func newTestAllocator(ctrl *gomock.Controller) (Allocator, *MockAllocationStore, *MockAllocationSyncer) {
	store := NewMockAllocationStore(ctrl)
	syncer := NewMockAllocationSyncer(ctrl)
	return &defaultAllocator{store: store, syncer: syncer, algorithm: &algorithm.PackedSchedule{}}, store, syncer
}

// --- Schedule ---

func TestSchedule(t *testing.T) {
	replica1 := int32(1)
	replica2 := int32(2)

	tests := []struct {
		name          string
		spec          *AllocSpec
		poolAlloc     *PoolAllocation
		sandboxAllocs map[string]*SandboxAllocation
		releases      map[string]*AllocationRelease
		released      map[string]*AllocationReleased
		wantAction    *algorithm.AllocAction
	}{
		{
			name: "allocate normally - 2 pods for 2 sandboxes",
			spec: &AllocSpec{
				Pods: []*corev1.Pod{
					{ObjectMeta: metav1.ObjectMeta{Name: "pod1"}, Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}},
					{ObjectMeta: metav1.ObjectMeta{Name: "pod2"}, Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}},
				},
				Pool: &sandboxv1alpha1.Pool{ObjectMeta: metav1.ObjectMeta{Name: "pool1"}},
				Sandboxes: []*sandboxv1alpha1.BatchSandbox{
					{ObjectMeta: metav1.ObjectMeta{Name: "sbx1"}, Spec: sandboxv1alpha1.BatchSandboxSpec{Replicas: &replica1}},
					{ObjectMeta: metav1.ObjectMeta{Name: "sbx2"}, Spec: sandboxv1alpha1.BatchSandboxSpec{Replicas: &replica1}},
				},
			},
			poolAlloc:     &PoolAllocation{PodAllocation: map[string]string{}},
			sandboxAllocs: map[string]*SandboxAllocation{"sbx1": {Pods: []string{}}, "sbx2": {Pods: []string{}}},
			releases:      map[string]*AllocationRelease{"sbx1": {Pods: []string{}}, "sbx2": {Pods: []string{}}},
			released:      map[string]*AllocationReleased{"sbx1": {Pods: []string{}}, "sbx2": {Pods: []string{}}},
			wantAction: &algorithm.AllocAction{
				ToAllocate:    map[string][]string{"sbx1": {"pod1"}, "sbx2": {"pod2"}},
				ToRelease:     map[string][]string{},
				PodSupplement: 0,
			},
		},
		{
			name: "skip non-running pods",
			spec: &AllocSpec{
				Pods: []*corev1.Pod{
					{ObjectMeta: metav1.ObjectMeta{Name: "pod1"}, Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}},
					{ObjectMeta: metav1.ObjectMeta{Name: "pod2"}, Status: corev1.PodStatus{Phase: corev1.PodPending}},
				},
				Pool: &sandboxv1alpha1.Pool{ObjectMeta: metav1.ObjectMeta{Name: "pool1"}},
				Sandboxes: []*sandboxv1alpha1.BatchSandbox{
					{ObjectMeta: metav1.ObjectMeta{Name: "sbx1"}, Spec: sandboxv1alpha1.BatchSandboxSpec{Replicas: &replica1}},
					{ObjectMeta: metav1.ObjectMeta{Name: "sbx2"}, Spec: sandboxv1alpha1.BatchSandboxSpec{Replicas: &replica1}},
				},
			},
			poolAlloc:     &PoolAllocation{PodAllocation: map[string]string{}},
			sandboxAllocs: map[string]*SandboxAllocation{"sbx1": {Pods: []string{}}, "sbx2": {Pods: []string{}}},
			releases:      map[string]*AllocationRelease{"sbx1": {Pods: []string{}}, "sbx2": {Pods: []string{}}},
			released:      map[string]*AllocationReleased{"sbx1": {Pods: []string{}}, "sbx2": {Pods: []string{}}},
			wantAction: &algorithm.AllocAction{
				ToAllocate:    map[string][]string{"sbx1": {"pod1"}},
				ToRelease:     map[string][]string{},
				PodSupplement: 1,
			},
		},
		{
			name: "partial allocated - allocate remaining",
			spec: &AllocSpec{
				Pods: []*corev1.Pod{
					{ObjectMeta: metav1.ObjectMeta{Name: "pod1"}, Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}},
					{ObjectMeta: metav1.ObjectMeta{Name: "pod2"}, Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}},
				},
				Pool: &sandboxv1alpha1.Pool{ObjectMeta: metav1.ObjectMeta{Name: "pool1"}},
				Sandboxes: []*sandboxv1alpha1.BatchSandbox{
					{ObjectMeta: metav1.ObjectMeta{Name: "sbx1"}, Spec: sandboxv1alpha1.BatchSandboxSpec{Replicas: &replica2}},
				},
			},
			poolAlloc:     &PoolAllocation{PodAllocation: map[string]string{"pod1": "sbx1"}},
			sandboxAllocs: map[string]*SandboxAllocation{"sbx1": {Pods: []string{"pod1"}}},
			releases:      map[string]*AllocationRelease{"sbx1": {Pods: []string{}}},
			released:      map[string]*AllocationReleased{"sbx1": {Pods: []string{}}},
			wantAction: &algorithm.AllocAction{
				ToAllocate:    map[string][]string{"sbx1": {"pod2"}},
				ToRelease:     map[string][]string{},
				PodSupplement: 0,
			},
		},
		{
			name: "with release - pods to release",
			spec: &AllocSpec{
				Pods: []*corev1.Pod{
					{ObjectMeta: metav1.ObjectMeta{Name: "pod1"}, Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}},
				},
				Pool: &sandboxv1alpha1.Pool{ObjectMeta: metav1.ObjectMeta{Name: "pool1"}},
				Sandboxes: []*sandboxv1alpha1.BatchSandbox{
					{ObjectMeta: metav1.ObjectMeta{Name: "sbx1"}, Spec: sandboxv1alpha1.BatchSandboxSpec{Replicas: &replica1}},
				},
			},
			poolAlloc:     &PoolAllocation{PodAllocation: map[string]string{"pod1": "sbx1"}},
			sandboxAllocs: map[string]*SandboxAllocation{"sbx1": {Pods: []string{"pod1"}}},
			releases:      map[string]*AllocationRelease{"sbx1": {Pods: []string{"pod1"}}},
			released:      map[string]*AllocationReleased{"sbx1": {Pods: []string{}}},
			wantAction: &algorithm.AllocAction{
				ToAllocate:    map[string][]string{},
				ToRelease:     map[string][]string{"sbx1": {"pod1"}},
				PodSupplement: 0,
			},
		},
		{
			name: "partial release - only unreleased pods in ToRelease",
			spec: &AllocSpec{
				Pods: []*corev1.Pod{
					{ObjectMeta: metav1.ObjectMeta{Name: "pod1"}, Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}},
					{ObjectMeta: metav1.ObjectMeta{Name: "pod2"}, Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}},
				},
				Pool: &sandboxv1alpha1.Pool{ObjectMeta: metav1.ObjectMeta{Name: "pool1"}},
				Sandboxes: []*sandboxv1alpha1.BatchSandbox{
					{ObjectMeta: metav1.ObjectMeta{Name: "sbx1"}, Spec: sandboxv1alpha1.BatchSandboxSpec{Replicas: &replica2}},
				},
			},
			poolAlloc:     &PoolAllocation{PodAllocation: map[string]string{"pod1": "sbx1", "pod2": "sbx1"}},
			sandboxAllocs: map[string]*SandboxAllocation{"sbx1": {Pods: []string{"pod1", "pod2"}}},
			releases:      map[string]*AllocationRelease{"sbx1": {Pods: []string{"pod1", "pod2"}}},
			released:      map[string]*AllocationReleased{"sbx1": {Pods: []string{"pod1"}}},
			wantAction: &algorithm.AllocAction{
				ToAllocate:    map[string][]string{},
				ToRelease:     map[string][]string{"sbx1": {"pod2"}},
				PodSupplement: 0,
			},
		},
		{
			name: "not enough pods - PodSupplement > 0",
			spec: &AllocSpec{
				Pods: []*corev1.Pod{
					{ObjectMeta: metav1.ObjectMeta{Name: "pod1"}, Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}},
				},
				Pool: &sandboxv1alpha1.Pool{ObjectMeta: metav1.ObjectMeta{Name: "pool1"}},
				Sandboxes: []*sandboxv1alpha1.BatchSandbox{
					{ObjectMeta: metav1.ObjectMeta{Name: "sbx1"}, Spec: sandboxv1alpha1.BatchSandboxSpec{Replicas: &replica2}},
				},
			},
			poolAlloc:     &PoolAllocation{PodAllocation: map[string]string{}},
			sandboxAllocs: map[string]*SandboxAllocation{"sbx1": {Pods: []string{}}},
			releases:      map[string]*AllocationRelease{"sbx1": {Pods: []string{}}},
			released:      map[string]*AllocationReleased{"sbx1": {Pods: []string{}}},
			wantAction: &algorithm.AllocAction{
				ToAllocate:    map[string][]string{"sbx1": {"pod1"}},
				ToRelease:     map[string][]string{},
				PodSupplement: 1,
			},
		},
		{
			name: "skip already allocated pod - other sandbox gets supplement",
			spec: &AllocSpec{
				Pods: []*corev1.Pod{
					{ObjectMeta: metav1.ObjectMeta{Name: "pod1"}, Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}},
				},
				Pool: &sandboxv1alpha1.Pool{ObjectMeta: metav1.ObjectMeta{Name: "pool1"}},
				Sandboxes: []*sandboxv1alpha1.BatchSandbox{
					{ObjectMeta: metav1.ObjectMeta{Name: "sbx1"}, Spec: sandboxv1alpha1.BatchSandboxSpec{Replicas: &replica1}},
					{ObjectMeta: metav1.ObjectMeta{Name: "sbx2"}, Spec: sandboxv1alpha1.BatchSandboxSpec{Replicas: &replica1}},
				},
			},
			poolAlloc:     &PoolAllocation{PodAllocation: map[string]string{"pod1": "sbx1"}},
			sandboxAllocs: map[string]*SandboxAllocation{"sbx1": {Pods: []string{"pod1"}}, "sbx2": {Pods: []string{}}},
			releases:      map[string]*AllocationRelease{"sbx1": {Pods: []string{}}, "sbx2": {Pods: []string{}}},
			released:      map[string]*AllocationReleased{"sbx1": {Pods: []string{}}, "sbx2": {Pods: []string{}}},
			wantAction: &algorithm.AllocAction{
				ToAllocate:    map[string][]string{},
				ToRelease:     map[string][]string{},
				PodSupplement: 1,
			},
		},
		{
			name: "terminating sandbox - queue unreleased pods for release, no supplement",
			spec: &AllocSpec{
				Pods: []*corev1.Pod{
					{ObjectMeta: metav1.ObjectMeta{Name: "pod1"}, Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}},
					{ObjectMeta: metav1.ObjectMeta{Name: "pod2"}, Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}},
				},
				Pool: &sandboxv1alpha1.Pool{ObjectMeta: metav1.ObjectMeta{Name: "pool1"}},
				Sandboxes: []*sandboxv1alpha1.BatchSandbox{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:              "sbx1",
							DeletionTimestamp: &metav1.Time{Time: time.Now()},
						},
						Spec: sandboxv1alpha1.BatchSandboxSpec{Replicas: &replica2},
					},
				},
			},
			poolAlloc:     &PoolAllocation{PodAllocation: map[string]string{"pod1": "sbx1", "pod2": "sbx1"}},
			sandboxAllocs: map[string]*SandboxAllocation{"sbx1": {Pods: []string{"pod1", "pod2"}}},
			releases:      map[string]*AllocationRelease{},
			released:      map[string]*AllocationReleased{"sbx1": {Pods: []string{"pod1"}}},
			wantAction: &algorithm.AllocAction{
				ToAllocate:    map[string][]string{},
				ToRelease:     map[string][]string{"sbx1": {"pod2"}},
				PodSupplement: 0,
			},
		},
		{
			name: "orphan sandbox - pods in store but sandbox no longer in spec",
			spec: &AllocSpec{
				Pods: []*corev1.Pod{
					{ObjectMeta: metav1.ObjectMeta{Name: "pod1"}, Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}},
					{ObjectMeta: metav1.ObjectMeta{Name: "pod2"}, Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}},
				},
				Pool: &sandboxv1alpha1.Pool{ObjectMeta: metav1.ObjectMeta{Name: "pool1"}},
				// sbx-orphan is not in spec (e.g. force-deleted), but pod1/pod2 still in store.
				Sandboxes: []*sandboxv1alpha1.BatchSandbox{},
			},
			poolAlloc:     &PoolAllocation{PodAllocation: map[string]string{"pod1": "sbx-orphan", "pod2": "sbx-orphan"}},
			sandboxAllocs: map[string]*SandboxAllocation{},
			releases:      map[string]*AllocationRelease{},
			released:      map[string]*AllocationReleased{},
			wantAction: &algorithm.AllocAction{
				ToAllocate:    map[string][]string{},
				ToRelease:     map[string][]string{"sbx-orphan": {"pod1", "pod2"}},
				PodSupplement: 0,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			allocator, store, syncer := newTestAllocator(ctrl)

			store.EXPECT().Recover(gomock.Any(), gomock.Any()).Return(nil).Times(1)
			store.EXPECT().GetAllocation(gomock.Any(), gomock.Any()).Return(tt.poolAlloc, nil).Times(1)

			for _, sbx := range tt.spec.Sandboxes {
				syncer.EXPECT().GetAllocation(gomock.Any(), sbx).Return(tt.sandboxAllocs[sbx.Name], nil).Times(1)
				syncer.EXPECT().GetReleased(gomock.Any(), sbx).Return(tt.released[sbx.Name], nil).Times(1)
				// Terminating sandboxes skip GetRelease; only active sandboxes need it.
				if sbx.DeletionTimestamp.IsZero() {
					syncer.EXPECT().GetRelease(gomock.Any(), sbx).Return(tt.releases[sbx.Name], nil).Times(1)
				}
			}

			action, err := allocator.Schedule(context.Background(), tt.spec)
			assert.NoError(t, err)
			assert.Equal(t, tt.wantAction.ToAllocate, action.ToAllocate)
			assert.Equal(t, tt.wantAction.PodSupplement, action.PodSupplement)
			// ToRelease values may be in any order when built from a map (e.g. orphan sandbox GC).
			assert.Equal(t, len(tt.wantAction.ToRelease), len(action.ToRelease))
			for sandboxName, wantPods := range tt.wantAction.ToRelease {
				assert.ElementsMatch(t, wantPods, action.ToRelease[sandboxName])
			}
		})
	}
}

// --- GetPoolAllocation ---

func TestGetPoolAllocation(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	allocator, store, _ := newTestAllocator(ctrl)
	pool := &sandboxv1alpha1.Pool{ObjectMeta: metav1.ObjectMeta{Name: "pool1"}}

	store.EXPECT().GetAllocation(gomock.Any(), pool).Return(&PoolAllocation{
		PodAllocation: map[string]string{"pod1": "sbx1"},
	}, nil).Times(1)

	alloc, err := allocator.GetPoolAllocation(context.Background(), pool)
	assert.NoError(t, err)
	assert.Equal(t, map[string]string{"pod1": "sbx1"}, alloc)
}

func TestGetPoolAllocation_Empty(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	allocator, store, _ := newTestAllocator(ctrl)
	pool := &sandboxv1alpha1.Pool{ObjectMeta: metav1.ObjectMeta{Name: "pool1"}}

	store.EXPECT().GetAllocation(gomock.Any(), pool).Return(nil, nil).Times(1)

	alloc, err := allocator.GetPoolAllocation(context.Background(), pool)
	assert.NoError(t, err)
	assert.Equal(t, map[string]string{}, alloc)
}

// --- ClearPoolAllocation ---

func TestClearPoolAllocation(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	allocator, store, _ := newTestAllocator(ctrl)

	store.EXPECT().ClearAllocation(gomock.Any(), "ns1", "pool1").Return(nil).Times(1)

	err := allocator.ClearPoolAllocation(context.Background(), "ns1", "pool1")
	assert.NoError(t, err)
}

// --- annoAllocationSyncer: finalizer behavior ---

// newTestSyncer creates an annoAllocationSyncer backed by a fake k8s client
// with the given sandbox pre-created.
func newTestSyncer(sandbox *sandboxv1alpha1.BatchSandbox) (*annoAllocationSyncer, *sandboxv1alpha1.BatchSandbox) {
	scheme := runtime.NewScheme()
	_ = sandboxv1alpha1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sandbox).Build()
	return &annoAllocationSyncer{client: fakeClient}, sandbox
}

func TestSetAllocation_AddsFinalizer(t *testing.T) {
	sandbox := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx1", Namespace: "default"},
		Spec:       sandboxv1alpha1.BatchSandboxSpec{PoolRef: "pool1"},
	}
	syncer, sbx := newTestSyncer(sandbox)

	err := syncer.SetAllocation(context.Background(), sbx, &SandboxAllocation{Pods: []string{"pod1"}})
	assert.NoError(t, err)
	assert.Contains(t, sbx.Finalizers, FinalizerPoolAllocation)
}

func TestSetReleased_FinalizerBehavior(t *testing.T) {
	now := metav1.NewTime(time.Now())

	tests := []struct {
		name              string
		allocated         []string
		released          []string
		deletionTimestamp *metav1.Time
		finalizersBefore  []string
		wantFinalizer     bool
	}{
		{
			name:             "not deleting - finalizer kept regardless of release coverage",
			allocated:        []string{"pod1"},
			released:         []string{"pod1"},
			finalizersBefore: []string{FinalizerPoolAllocation},
			wantFinalizer:    true,
		},
		{
			name:              "deleting - partial release - finalizer kept",
			allocated:         []string{"pod1", "pod2"},
			released:          []string{"pod1"},
			deletionTimestamp: &now,
			finalizersBefore:  []string{FinalizerPoolAllocation},
			wantFinalizer:     true,
		},
		{
			name:              "deleting - all pods released - finalizer removed",
			allocated:         []string{"pod1", "pod2"},
			released:          []string{"pod1", "pod2"},
			deletionTimestamp: &now,
			finalizersBefore:  []string{FinalizerPoolAllocation},
			wantFinalizer:     false,
		},
		{
			name:              "deleting - no pods allocated - finalizer removed",
			allocated:         []string{},
			released:          []string{},
			deletionTimestamp: &now,
			finalizersBefore:  []string{FinalizerPoolAllocation},
			wantFinalizer:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			allocJSON, _ := marshalJSON(&SandboxAllocation{Pods: tt.allocated})
			sandbox := &sandboxv1alpha1.BatchSandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "sbx1",
					Namespace:         "default",
					Finalizers:        tt.finalizersBefore,
					DeletionTimestamp: tt.deletionTimestamp,
					Annotations: map[string]string{
						AnnoAllocStatusKey: allocJSON,
					},
				},
			}
			syncer, sbx := newTestSyncer(sandbox)

			err := syncer.SetReleased(context.Background(), sbx, &AllocationReleased{Pods: tt.released})
			assert.NoError(t, err)

			if tt.wantFinalizer {
				assert.Contains(t, sbx.Finalizers, FinalizerPoolAllocation, "finalizer should be kept")
			} else {
				assert.NotContains(t, sbx.Finalizers, FinalizerPoolAllocation, "finalizer should be removed")
			}
		})
	}
}

// marshalJSON is a test helper that marshals v to a JSON string.
func marshalJSON(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}

func TestSyncSandboxAllocation_Success(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	allocator, store, syncer := newTestAllocator(ctrl)
	sandbox := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx1", Namespace: "ns1"},
		Spec:       sandboxv1alpha1.BatchSandboxSpec{PoolRef: "pool1"},
	}
	newPods := []string{"pod1", "pod2"}

	// Phase 1: snapshot old state, then update memory.
	syncer.EXPECT().GetAllocation(gomock.Any(), sandbox).Return(&SandboxAllocation{Pods: []string{}}, nil).Times(1)
	store.EXPECT().UpdateAllocation(gomock.Any(), "ns1", "pool1", "sbx1", newPods).Times(1)
	// Phase 2: persist to annotation.
	syncer.EXPECT().SetAllocation(gomock.Any(), sandbox, gomock.Any()).DoAndReturn(
		func(ctx context.Context, sbx *sandboxv1alpha1.BatchSandbox, alloc *SandboxAllocation) error {
			assert.Equal(t, newPods, alloc.Pods)
			return nil
		}).Times(1)

	err := allocator.SyncSandboxAllocation(context.Background(), sandbox, newPods)
	assert.NoError(t, err)
}

func TestSyncSandboxAllocation_GetFailed(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	allocator, _, syncer := newTestAllocator(ctrl)
	sandbox := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx1", Namespace: "ns1"},
		Spec:       sandboxv1alpha1.BatchSandboxSpec{PoolRef: "pool1"},
	}

	// GetAllocation fails: should return early without touching store or SetAllocation.
	syncer.EXPECT().GetAllocation(gomock.Any(), sandbox).Return(nil, assert.AnError).Times(1)

	err := allocator.SyncSandboxAllocation(context.Background(), sandbox, []string{"pod1"})
	assert.Error(t, err)
}

func TestSyncSandboxAllocation_SetFailed_Rollback(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	allocator, store, syncer := newTestAllocator(ctrl)
	sandbox := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx1", Namespace: "ns1"},
		Spec:       sandboxv1alpha1.BatchSandboxSpec{PoolRef: "pool1"},
	}
	oldPods := []string{"pod-old"}
	newPods := []string{"pod1"}

	syncer.EXPECT().GetAllocation(gomock.Any(), sandbox).Return(&SandboxAllocation{Pods: oldPods}, nil).Times(1)
	// Phase 1: optimistic memory update.
	store.EXPECT().UpdateAllocation(gomock.Any(), "ns1", "pool1", "sbx1", newPods).Times(1)
	// Phase 2: annotation fails → rollback memory to old state.
	syncer.EXPECT().SetAllocation(gomock.Any(), sandbox, gomock.Any()).Return(assert.AnError).Times(1)
	store.EXPECT().UpdateAllocation(gomock.Any(), "ns1", "pool1", "sbx1", oldPods).Times(1)

	err := allocator.SyncSandboxAllocation(context.Background(), sandbox, newPods)
	assert.Error(t, err)
}

// --- SyncSandboxReleased ---

func TestSyncSandboxReleased_Success(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	allocator, store, syncer := newTestAllocator(ctrl)
	sandbox := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx1", Namespace: "ns1"},
		Spec:       sandboxv1alpha1.BatchSandboxSpec{PoolRef: "pool1"},
	}
	pods := []string{"pod1", "pod2"}

	// Phase 1: persist to annotation first to prevent premature re-allocation.
	syncer.EXPECT().SetReleased(gomock.Any(), sandbox, gomock.Any()).DoAndReturn(
		func(ctx context.Context, sbx *sandboxv1alpha1.BatchSandbox, released *AllocationReleased) error {
			assert.Equal(t, pods, released.Pods)
			return nil
		}).Times(1)
	// Phase 2: release from memory only after annotation is committed.
	store.EXPECT().ReleaseAllocation(gomock.Any(), "ns1", "pool1", pods).Times(1)

	err := allocator.SyncSandboxReleased(context.Background(), sandbox, pods)
	assert.NoError(t, err)
}

func TestSyncSandboxReleased_SetFailed(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	allocator, _, syncer := newTestAllocator(ctrl)
	sandbox := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx1", Namespace: "ns1"},
		Spec:       sandboxv1alpha1.BatchSandboxSpec{PoolRef: "pool1"},
	}
	pods := []string{"pod1"}

	syncer.EXPECT().SetReleased(gomock.Any(), sandbox, gomock.Any()).Return(assert.AnError).Times(1)
	// ReleaseAllocation must NOT be called when annotation sync fails:
	// pods remain "in use" in the memory store to prevent re-allocation.

	err := allocator.SyncSandboxReleased(context.Background(), sandbox, pods)
	assert.Error(t, err)
}
