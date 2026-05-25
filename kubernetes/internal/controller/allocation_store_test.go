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
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
)

func TestInMemoryAllocationStore_GetAllocation_Empty(t *testing.T) {
	store := NewInMemoryAllocationStore()
	ctx := context.Background()

	pool := &sandboxv1alpha1.Pool{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pool"},
	}

	alloc, err := store.GetAllocation(ctx, pool)
	assert.NoError(t, err)
	assert.NotNil(t, alloc)
	assert.Empty(t, alloc.PodAllocation)
}

func TestInMemoryAllocationStore_SetAndGetAllocation(t *testing.T) {
	store := NewInMemoryAllocationStore()
	ctx := context.Background()

	pool := &sandboxv1alpha1.Pool{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pool"},
	}

	alloc := &PoolAllocation{
		PodAllocation: map[string]string{
			"pod1": "sandbox1",
			"pod2": "sandbox2",
		},
	}

	err := store.SetAllocation(ctx, pool, alloc)
	assert.NoError(t, err)

	alloc2, err := store.GetAllocation(ctx, pool)
	assert.NoError(t, err)
	assert.Equal(t, alloc.PodAllocation, alloc2.PodAllocation)
}

func TestInMemoryAllocationStore_UpdateAllocation_AddPods(t *testing.T) {
	store := NewInMemoryAllocationStore().(*InMemoryAllocationStore)
	ctx := context.Background()

	store.UpdateAllocation(ctx, "default", "pool1", "sandbox1", []string{"pod1", "pod2"})

	assert.Equal(t, "sandbox1", store.pools["default/pool1"].data["pod1"])
	assert.Equal(t, "sandbox1", store.pools["default/pool1"].data["pod2"])
}

func TestInMemoryAllocationStore_UpdateAllocation_ReplacePods(t *testing.T) {
	store := NewInMemoryAllocationStore().(*InMemoryAllocationStore)
	ctx := context.Background()

	store.UpdateAllocation(ctx, "default", "pool1", "sandbox1", []string{"pod1", "pod2"})
	store.UpdateAllocation(ctx, "default", "pool1", "sandbox1", []string{"pod3", "pod4"})

	_, exists1 := store.pools["default/pool1"].data["pod1"]
	_, exists2 := store.pools["default/pool1"].data["pod2"]
	assert.False(t, exists1, "old pod1 should be removed")
	assert.False(t, exists2, "old pod2 should be removed")

	assert.Equal(t, "sandbox1", store.pools["default/pool1"].data["pod3"])
	assert.Equal(t, "sandbox1", store.pools["default/pool1"].data["pod4"])
}

func TestInMemoryAllocationStore_UpdateAllocation_MultipleSandboxes(t *testing.T) {
	store := NewInMemoryAllocationStore().(*InMemoryAllocationStore)
	ctx := context.Background()

	store.UpdateAllocation(ctx, "default", "pool1", "sandbox1", []string{"pod1", "pod2"})
	store.UpdateAllocation(ctx, "default", "pool1", "sandbox2", []string{"pod3", "pod4"})

	assert.Equal(t, "sandbox1", store.pools["default/pool1"].data["pod1"])
	assert.Equal(t, "sandbox1", store.pools["default/pool1"].data["pod2"])
	assert.Equal(t, "sandbox2", store.pools["default/pool1"].data["pod3"])
	assert.Equal(t, "sandbox2", store.pools["default/pool1"].data["pod4"])
}

func TestInMemoryAllocationStore_UpdateAllocation_MultiplePools(t *testing.T) {
	store := NewInMemoryAllocationStore().(*InMemoryAllocationStore)
	ctx := context.Background()

	store.UpdateAllocation(ctx, "default", "pool1", "sandbox1", []string{"pod1", "pod2"})
	store.UpdateAllocation(ctx, "default", "pool2", "sandbox2", []string{"pod3", "pod4"})

	assert.Equal(t, "sandbox1", store.pools["default/pool1"].data["pod1"])
	assert.Equal(t, "sandbox2", store.pools["default/pool2"].data["pod3"])

	_, exists := store.pools["default/pool1"].data["pod3"]
	assert.False(t, exists, "pool1 should not see pool2's pods")
}

func TestInMemoryAllocationStore_GetAllocation_IsolatedByPool(t *testing.T) {
	store := NewInMemoryAllocationStore()
	ctx := context.Background()

	pool1 := &sandboxv1alpha1.Pool{ObjectMeta: metav1.ObjectMeta{Name: "pool1"}}
	pool2 := &sandboxv1alpha1.Pool{ObjectMeta: metav1.ObjectMeta{Name: "pool2"}}

	_ = store.SetAllocation(ctx, pool1, &PoolAllocation{
		PodAllocation: map[string]string{"pod1": "sandbox1"},
	})
	_ = store.SetAllocation(ctx, pool2, &PoolAllocation{
		PodAllocation: map[string]string{"pod2": "sandbox2"},
	})

	alloc1, _ := store.GetAllocation(ctx, pool1)
	alloc2, _ := store.GetAllocation(ctx, pool2)

	assert.Equal(t, "sandbox1", alloc1.PodAllocation["pod1"])
	assert.Empty(t, alloc1.PodAllocation["pod2"])

	assert.Equal(t, "sandbox2", alloc2.PodAllocation["pod2"])
	assert.Empty(t, alloc2.PodAllocation["pod1"])
}

func TestInMemoryAllocationStore_Recover(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = sandboxv1alpha1.AddToScheme(scheme)

	allocation1 := &SandboxAllocation{Pods: []string{"pod1", "pod2"}}
	allocation2 := &SandboxAllocation{Pods: []string{"pod3", "pod4"}}
	// released (alloc-released) means recycle is complete; Recover should exclude these pods.
	released2 := &AllocationReleased{Pods: []string{"pod4"}}

	alloc1JSON, _ := json.Marshal(allocation1)
	alloc2JSON, _ := json.Marshal(allocation2)
	released2JSON, _ := json.Marshal(released2)

	sandbox1 := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sandbox1",
			Namespace: "default",
			Annotations: map[string]string{
				AnnoAllocStatusKey: string(alloc1JSON),
			},
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			PoolRef: "pool1",
		},
	}

	sandbox2 := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sandbox2",
			Namespace: "default",
			Annotations: map[string]string{
				AnnoAllocStatusKey:   string(alloc2JSON),
				AnnoAllocReleasedKey: string(released2JSON),
			},
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			PoolRef: "pool1",
		},
	}

	sandbox3 := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sandbox3",
			Namespace: "default",
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			PoolRef: "pool2",
		},
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(sandbox1, sandbox2, sandbox3).
		Build()

	store := NewInMemoryAllocationStore().(*InMemoryAllocationStore)
	ctx := context.Background()

	err := store.Recover(ctx, client)
	assert.NoError(t, err)

	assert.Equal(t, "sandbox1", store.pools["default/pool1"].data["pod1"])
	assert.Equal(t, "sandbox1", store.pools["default/pool1"].data["pod2"])
	assert.Equal(t, "sandbox2", store.pools["default/pool1"].data["pod3"])
	// pod4 is in alloc-released (completed recycle), so it should be excluded from the pool.
	assert.Equal(t, "", store.pools["default/pool1"].data["pod4"])

	assert.Equal(t, 0, len(store.pools["default/pool2"].data), "pool2 should have no allocations")
}

func TestInMemoryAllocationStore_Recover_ReleaseOnlyOwnPods(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = sandboxv1alpha1.AddToScheme(scheme)

	allocation1 := &SandboxAllocation{Pods: []string{"pod1"}}
	// sandbox1 has completed recycling pod1 (alloc-released), so pod1 should be freed.
	released1 := &AllocationReleased{Pods: []string{"pod1"}}
	allocation2 := &SandboxAllocation{Pods: []string{"pod1"}}

	alloc1JSON, _ := json.Marshal(allocation1)
	released1JSON, _ := json.Marshal(released1)
	alloc2JSON, _ := json.Marshal(allocation2)

	sandbox1 := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sandbox1",
			Namespace: "default",
			Annotations: map[string]string{
				AnnoAllocStatusKey:   string(alloc1JSON),
				AnnoAllocReleasedKey: string(released1JSON),
			},
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			PoolRef: "pool1",
		},
	}

	sandbox2 := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sandbox2",
			Namespace: "default",
			Annotations: map[string]string{
				AnnoAllocStatusKey: string(alloc2JSON),
			},
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			PoolRef: "pool1",
		},
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(sandbox2, sandbox1).
		Build()

	store := NewInMemoryAllocationStore().(*InMemoryAllocationStore)
	ctx := context.Background()

	err := store.Recover(ctx, client)
	assert.NoError(t, err)

	assert.Equal(t, "sandbox2", store.pools["default/pool1"].data["pod1"])
}

func TestInMemoryAllocationStore_Recover_ReleasePodReassignedMultipleTimes(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = sandboxv1alpha1.AddToScheme(scheme)

	allocation1 := &SandboxAllocation{Pods: []string{"pod1"}}
	released1 := &AllocationReleased{Pods: []string{"pod1"}}
	allocation2 := &SandboxAllocation{Pods: []string{"pod1"}}
	released2 := &AllocationReleased{Pods: []string{"pod1"}}
	allocation3 := &SandboxAllocation{Pods: []string{"pod1"}}

	alloc1JSON, _ := json.Marshal(allocation1)
	released1JSON, _ := json.Marshal(released1)
	alloc2JSON, _ := json.Marshal(allocation2)
	released2JSON, _ := json.Marshal(released2)
	alloc3JSON, _ := json.Marshal(allocation3)

	sandbox1 := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sandbox1",
			Namespace: "default",
			Annotations: map[string]string{
				AnnoAllocStatusKey:   string(alloc1JSON),
				AnnoAllocReleasedKey: string(released1JSON),
			},
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{PoolRef: "pool1"},
	}

	sandbox2 := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sandbox2",
			Namespace: "default",
			Annotations: map[string]string{
				AnnoAllocStatusKey:   string(alloc2JSON),
				AnnoAllocReleasedKey: string(released2JSON),
			},
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{PoolRef: "pool1"},
	}

	sandbox3 := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sandbox3",
			Namespace: "default",
			Annotations: map[string]string{
				AnnoAllocStatusKey: string(alloc3JSON),
			},
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{PoolRef: "pool1"},
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(sandbox3, sandbox2, sandbox1).
		Build()

	store := NewInMemoryAllocationStore().(*InMemoryAllocationStore)
	ctx := context.Background()

	err := store.Recover(ctx, client)
	assert.NoError(t, err)

	assert.Equal(t, "sandbox3", store.pools["default/pool1"].data["pod1"])
}

func TestInMemoryAllocationStore_Recover_ClearsExisting(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = sandboxv1alpha1.AddToScheme(scheme)

	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	store := NewInMemoryAllocationStore().(*InMemoryAllocationStore)
	ctx := context.Background()

	store.pools["default/pool1"] = &poolEntry{data: map[string]string{"old-pod": "old-sandbox"}}

	allocation := &SandboxAllocation{Pods: []string{"new-pod"}}
	allocJSON, _ := json.Marshal(allocation)

	sandbox := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sandbox1",
			Namespace: "default",
			Annotations: map[string]string{
				AnnoAllocStatusKey: string(allocJSON),
			},
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			PoolRef: "pool1",
		},
	}

	client = fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(sandbox).
		Build()

	err := store.Recover(ctx, client)
	assert.NoError(t, err)

	_, exists := store.pools["default/pool1"].data["old-pod"]
	assert.False(t, exists, "old allocation should be cleared")
	assert.Equal(t, "sandbox1", store.pools["default/pool1"].data["new-pod"])
}

func TestInMemoryAllocationStore_ThreadSafety(t *testing.T) {
	store := NewInMemoryAllocationStore()
	ctx := context.Background()

	var wg sync.WaitGroup
	numGoroutines := 100
	numOperations := 100

	pool := &sandboxv1alpha1.Pool{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pool"},
	}

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < numOperations; j++ {
				alloc := &PoolAllocation{
					PodAllocation: map[string]string{
						"pod": "sandbox",
					},
				}
				_ = store.SetAllocation(ctx, pool, alloc)
				_, _ = store.GetAllocation(ctx, pool)
			}
		}(i)
	}

	wg.Wait()
}

func TestInMemoryAllocationStore_GetAllocation_ReturnsCopy(t *testing.T) {
	store := NewInMemoryAllocationStore()
	ctx := context.Background()

	pool := &sandboxv1alpha1.Pool{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pool"},
	}

	alloc := &PoolAllocation{
		PodAllocation: map[string]string{
			"pod1": "sandbox1",
		},
	}

	_ = store.SetAllocation(ctx, pool, alloc)

	alloc1, _ := store.GetAllocation(ctx, pool)
	alloc2, _ := store.GetAllocation(ctx, pool)

	alloc1.PodAllocation["pod1"] = "modified"

	assert.Equal(t, "sandbox1", alloc2.PodAllocation["pod1"], "GetAllocation should return a copy")
}

func TestInMemoryAllocationStore_SandboxDeleted_PodsReturnedToPool(t *testing.T) {
	store := NewInMemoryAllocationStore()
	ctx := context.Background()

	pool := &sandboxv1alpha1.Pool{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pool"},
	}

	// Initial state: sandbox1 has pod1 and pod2 allocated
	initialAlloc := &PoolAllocation{
		PodAllocation: map[string]string{
			"pod1": "sandbox1",
			"pod2": "sandbox1",
			"pod3": "sandbox2",
		},
	}
	err := store.SetAllocation(ctx, pool, initialAlloc)
	assert.NoError(t, err)

	// Simulate sandbox1 deletion by updating allocation without sandbox1's pods
	// This is what Schedule does when GC detects deleted sandbox
	updatedAlloc := &PoolAllocation{
		PodAllocation: map[string]string{
			"pod3": "sandbox2",
		},
	}
	err = store.SetAllocation(ctx, pool, updatedAlloc)
	assert.NoError(t, err)

	// Verify pods are returned to pool (no longer allocated)
	result, err := store.GetAllocation(ctx, pool)
	assert.NoError(t, err)
	assert.Len(t, result.PodAllocation, 1)
	assert.Equal(t, "sandbox2", result.PodAllocation["pod3"])
	_, exists1 := result.PodAllocation["pod1"]
	_, exists2 := result.PodAllocation["pod2"]
	assert.False(t, exists1, "pod1 should be returned to pool")
	assert.False(t, exists2, "pod2 should be returned to pool")
}

func TestInMemoryAllocationStore_UpdateAllocation_EmptyPods_ReleaseAll(t *testing.T) {
	store := NewInMemoryAllocationStore().(*InMemoryAllocationStore)
	ctx := context.Background()

	// Setup initial allocation
	store.UpdateAllocation(ctx, "default", "pool1", "sandbox1", []string{"pod1", "pod2"})
	assert.Equal(t, 2, len(store.pools["default/pool1"].data))

	// Update with empty pods (simulates release)
	store.UpdateAllocation(ctx, "default", "pool1", "sandbox1", []string{})

	// Verify all pods are released
	assert.Empty(t, store.pools["default/pool1"].data, "all pods should be released when empty slice provided")
}

func TestInMemoryAllocationStore_SetAllocation_ReplaceAll(t *testing.T) {
	store := NewInMemoryAllocationStore()
	ctx := context.Background()

	pool := &sandboxv1alpha1.Pool{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pool"},
	}

	// Set initial allocation with multiple sandboxes
	_ = store.SetAllocation(ctx, pool, &PoolAllocation{
		PodAllocation: map[string]string{
			"pod1": "sandbox1",
			"pod2": "sandbox1",
			"pod3": "sandbox2",
			"pod4": "sandbox2",
		},
	})

	// Simulate GC: replace with allocation excluding deleted sandbox
	_ = store.SetAllocation(ctx, pool, &PoolAllocation{
		PodAllocation: map[string]string{
			"pod3": "sandbox2",
			"pod4": "sandbox2",
		},
	})

	result, _ := store.GetAllocation(ctx, pool)
	assert.Len(t, result.PodAllocation, 2)
	assert.NotContains(t, result.PodAllocation, "pod1")
	assert.NotContains(t, result.PodAllocation, "pod2")
	assert.Equal(t, "sandbox2", result.PodAllocation["pod3"])
	assert.Equal(t, "sandbox2", result.PodAllocation["pod4"])
}

// TestInMemoryAllocationStore_MultiNamespaceSamePoolName tests that pools with the same name
// in different namespaces are properly isolated from each other.
func TestInMemoryAllocationStore_MultiNamespaceSamePoolName(t *testing.T) {
	store := NewInMemoryAllocationStore()
	ctx := context.Background()

	// Create two pools with the same name but in different namespaces
	poolNs1 := &sandboxv1alpha1.Pool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "shared-pool",
			Namespace: "namespace1",
		},
	}
	poolNs2 := &sandboxv1alpha1.Pool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "shared-pool",
			Namespace: "namespace2",
		},
	}

	// Set different allocations for each namespace
	_ = store.SetAllocation(ctx, poolNs1, &PoolAllocation{
		PodAllocation: map[string]string{
			"pod1": "sandbox1-ns1",
			"pod2": "sandbox1-ns1",
		},
	})
	_ = store.SetAllocation(ctx, poolNs2, &PoolAllocation{
		PodAllocation: map[string]string{
			"pod3": "sandbox1-ns2",
			"pod4": "sandbox1-ns2",
		},
	})

	// Verify each namespace only sees its own allocations
	allocNs1, _ := store.GetAllocation(ctx, poolNs1)
	allocNs2, _ := store.GetAllocation(ctx, poolNs2)

	// Namespace1 should have pod1 and pod2
	assert.Len(t, allocNs1.PodAllocation, 2)
	assert.Equal(t, "sandbox1-ns1", allocNs1.PodAllocation["pod1"])
	assert.Equal(t, "sandbox1-ns1", allocNs1.PodAllocation["pod2"])
	assert.Empty(t, allocNs1.PodAllocation["pod3"], "namespace1 should not see namespace2's pods")
	assert.Empty(t, allocNs1.PodAllocation["pod4"], "namespace1 should not see namespace2's pods")

	// Namespace2 should have pod3 and pod4
	assert.Len(t, allocNs2.PodAllocation, 2)
	assert.Equal(t, "sandbox1-ns2", allocNs2.PodAllocation["pod3"])
	assert.Equal(t, "sandbox1-ns2", allocNs2.PodAllocation["pod4"])
	assert.Empty(t, allocNs2.PodAllocation["pod1"], "namespace2 should not see namespace1's pods")
	assert.Empty(t, allocNs2.PodAllocation["pod2"], "namespace2 should not see namespace1's pods")

	// Update allocation in namespace1 should not affect namespace2
	_ = store.SetAllocation(ctx, poolNs1, &PoolAllocation{
		PodAllocation: map[string]string{
			"pod5": "sandbox2-ns1",
		},
	})

	// Verify namespace2 is unchanged
	allocNs2AfterUpdate, _ := store.GetAllocation(ctx, poolNs2)
	assert.Len(t, allocNs2AfterUpdate.PodAllocation, 2, "namespace2 should still have 2 pods")
	assert.Equal(t, "sandbox1-ns2", allocNs2AfterUpdate.PodAllocation["pod3"])
	assert.Equal(t, "sandbox1-ns2", allocNs2AfterUpdate.PodAllocation["pod4"])
}
