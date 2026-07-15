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
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils"
)

type PoolUpdateStrategy interface {
	Compute(ctx context.Context, updateRevision string, pods []*corev1.Pod, idlePods []string) *UpdateResult
}

func NewPoolUpdateStrategy(pool *sandboxv1alpha1.Pool) PoolUpdateStrategy {
	return &recreateUpdateStrategy{pool: pool}
}

func getUpdateMaxUnavailable(pool *sandboxv1alpha1.Pool, desiredTotal int32) int32 {
	defaultPercentage := intstr.FromString("25%")
	maxUnavailable := &defaultPercentage
	if pool.Spec.UpdateStrategy != nil && pool.Spec.UpdateStrategy.MaxUnavailable != nil {
		maxUnavailable = pool.Spec.UpdateStrategy.MaxUnavailable
	}
	result, err := intstr.GetScaledValueFromIntOrPercent(maxUnavailable, int(desiredTotal), true)
	if err != nil || result < 1 {
		result = 1
	}
	return int32(result)
}

type recreateUpdateStrategy struct {
	pool *sandboxv1alpha1.Pool
}

func (s *recreateUpdateStrategy) Compute(ctx context.Context, updateRevision string, pods []*corev1.Pod, idlePods []string) *UpdateResult {
	log := logf.FromContext(ctx)
	maxUnavailable := getUpdateMaxUnavailable(s.pool, int32(len(pods)))

	podMap := make(map[string]*corev1.Pod, len(pods))
	for _, pod := range pods {
		podMap[pod.Name] = pod
	}

	curUnavailable := int32(0)
	for _, pod := range pods {
		if !utils.IsPodReady(pod) {
			curUnavailable++
		}
	}
	unavailableBudget := max(maxUnavailable-curUnavailable, 0)

	idlePodList := make([]*corev1.Pod, 0, len(idlePods))
	for _, name := range idlePods {
		if pod, ok := podMap[name]; ok {
			idlePodList = append(idlePodList, pod)
		}
	}

	sort.SliceStable(idlePodList, func(i, j int) bool {
		return utils.ComparePodsForDeletion(idlePodList[i], idlePodList[j])
	})

	toDeleteCurRevPods := make([]string, 0)
	supplyNew := int32(0)
	remainingIdlePods := make([]string, 0)

	for _, pod := range idlePodList {
		if pod.Labels[LabelPoolRevision] == updateRevision {
			remainingIdlePods = append(remainingIdlePods, pod.Name)
			continue
		}
		if !utils.IsPodReady(pod) {
			toDeleteCurRevPods = append(toDeleteCurRevPods, pod.Name)
			supplyNew++
		} else if unavailableBudget > 0 {
			toDeleteCurRevPods = append(toDeleteCurRevPods, pod.Name)
			supplyNew++
			unavailableBudget--
		} else {
			remainingIdlePods = append(remainingIdlePods, pod.Name)
		}
	}

	if len(toDeleteCurRevPods) > 0 {
		log.Info("Recreate update: to recreate current revision pods", "updateRevision", updateRevision,
			"maxUnavailable", maxUnavailable, "curUnavailable", curUnavailable,
			"toDeleteCurrentRevisionPods", toDeleteCurRevPods, "supplyNew", supplyNew, "idlePods", len(remainingIdlePods))
	}
	return &UpdateResult{
		IdlePods:             remainingIdlePods,
		ToDeletePods:         toDeleteCurRevPods,
		SupplyUpdateRevision: supplyNew,
	}
}
