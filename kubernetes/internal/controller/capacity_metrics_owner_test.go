// Copyright 2026 Alibaba Group Holding Ltd.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils/fieldindex"
)

func TestCapacityMetricsSelectPoolPodsByOwnerUID(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, sandboxv1alpha1.AddToScheme(scheme))

	pool := &sandboxv1alpha1.Pool{ObjectMeta: metav1.ObjectMeta{Name: "warm", Namespace: "tenant-a", UID: "current-pool"}}
	owned := poolPod("owned", "warm", true, "500m", "512Mi")
	setPoolOwner(pool, owned)
	mislabeled := poolPod("mislabeled", "warm", true, "1", "1Gi")
	client := capacityMetricsClient(scheme, pool, owned, mislabeled)
	allocations := capacityAllocationReaderStub{allocations: map[string]map[string]string{"tenant-a/warm": {}}}

	capacity, err := collectPoolCapacity(context.Background(), client, allocations, pool)
	require.NoError(t, err)
	require.Equal(t, map[string]float64{"total": 0.5, "allocated": 0, "available": 0.5}, capacity.cpu)
	require.Equal(t, map[string]int64{"total": 536870912, "allocated": 0, "available": 536870912}, capacity.memory)
}

func capacityMetricsClient(scheme *runtime.Scheme, objects ...client.Object) client.Client {
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithIndex(&corev1.Pod{}, fieldindex.IndexNameForOwnerRefUID, fieldindex.OwnerIndexFunc).
		WithObjects(objects...).
		Build()
}

func setPoolOwner(pool *sandboxv1alpha1.Pool, pods ...*corev1.Pod) {
	for _, pod := range pods {
		pod.OwnerReferences = []metav1.OwnerReference{{UID: pool.UID}}
	}
}
