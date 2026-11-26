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
	"encoding/json"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/controller/algorithm"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/controller/strategy"
)

// newTestAllocator creates an Allocator backed by mock store and syncer.
func newTestAllocator(ctrl *gomock.Controller) (Allocator, *MockallocationStore, *MockallocationSyncer) {
	store := NewMockallocationStore(ctrl)
	syncer := NewMockallocationSyncer(ctrl)
	return &defaultAllocator{store: store, syncer: syncer, algorithm: &algorithm.PackedSchedule{}}, store, syncer
}

// --- Schedule ---

func TestSchedule(t *testing.T) {
	replica1 := int32(1)
	replica2 := int32(2)

	tests := []struct {
		name          string
		spec          *allocSpec
		poolAlloc     *poolAllocation
		sandboxAllocs map[string]*sandboxAllocation
		releases      map[string]*allocationRelease
		released      map[string]*allocationReleased
		wantAction    *algorithm.AllocAction
	}{
		{
			name: "allocate normally - 2 pods for 2 sandboxes",
			spec: &allocSpec{
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
			poolAlloc:     &poolAllocation{PodAllocation: map[string]string{}},
			sandboxAllocs: map[string]*sandboxAllocation{"sbx1": {Pods: []string{}}, "sbx2": {Pods: []string{}}},
			releases:      map[string]*allocationRelease{"sbx1": {Pods: []string{}}, "sbx2": {Pods: []string{}}},
			released:      map[string]*allocationReleased{"sbx1": {Pods: []string{}}, "sbx2": {Pods: []string{}}},
			wantAction: &algorithm.AllocAction{
				ToAllocate:    map[string][]string{"sbx1": {"pod1"}, "sbx2": {"pod2"}},
				ToRelease:     map[string][]string{},
				PodSupplement: 0,
			},
		},
		{
			name: "skip non-running pods",
			spec: &allocSpec{
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
			poolAlloc:     &poolAllocation{PodAllocation: map[string]string{}},
			sandboxAllocs: map[string]*sandboxAllocation{"sbx1": {Pods: []string{}}, "sbx2": {Pods: []string{}}},
			releases:      map[string]*allocationRelease{"sbx1": {Pods: []string{}}, "sbx2": {Pods: []string{}}},
			released:      map[string]*allocationReleased{"sbx1": {Pods: []string{}}, "sbx2": {Pods: []string{}}},
			wantAction: &algorithm.AllocAction{
				ToAllocate:    map[string][]string{"sbx1": {"pod1"}},
				ToRelease:     map[string][]string{},
				PodSupplement: 1,
			},
		},
		{
			name: "partial allocated - allocate remaining",
			spec: &allocSpec{
				Pods: []*corev1.Pod{
					{ObjectMeta: metav1.ObjectMeta{Name: "pod1"}, Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}},
					{ObjectMeta: metav1.ObjectMeta{Name: "pod2"}, Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}},
				},
				Pool: &sandboxv1alpha1.Pool{ObjectMeta: metav1.ObjectMeta{Name: "pool1"}},
				Sandboxes: []*sandboxv1alpha1.BatchSandbox{
					{ObjectMeta: metav1.ObjectMeta{Name: "sbx1"}, Spec: sandboxv1alpha1.BatchSandboxSpec{Replicas: &replica2}},
				},
			},
			poolAlloc:     &poolAllocation{PodAllocation: map[string]string{"pod1": "sbx1"}},
			sandboxAllocs: map[string]*sandboxAllocation{"sbx1": {Pods: []string{"pod1"}}},
			releases:      map[string]*allocationRelease{"sbx1": {Pods: []string{}}},
			released:      map[string]*allocationReleased{"sbx1": {Pods: []string{}}},
			wantAction: &algorithm.AllocAction{
				ToAllocate:    map[string][]string{"sbx1": {"pod2"}},
				ToRelease:     map[string][]string{},
				PodSupplement: 0,
			},
		},
		{
			name: "with release - pods to release",
			spec: &allocSpec{
				Pods: []*corev1.Pod{
					{ObjectMeta: metav1.ObjectMeta{Name: "pod1"}, Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}},
				},
				Pool: &sandboxv1alpha1.Pool{ObjectMeta: metav1.ObjectMeta{Name: "pool1"}},
				Sandboxes: []*sandboxv1alpha1.BatchSandbox{
					{ObjectMeta: metav1.ObjectMeta{Name: "sbx1"}, Spec: sandboxv1alpha1.BatchSandboxSpec{Replicas: &replica1}},
				},
			},
			poolAlloc:     &poolAllocation{PodAllocation: map[string]string{"pod1": "sbx1"}},
			sandboxAllocs: map[string]*sandboxAllocation{"sbx1": {Pods: []string{"pod1"}}},
			releases:      map[string]*allocationRelease{"sbx1": {Pods: []string{"pod1"}}},
			released:      map[string]*allocationReleased{"sbx1": {Pods: []string{}}},
			wantAction: &algorithm.AllocAction{
				ToAllocate:    map[string][]string{},
				ToRelease:     map[string][]string{"sbx1": {"pod1"}},
				PodSupplement: 0,
			},
		},
		{
			name: "partial release - only unreleased pods in ToRelease",
			spec: &allocSpec{
				Pods: []*corev1.Pod{
					{ObjectMeta: metav1.ObjectMeta{Name: "pod1"}, Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}},
					{ObjectMeta: metav1.ObjectMeta{Name: "pod2"}, Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}},
				},
				Pool: &sandboxv1alpha1.Pool{ObjectMeta: metav1.ObjectMeta{Name: "pool1"}},
				Sandboxes: []*sandboxv1alpha1.BatchSandbox{
					{ObjectMeta: metav1.ObjectMeta{Name: "sbx1"}, Spec: sandboxv1alpha1.BatchSandboxSpec{Replicas: &replica2}},
				},
			},
			poolAlloc:     &poolAllocation{PodAllocation: map[string]string{"pod1": "sbx1", "pod2": "sbx1"}},
			sandboxAllocs: map[string]*sandboxAllocation{"sbx1": {Pods: []string{"pod1", "pod2"}}},
			releases:      map[string]*allocationRelease{"sbx1": {Pods: []string{"pod1", "pod2"}}},
			released:      map[string]*allocationReleased{"sbx1": {Pods: []string{"pod1"}}},
			wantAction: &algorithm.AllocAction{
				ToAllocate:    map[string][]string{},
				ToRelease:     map[string][]string{"sbx1": {"pod2"}},
				PodSupplement: 0,
			},
		},
		{
			name: "not enough pods - PodSupplement > 0",
			spec: &allocSpec{
				Pods: []*corev1.Pod{
					{ObjectMeta: metav1.ObjectMeta{Name: "pod1"}, Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}},
				},
				Pool: &sandboxv1alpha1.Pool{ObjectMeta: metav1.ObjectMeta{Name: "pool1"}},
				Sandboxes: []*sandboxv1alpha1.BatchSandbox{
					{ObjectMeta: metav1.ObjectMeta{Name: "sbx1"}, Spec: sandboxv1alpha1.BatchSandboxSpec{Replicas: &replica2}},
				},
			},
			poolAlloc:     &poolAllocation{PodAllocation: map[string]string{}},
			sandboxAllocs: map[string]*sandboxAllocation{"sbx1": {Pods: []string{}}},
			releases:      map[string]*allocationRelease{"sbx1": {Pods: []string{}}},
			released:      map[string]*allocationReleased{"sbx1": {Pods: []string{}}},
			wantAction: &algorithm.AllocAction{
				ToAllocate:    map[string][]string{"sbx1": {"pod1"}},
				ToRelease:     map[string][]string{},
				PodSupplement: 1,
			},
		},
		{
			name: "skip already allocated pod - other sandbox gets supplement",
			spec: &allocSpec{
				Pods: []*corev1.Pod{
					{ObjectMeta: metav1.ObjectMeta{Name: "pod1"}, Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}},
				},
				Pool: &sandboxv1alpha1.Pool{ObjectMeta: metav1.ObjectMeta{Name: "pool1"}},
				Sandboxes: []*sandboxv1alpha1.BatchSandbox{
					{ObjectMeta: metav1.ObjectMeta{Name: "sbx1"}, Spec: sandboxv1alpha1.BatchSandboxSpec{Replicas: &replica1}},
					{ObjectMeta: metav1.ObjectMeta{Name: "sbx2"}, Spec: sandboxv1alpha1.BatchSandboxSpec{Replicas: &replica1}},
				},
			},
			poolAlloc:     &poolAllocation{PodAllocation: map[string]string{"pod1": "sbx1"}},
			sandboxAllocs: map[string]*sandboxAllocation{"sbx1": {Pods: []string{"pod1"}}, "sbx2": {Pods: []string{}}},
			releases:      map[string]*allocationRelease{"sbx1": {Pods: []string{}}, "sbx2": {Pods: []string{}}},
			released:      map[string]*allocationReleased{"sbx1": {Pods: []string{}}, "sbx2": {Pods: []string{}}},
			wantAction: &algorithm.AllocAction{
				ToAllocate:    map[string][]string{},
				ToRelease:     map[string][]string{},
				PodSupplement: 1,
			},
		},
		{
			name: "terminating sandbox - queue unreleased pods for release, no supplement",
			spec: &allocSpec{
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
			poolAlloc:     &poolAllocation{PodAllocation: map[string]string{"pod1": "sbx1", "pod2": "sbx1"}},
			sandboxAllocs: map[string]*sandboxAllocation{"sbx1": {Pods: []string{"pod1", "pod2"}}},
			releases:      map[string]*allocationRelease{},
			released:      map[string]*allocationReleased{"sbx1": {Pods: []string{"pod1"}}},
			wantAction: &algorithm.AllocAction{
				ToAllocate:    map[string][]string{},
				ToRelease:     map[string][]string{"sbx1": {"pod2"}},
				PodSupplement: 0,
			},
		},
		{
			name: "orphan sandbox - pods in store but sandbox no longer in spec",
			spec: &allocSpec{
				Pods: []*corev1.Pod{
					{ObjectMeta: metav1.ObjectMeta{Name: "pod1"}, Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}},
					{ObjectMeta: metav1.ObjectMeta{Name: "pod2"}, Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}},
				},
				Pool: &sandboxv1alpha1.Pool{ObjectMeta: metav1.ObjectMeta{Name: "pool1"}},
				// sbx-orphan is not in spec (e.g. force-deleted), but pod1/pod2 still in store.
				Sandboxes: []*sandboxv1alpha1.BatchSandbox{},
			},
			poolAlloc:     &poolAllocation{PodAllocation: map[string]string{"pod1": "sbx-orphan", "pod2": "sbx-orphan"}},
			sandboxAllocs: map[string]*sandboxAllocation{},
			releases:      map[string]*allocationRelease{},
			released:      map[string]*allocationReleased{},
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
			store.EXPECT().SetAllocation(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).Times(1)

			for _, sbx := range tt.spec.Sandboxes {
				syncer.EXPECT().GetAllocation(gomock.Any(), sbx).Return(tt.sandboxAllocs[sbx.Name], nil).Times(2)
				syncer.EXPECT().GetReleased(gomock.Any(), sbx).Return(tt.released[sbx.Name], nil).Times(1)
				// Terminating sandboxes skip GetRelease; only active sandboxes need it.
				if sbx.DeletionTimestamp.IsZero() {
					syncer.EXPECT().GetRelease(gomock.Any(), sbx).Return(tt.releases[sbx.Name], nil).Times(1)
				}
			}

			// Recovery is mocked here; startup cache confirmation has its own regression.
			allocator.(*defaultAllocator).taskAllocationState(tt.spec.Pool.Namespace, tt.spec.Pool.Name).sandboxes = map[string]types.UID{}
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

func TestGetPoolAllocation_Empty(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	allocator, store, _ := newTestAllocator(ctrl)
	pool := &sandboxv1alpha1.Pool{ObjectMeta: metav1.ObjectMeta{Name: "pool1"}}

	// The store is recovered before it is first read.
	store.EXPECT().Recover(gomock.Any(), gomock.Any()).Return(nil).Times(1)
	store.EXPECT().GetAllocation(gomock.Any(), pool).Return(nil, nil).Times(1)

	alloc, err := allocator.GetPoolAllocation(context.Background(), pool)
	assert.NoError(t, err)
	assert.Equal(t, map[string]string{}, alloc)
}

// --- annoAllocationSyncer: finalizer behavior ---

// newTestSyncer creates an annoAllocationSyncer backed by a fake k8s client
// with the given sandbox pre-created.
func newTestSyncer(sandbox *sandboxv1alpha1.BatchSandbox) (*annoAllocationSyncer, *sandboxv1alpha1.BatchSandbox) {
	scheme := runtime.NewScheme()
	_ = sandboxv1alpha1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sandbox).Build()
	return &annoAllocationSyncer{client: fakeClient, apiReader: fakeClient}, sandbox
}

func TestSetAllocation_AddsFinalizer(t *testing.T) {
	sandbox := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx1", Namespace: "default", Generation: 7},
		Spec:       sandboxv1alpha1.BatchSandboxSpec{PoolRef: "pool1"},
	}
	syncer, sbx := newTestSyncer(sandbox)

	err := syncer.SetAllocation(context.Background(), sbx, &sandboxAllocation{Pods: []string{"pod1"}})
	assert.NoError(t, err)
	assert.Contains(t, sbx.Finalizers, finalizerPoolAllocation)

	allocation, err := syncer.GetAllocation(context.Background(), sbx)
	assert.NoError(t, err)
	assert.Equal(t, []string{"pod1"}, allocation.Pods)
	assert.Equal(t, "pool1", allocation.PoolRef)
	assert.Equal(t, int64(7), allocation.Generation)
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
			finalizersBefore: []string{finalizerPoolAllocation},
			wantFinalizer:    true,
		},
		{
			name:              "deleting - partial release - finalizer kept",
			allocated:         []string{"pod1", "pod2"},
			released:          []string{"pod1"},
			deletionTimestamp: &now,
			finalizersBefore:  []string{finalizerPoolAllocation},
			wantFinalizer:     true,
		},
		{
			name:              "deleting - all pods released - finalizer removed",
			allocated:         []string{"pod1", "pod2"},
			released:          []string{"pod1", "pod2"},
			deletionTimestamp: &now,
			finalizersBefore:  []string{finalizerPoolAllocation},
			wantFinalizer:     false,
		},
		{
			name:              "deleting - no pods allocated - finalizer removed",
			allocated:         []string{},
			released:          []string{},
			deletionTimestamp: &now,
			finalizersBefore:  []string{finalizerPoolAllocation},
			wantFinalizer:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			allocJSON, _ := marshalJSON(&sandboxAllocation{Pods: tt.allocated})
			sandbox := &sandboxv1alpha1.BatchSandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "sbx1",
					Namespace:         "default",
					Finalizers:        tt.finalizersBefore,
					DeletionTimestamp: tt.deletionTimestamp,
					Annotations: map[string]string{
						annoAllocStatusKey: allocJSON,
					},
				},
			}
			syncer, sbx := newTestSyncer(sandbox)

			applied, err := syncer.SetReleased(context.Background(), sbx, &allocationReleased{Pods: tt.released})
			assert.NoError(t, err)
			assert.True(t, applied)

			if tt.wantFinalizer {
				assert.Contains(t, sbx.Finalizers, finalizerPoolAllocation, "finalizer should be kept")
			} else {
				assert.NotContains(t, sbx.Finalizers, finalizerPoolAllocation, "finalizer should be removed")
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
	syncer.EXPECT().GetAllocation(gomock.Any(), sandbox).Return(&sandboxAllocation{Pods: []string{}}, nil).Times(1)
	syncer.EXPECT().GetReleased(gomock.Any(), sandbox).Return(&allocationReleased{}, nil)
	store.EXPECT().UpdateAllocation(gomock.Any(), "ns1", "pool1", "sbx1", newPods).Times(1)
	// Phase 2: persist to annotation.
	syncer.EXPECT().SetAllocation(gomock.Any(), sandbox, gomock.Any()).DoAndReturn(
		func(ctx context.Context, sbx *sandboxv1alpha1.BatchSandbox, alloc *sandboxAllocation) error {
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

	syncer.EXPECT().GetAllocation(gomock.Any(), sandbox).Return(&sandboxAllocation{Pods: oldPods}, nil).Times(1)
	syncer.EXPECT().GetReleased(gomock.Any(), sandbox).Return(&allocationReleased{}, nil)
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
		func(ctx context.Context, sbx *sandboxv1alpha1.BatchSandbox, released *allocationReleased) (bool, error) {
			assert.Equal(t, pods, released.Pods)
			return true, nil
		}).Times(1)
	// Phase 2: release from memory only after annotation is committed.
	store.EXPECT().ReleaseAllocation(gomock.Any(), "ns1", "pool1", "sbx1", pods).Times(1)

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

	syncer.EXPECT().SetReleased(gomock.Any(), sandbox, gomock.Any()).Return(false, assert.AnError).Times(1)
	// ReleaseAllocation must NOT be called when annotation sync fails:
	// pods remain "in use" in the memory store to prevent re-allocation.

	err := allocator.SyncSandboxReleased(context.Background(), sandbox, pods)
	assert.Error(t, err)
}

func TestTerminatingTaskAllocationHandoff(t *testing.T) {
	now := metav1.Now()
	replicas := int32(5)
	for _, tc := range []struct {
		name     string
		cleaning bool
		release  string
		want     []string
	}{
		{name: "hooks pending", cleaning: true},
		{name: "explicit completed task", cleaning: true, release: `{"pods":["done","historical"]}`, want: []string{"done"}},
		{name: "cleanup complete", want: []string{"done", "running"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sandbox := &sandboxv1alpha1.BatchSandbox{
				ObjectMeta: metav1.ObjectMeta{Name: "sandbox", DeletionTimestamp: &now,
					Annotations: map[string]string{
						annoAllocStatusKey:   `{"pods":["done","running","historical"]}`,
						annoAllocReleasedKey: `{"pods":["historical"]}`,
						annoAllocReleaseKey:  tc.release,
					}},
				Spec: sandboxv1alpha1.BatchSandboxSpec{Replicas: &replicas},
			}
			if tc.cleaning {
				sandbox.Finalizers = []string{finalizerTaskCleanup}
			}
			c := fake.NewClientBuilder().WithScheme(testscheme).Build()
			allocator := NewDefaultAllocator(c, c)
			request, err := allocator.getSandboxRequest(context.Background(), sandbox, map[string]string{"done": sandbox.Name, "running": sandbox.Name})
			if !assert.NoError(t, err) {
				return
			}
			assert.Zero(t, request.PodSupplement)
			assert.ElementsMatch(t, tc.want, request.ToRelease)
		})
	}
}

func TestSyncSandboxReleasedPreservesNewOwner(t *testing.T) {
	for _, currentName := range []string{"current", "previous"} {
		t.Run(currentName, func(t *testing.T) {
			ctx := context.Background()
			pool := &sandboxv1alpha1.Pool{ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "default"}}
			previous := &sandboxv1alpha1.BatchSandbox{
				ObjectMeta: metav1.ObjectMeta{Name: "previous", Namespace: pool.Namespace, UID: "old-sandbox"},
				Spec:       sandboxv1alpha1.BatchSandboxSpec{PoolRef: pool.Name},
			}
			setSandboxAllocation(previous, sandboxAllocation{Pods: []string{"pod"}})
			previous.Annotations[annoAllocReleasedKey] = `{"pods":["pod"]}`
			stored := previous.DeepCopy()
			if currentName == previous.Name {
				stored.UID = "replacement-sandbox"
				delete(stored.Annotations, annoAllocReleasedKey)
			}
			c := fake.NewClientBuilder().WithScheme(testscheme).WithObjects(stored).Build()
			allocator := NewDefaultAllocator(c, c)
			allocator.store.UpdateAllocation(ctx, pool.Namespace, pool.Name, currentName, []string{"pod"})

			for range 2 {
				require.NoError(t, allocator.SyncSandboxReleased(ctx, previous, []string{"pod"}))
				allocation, err := allocator.store.GetAllocation(ctx, pool)
				require.NoError(t, err)
				require.Equal(t, map[string]string{"pod": currentName}, allocation.PodAllocation)
			}
		})
	}
}

func TestSyncSandboxReleasedRecreatedSandbox(t *testing.T) {
	for _, tc := range []struct {
		name            string
		retainPod       bool
		observeDeletion bool
	}{
		{name: "release stale reservation"},
		{name: "release after deletion round", observeDeletion: true},
		{name: "preserve replacement reservation", retainPod: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			retainPod := tc.retainPod
			ctx := context.Background()
			pool := &sandboxv1alpha1.Pool{ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "test"}}
			previous := &sandboxv1alpha1.BatchSandbox{
				ObjectMeta: metav1.ObjectMeta{Name: "sandbox", Namespace: pool.Namespace, UID: "previous"},
				Spec:       sandboxv1alpha1.BatchSandboxSpec{PoolRef: pool.Name, Replicas: ptr.To(int32(1))},
			}
			setSandboxAllocation(previous, sandboxAllocation{Pods: []string{"pod"}})
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: pool.Namespace},
				Status:     corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
			}
			c := fake.NewClientBuilder().WithScheme(testscheme).WithObjects(previous, pod).Build()
			allocator := NewDefaultAllocator(c, c)
			_, err := allocator.Schedule(ctx, &allocSpec{Pool: pool, Pods: []*corev1.Pod{pod}, Sandboxes: []*sandboxv1alpha1.BatchSandbox{previous}})
			require.NoError(t, err)
			require.NoError(t, c.Delete(ctx, previous))
			if tc.observeDeletion {
				action, err := allocator.Schedule(ctx, &allocSpec{Pool: pool, Pods: []*corev1.Pod{pod}})
				require.NoError(t, err)
				require.Equal(t, []string{"pod"}, action.ToRelease[previous.Name])
			}
			replacement := &sandboxv1alpha1.BatchSandbox{
				ObjectMeta: metav1.ObjectMeta{Name: previous.Name, Namespace: pool.Namespace, UID: "replacement"},
				Spec:       sandboxv1alpha1.BatchSandboxSpec{PoolRef: pool.Name, Replicas: ptr.To(int32(0))},
			}
			if retainPod {
				setSandboxAllocation(replacement, sandboxAllocation{Pods: []string{"pod"}})
			}
			require.NoError(t, c.Create(ctx, replacement))
			waiting := &sandboxv1alpha1.BatchSandbox{
				ObjectMeta: metav1.ObjectMeta{Name: "waiting", Namespace: pool.Namespace, UID: "waiting"},
				Spec:       sandboxv1alpha1.BatchSandboxSpec{PoolRef: pool.Name, Replicas: ptr.To(int32(1))},
			}
			require.NoError(t, c.Create(ctx, waiting))

			for attempt := range 2 {
				require.NoError(t, allocator.SyncSandboxReleased(ctx, previous, []string{"pod"}))
				require.Error(t, allocator.CheckSandboxAllocation(ctx, previous, []string{"pod"}), "the old UID must not retain task access")
				if retainPod && attempt > 0 {
					require.NoError(t, allocator.CheckSandboxAllocation(ctx, replacement, []string{"pod"}))
				}
				action, err := allocator.Schedule(ctx, &allocSpec{Pool: pool, Pods: []*corev1.Pod{pod}, Sandboxes: []*sandboxv1alpha1.BatchSandbox{replacement, waiting}})
				require.NoError(t, err)
				require.Empty(t, action.ToRelease, "stale handback must not recycle the replacement's Pod")
				if retainPod {
					require.Empty(t, action.ToAllocate)
					require.NoError(t, allocator.CheckSandboxAllocation(ctx, replacement, []string{"pod"}))
				} else {
					require.Equal(t, []string{"pod"}, action.ToAllocate[waiting.Name], "the stale reservation must not prevent reuse")
					require.Zero(t, action.PodSupplement)
				}
			}
			stored := &sandboxv1alpha1.BatchSandbox{}
			require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(replacement), stored))
			require.Empty(t, stored.Annotations[annoAllocReleasedKey], "old cleanup must not modify the replacement")
		})
	}
}

func TestSandboxAllocationReader(t *testing.T) {
	ctx := context.Background()
	pool := &sandboxv1alpha1.Pool{ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "test"}}
	sandbox := &sandboxv1alpha1.BatchSandbox{ObjectMeta: metav1.ObjectMeta{Name: "sandbox", Namespace: "test", UID: "current"}, Spec: sandboxv1alpha1.BatchSandboxSpec{PoolRef: pool.Name, Replicas: ptr.To(int32(1)), TaskTemplate: &sandboxv1alpha1.TaskTemplateSpec{}}}
	setSandboxAllocation(sandbox, sandboxAllocation{Pods: []string{"pod"}})
	c := fake.NewClientBuilder().WithScheme(testscheme).WithObjects(sandbox, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "test"}}).Build()
	allocator := NewDefaultAllocator(c, c)
	require.Error(t, allocator.CheckSandboxAllocation(ctx, sandbox, []string{"pod"}), "recovered names are not authorization until Pool scheduling confirms ownership")
	_, err := allocator.Schedule(ctx, &allocSpec{Pool: pool, Sandboxes: []*sandboxv1alpha1.BatchSandbox{sandbox}})
	require.NoError(t, err)
	noAPI := interceptor.NewClient(c, interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			t.Fatal("normal ownership checks must not List the API")
			return nil
		},
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			t.Fatal("normal ownership checks must not Get the API")
			return nil
		},
	})
	batch := &BatchSandboxReconciler{Client: c, APIReader: noAPI, AllocationReader: allocator}
	a := allocator
	a.client, a.apiReader = noAPI, noAPI
	for range 3 {
		require.NoError(t, allocator.CheckSandboxAllocation(ctx, sandbox, []string{"pod"}))
		_, err := batch.listPods(ctx, strategy.NewPoolStrategy(sandbox), sandbox)
		require.NoError(t, err)
	}
	_, err = allocator.Schedule(ctx, &allocSpec{Pool: pool, Sandboxes: []*sandboxv1alpha1.BatchSandbox{sandbox}})
	require.NoError(t, err, "confirmed Pool scheduling must not repeat the recovery List")
	a.client, a.apiReader = c, c
	require.Error(t, allocator.CheckSandboxAllocation(ctx, sandbox, []string{"foreign"}))
	stale := sandbox.DeepCopy()
	stale.UID = "previous"
	require.Error(t, allocator.CheckSandboxAllocation(ctx, stale, []string{"pod"}))
	require.NoError(t, allocator.SyncSandboxReleased(ctx, sandbox, []string{"pod"}))
	require.Error(t, allocator.CheckSandboxAllocation(ctx, sandbox, []string{"pod"}), "released ownership must invalidate a stale task view")
	require.NoError(t, allocator.ClearPoolAllocation(ctx, pool.Namespace, pool.Name))
	require.Error(t, allocator.CheckSandboxAllocation(ctx, sandbox, []string{"pod"}))
	require.NoError(t, c.Delete(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "test"}}))
	visible, err := batch.listPods(ctx, strategy.NewPoolStrategy(sandbox), sandbox)
	require.NoError(t, err, "missing Pods must not block cleanup while Pool ownership is unconfirmed")
	require.Empty(t, visible)
	// Recovery may have run before a new annotation claim arrived.
	setSandboxAllocation(sandbox, sandboxAllocation{Pods: []string{"replacement"}})
	require.NoError(t, c.Update(ctx, sandbox))
	_, err = allocator.Schedule(ctx, &allocSpec{Pool: pool, Sandboxes: []*sandboxv1alpha1.BatchSandbox{sandbox}})
	require.NoError(t, err)
	require.NoError(t, allocator.CheckSandboxAllocation(ctx, sandbox, []string{"replacement"}))
}

func TestColdAllocationUsesConfirmedHandback(t *testing.T) {
	ctx := context.Background()
	pool := &sandboxv1alpha1.Pool{ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "test"}}
	previous := &sandboxv1alpha1.BatchSandbox{ObjectMeta: metav1.ObjectMeta{Name: "previous", Namespace: "test"}, Spec: sandboxv1alpha1.BatchSandboxSpec{PoolRef: pool.Name, Replicas: ptr.To(int32(1))}}
	setSandboxAllocation(previous, sandboxAllocation{Pods: []string{"pod"}})
	c := fake.NewClientBuilder().WithScheme(testscheme).WithObjects(previous).Build()
	allocator := NewDefaultAllocator(c, c)
	_, err := allocator.GetPoolAllocation(ctx, pool)
	require.NoError(t, err)
	previous.Annotations[annoAllocReleasedKey] = `{"pods":["pod"]}`
	require.NoError(t, c.Update(ctx, previous))
	current := &sandboxv1alpha1.BatchSandbox{ObjectMeta: metav1.ObjectMeta{Name: "current", Namespace: "test"}, Spec: previous.Spec}
	setSandboxAllocation(current, sandboxAllocation{Pods: []string{"pod"}})
	require.NoError(t, c.Create(ctx, current))
	_, err = allocator.Schedule(ctx, &allocSpec{Pool: pool, Sandboxes: []*sandboxv1alpha1.BatchSandbox{previous, current}})
	require.NoError(t, err)
	owners, err := allocator.GetPoolAllocation(ctx, pool)
	require.NoError(t, err)
	require.Equal(t, "current", owners["pod"])
	require.NoError(t, allocator.CheckSandboxAllocation(ctx, current, []string{"pod"}))
}

func TestHandbackRequiresCurrentAllocation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		allocated bool
		owner     string
		release   bool
	}{
		{name: "stale release without a reservation"},
		{name: "stale owner after handback", allocated: true},
		{name: "reservation without allocation reference", owner: "sandbox"},
		{name: "current owner", allocated: true, owner: "sandbox", release: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sandbox := &sandboxv1alpha1.BatchSandbox{ObjectMeta: metav1.ObjectMeta{Name: "sandbox", Annotations: map[string]string{annoAllocReleaseKey: `{"pods":["pod"]}`}}}
			if tc.allocated {
				setSandboxAllocation(sandbox, sandboxAllocation{Pods: []string{"pod"}})
			}
			c := fake.NewClientBuilder().WithScheme(testscheme).Build()
			owners := map[string]string{}
			if tc.owner != "" {
				owners["pod"] = tc.owner
			}
			request, err := NewDefaultAllocator(c, c).getSandboxRequest(context.Background(), sandbox, owners)
			require.NoError(t, err)
			if tc.release {
				require.Equal(t, []string{"pod"}, request.ToRelease)
			} else {
				require.Empty(t, request.ToRelease)
			}
		})
	}
}

func TestSandboxAllocationFailureIsolation(t *testing.T) {
	ctx := context.Background()
	pool := &sandboxv1alpha1.Pool{ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "test"}}
	sandboxes := make([]*sandboxv1alpha1.BatchSandbox, 0, 3)
	objects := []client.Object{}
	for _, name := range []string{"owner", "contender", "unrelated"} {
		sandbox := &sandboxv1alpha1.BatchSandbox{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "test", UID: types.UID(name)}, Spec: sandboxv1alpha1.BatchSandboxSpec{PoolRef: pool.Name, Replicas: ptr.To(int32(1)), TaskTemplate: &sandboxv1alpha1.TaskTemplateSpec{}}}
		if name != "contender" {
			setSandboxAllocation(sandbox, sandboxAllocation{Pods: []string{name + "-pod"}})
		}
		sandboxes = append(sandboxes, sandbox)
		objects = append(objects, sandbox)
	}
	c := fake.NewClientBuilder().WithScheme(testscheme).WithObjects(objects...).Build()
	apiReader := interceptor.NewClient(c, interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if _, ok := obj.(*corev1.Pod); ok {
			return assert.AnError
		}
		return c.Get(ctx, key, obj, opts...)
	}})
	allocator := NewDefaultAllocator(c, apiReader)
	spec := &allocSpec{Pool: pool, Sandboxes: sandboxes}
	_, err := allocator.Schedule(ctx, spec)
	require.NoError(t, err)
	owner, contender, unrelated := sandboxes[0], sandboxes[1], sandboxes[2]
	contender.Annotations = map[string]string{annoAllocStatusKey: "invalid-json"}
	require.NoError(t, c.Update(ctx, contender))
	_, err = allocator.Schedule(ctx, spec)
	require.Error(t, err)
	require.NoError(t, allocator.CheckSandboxAllocation(ctx, owner, []string{"owner-pod"}))
	require.NoError(t, allocator.CheckSandboxAllocation(ctx, unrelated, []string{"unrelated-pod"}))
	setSandboxAllocation(contender, sandboxAllocation{Pods: []string{"owner-pod"}})
	require.NoError(t, c.Update(ctx, contender))
	_, err = allocator.Schedule(ctx, spec)
	require.Error(t, err, "unavailable executor must leave the conflicting claims blocked")
	require.Error(t, allocator.CheckSandboxAllocation(ctx, owner, []string{"owner-pod"}))
	require.Error(t, allocator.CheckSandboxAllocation(ctx, contender, []string{"owner-pod"}))
	require.NoError(t, allocator.CheckSandboxAllocation(ctx, unrelated, []string{"unrelated-pod"}))
	setSandboxAllocation(contender, sandboxAllocation{})
	require.NoError(t, c.Update(ctx, contender))
	_, err = allocator.Schedule(ctx, spec)
	require.NoError(t, err)
	require.NoError(t, allocator.CheckSandboxAllocation(ctx, owner, []string{"owner-pod"}))
	require.NoError(t, allocator.CheckSandboxAllocation(ctx, unrelated, []string{"unrelated-pod"}))
}
