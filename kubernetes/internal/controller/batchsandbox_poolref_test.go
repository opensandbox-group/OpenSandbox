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
	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/controller/strategy"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils/fieldindex"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"testing"
)

func TestPoolRefGuardRecovery(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, sandboxv1alpha1.AddToScheme(scheme))
	for _, requested := range []string{"pool-b", "*"} {
		t.Run(requested, func(t *testing.T) {
			sandbox := &sandboxv1alpha1.BatchSandbox{
				ObjectMeta: metav1.ObjectMeta{Name: "sandbox", Namespace: "default", Annotations: map[string]string{
					annoAllocStatusKey:   `{"pods":["pod-1","pod-2"],"poolRef":"pool-a","generation":1}`,
					annoAllocReleasedKey: `{"pods":["pod-2"]}`,
				}},
				Spec: sandboxv1alpha1.BatchSandboxSpec{PoolRef: requested},
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sandbox).
				WithIndex(&sandboxv1alpha1.BatchSandbox{}, fieldindex.IndexNameForPoolRef, fieldindex.PoolRefIndexFunc).Build()
			store := newInMemoryAllocationStore()
			require.NoError(t, store.Recover(ctx, c))
			poolA := &sandboxv1alpha1.Pool{ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "default"}}
			allocation, err := store.GetAllocation(ctx, poolA)
			require.NoError(t, err)
			require.Equal(t, map[string]string{"pod-1": "sandbox"}, allocation.PodAllocation)
			poolB := &sandboxv1alpha1.Pool{ObjectMeta: metav1.ObjectMeta{Name: requested, Namespace: "default"}}
			allocation, err = store.GetAllocation(ctx, poolB)
			require.NoError(t, err)
			require.Empty(t, allocation.PodAllocation)
			for pool, count := range map[string]int{"pool-a": 1, requested: 0} {
				list := &sandboxv1alpha1.BatchSandboxList{}
				require.NoError(t, c.List(ctx, list, client.MatchingFields{fieldindex.IndexNameForPoolRef: pool}))
				require.Len(t, list.Items, count)
			}
			require.Empty(t, strategy.NewPoolStrategy(sandbox).AssignProfile())
			// Later allocation syncs must not silently migrate the durable binding.
			syncer := newAnnoAllocationSyncer(c)
			require.NoError(t, syncer.SetAllocation(ctx, sandbox, &sandboxAllocation{Pods: []string{"pod-1", "pod-3"}}))
			latest := &sandboxv1alpha1.BatchSandbox{}
			require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(sandbox), latest))
			persisted, err := parseSandboxAllocation(latest)
			require.NoError(t, err)
			require.Equal(t, "pool-a", persisted.PoolRef)
			require.Equal(t, requested, latest.Spec.PoolRef)

			recorder := record.NewFakeRecorder(10)
			r := &BatchSandboxReconciler{Recorder: recorder}
			r.applyPoolRefGuard(sandbox, &sandbox.Status)
			require.True(t, hasTrueBatchSandboxCondition(sandbox.Status.Conditions, sandboxv1alpha1.BatchSandboxConditionPoolRefUpdateRejected))
			require.Contains(t, <-recorder.Events, "Warning PoolRefUpdateRejected")
			r.applyPoolRefGuard(sandbox, &sandbox.Status)
			require.Empty(t, recorder.Events, "unchanged rejections must not emit repeatedly")
			sandbox.Spec.PoolRef = ""
			require.Empty(t, utils.EffectivePoolRef(sandbox))
			r.applyPoolRefGuard(sandbox, &sandbox.Status)
			require.Empty(t, sandbox.Status.Conditions)
		})
	}
}
