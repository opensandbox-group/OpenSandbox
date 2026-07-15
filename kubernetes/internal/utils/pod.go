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

package utils

import (
	"fmt"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	apimachineryvalidation "k8s.io/apimachinery/pkg/api/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
)

// IsPodAvailable returns true if a pod is available; false otherwise.
// Precondition for an available pod is that it must be ready. On top
// of that, there are two cases when a pod can be considered available:
// 1. minReadySeconds == 0, or
// 2. LastTransitionTime (is set) + minReadySeconds < current time
func IsPodAvailable(pod *v1.Pod, minReadySeconds int32, now metav1.Time) bool {
	if !IsPodReady(pod) {
		return false
	}

	c := GetPodReadyCondition(pod.Status)
	minReadySecondsDuration := time.Duration(minReadySeconds) * time.Second
	if minReadySeconds == 0 || (!c.LastTransitionTime.IsZero() && c.LastTransitionTime.Add(minReadySecondsDuration).Before(now.Time)) {
		return true
	}
	return false
}

// IsPodReady returns true if a pod is ready; false otherwise.
func IsPodReady(pod *v1.Pod) bool {
	return pod.Status.Phase == v1.PodRunning && IsPodReadyConditionTrue(pod.Status)
}

// IsPodTerminal returns true if a pod is terminal, all containers are stopped and cannot ever regress.
func IsPodTerminal(pod *v1.Pod) bool {
	return IsPodPhaseTerminal(pod.Status.Phase)
}

// IsPodPhaseTerminal returns true if the pod's phase is terminal.
func IsPodPhaseTerminal(phase v1.PodPhase) bool {
	return phase == v1.PodFailed || phase == v1.PodSucceeded
}

// IsPodReadyConditionTrue returns true if a pod is ready; false otherwise.
func IsPodReadyConditionTrue(status v1.PodStatus) bool {
	condition := GetPodReadyCondition(status)
	return condition != nil && condition.Status == v1.ConditionTrue
}

// IsContainersReadyConditionTrue returns true if a pod is ready; false otherwise.
func IsContainersReadyConditionTrue(status v1.PodStatus) bool {
	condition := GetContainersReadyCondition(status)
	return condition != nil && condition.Status == v1.ConditionTrue
}

// GetPodReadyCondition extracts the pod ready condition from the given status and returns that.
// Returns nil if the condition is not present.
func GetPodReadyCondition(status v1.PodStatus) *v1.PodCondition {
	_, condition := GetPodCondition(&status, v1.PodReady)
	return condition
}

// GetContainersReadyCondition extracts the containers ready condition from the given status and returns that.
// Returns nil if the condition is not present.
func GetContainersReadyCondition(status v1.PodStatus) *v1.PodCondition {
	_, condition := GetPodCondition(&status, v1.ContainersReady)
	return condition
}

// GetPodCondition extracts the provided condition from the given status and returns that.
// Returns nil and -1 if the condition is not present, and the index of the located condition.
func GetPodCondition(status *v1.PodStatus, conditionType v1.PodConditionType) (int, *v1.PodCondition) {
	if status == nil {
		return -1, nil
	}
	return GetPodConditionFromList(status.Conditions, conditionType)
}

// GetPodConditionFromList extracts the provided condition from the given list of condition and
// returns the index of the condition and the condition. Returns -1 and nil if the condition is not present.
func GetPodConditionFromList(conditions []v1.PodCondition, conditionType v1.PodConditionType) (int, *v1.PodCondition) {
	if conditions == nil {
		return -1, nil
	}
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return i, &conditions[i]
		}
	}
	return -1, nil
}

func GetPodFromTemplate(
	template *v1.PodTemplateSpec,
	parentObject runtime.Object,
	controllerRef *metav1.OwnerReference,
) (*v1.Pod, error) {
	desiredLabels := getPodsLabelSet(template)
	desiredFinalizers := getPodsFinalizers(template)
	desiredAnnotations := getPodsAnnotationSet(template)
	accessor, err := meta.Accessor(parentObject)
	if err != nil {
		return nil, fmt.Errorf("parentObject does not have ObjectMeta, %v", err)
	}
	prefix := getPodsPrefix(accessor.GetName())

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Labels:       desiredLabels,
			Annotations:  desiredAnnotations,
			GenerateName: prefix,
			Finalizers:   desiredFinalizers,
		},
	}
	if controllerRef != nil {
		pod.OwnerReferences = append(pod.OwnerReferences, *controllerRef)
	}
	pod.Spec = *template.Spec.DeepCopy()
	return pod, nil
}

func getPodsLabelSet(template *v1.PodTemplateSpec) labels.Set {
	desiredLabels := make(labels.Set)
	for k, v := range template.Labels {
		desiredLabels[k] = v
	}
	return desiredLabels
}

func getPodsFinalizers(template *v1.PodTemplateSpec) []string {
	desiredFinalizers := make([]string, len(template.Finalizers))
	copy(desiredFinalizers, template.Finalizers)
	return desiredFinalizers
}

func getPodsAnnotationSet(template *v1.PodTemplateSpec) labels.Set {
	desiredAnnotations := make(labels.Set)
	for k, v := range template.Annotations {
		desiredAnnotations[k] = v
	}
	return desiredAnnotations
}

func getPodsPrefix(controllerName string) string {
	// use the dash (if the name isn't too long) to make the pod name a bit prettier
	prefix := fmt.Sprintf("%s-", controllerName)
	if len(apimachineryvalidation.NameIsDNSSubdomain(prefix, true)) != 0 {
		prefix = controllerName
	}
	return prefix
}

func IsAssigned(pod *v1.Pod) bool {
	return pod != nil && (pod.Spec.NodeName != "" || pod.Status.PodIP != "")
}

func PodNameSorter(a, b *v1.Pod) int {
	if a.Name < b.Name {
		return -1
	} else if a.Name > b.Name {
		return 1
	}
	return 0
}

func WithPodIndexSorter(podIndex map[string]int) func(*v1.Pod, *v1.Pod) int {
	return func(a, b *v1.Pod) int {
		aIdx, aOk := podIndex[a.Name]
		bIdx, bOk := podIndex[b.Name]
		if !aOk && !bOk {
			return 0
		}
		if !aOk {
			return 1
		}
		if !bOk {
			return -1
		}
		if aIdx < bIdx {
			return -1
		} else if aIdx > bIdx {
			return 1
		}
		return 0
	}
}

type MultiPodSorter []func(a, b *v1.Pod) int

func (m MultiPodSorter) Sort(a, b *v1.Pod) int {
	for i := range m {
		ret := m[i](a, b)
		if ret != 0 {
			return ret
		}
	}
	return 0
}

// ComparePodsForDeletion compares two pods for deletion priority.
// Returns true if p1 should be deleted before p2.
// Priority order: Unassigned < Assigned, Pending < Unknown < Running,
// NotReady < Ready, shorter ready time < longer ready time,
// higher restarts < lower restarts, newer < older, name for tie-breaking.
func ComparePodsForDeletion(p1, p2 *v1.Pod) bool {
	if len(p1.Spec.NodeName) != len(p2.Spec.NodeName) && (len(p1.Spec.NodeName) == 0 || len(p2.Spec.NodeName) == 0) {
		return len(p1.Spec.NodeName) == 0
	}

	phaseOrder := map[v1.PodPhase]int{
		v1.PodPending: 0,
		v1.PodUnknown: 1,
		v1.PodRunning: 2,
	}
	if phaseOrder[p1.Status.Phase] != phaseOrder[p2.Status.Phase] {
		return phaseOrder[p1.Status.Phase] < phaseOrder[p2.Status.Phase]
	}

	p1Ready := IsPodReady(p1)
	p2Ready := IsPodReady(p2)
	if p1Ready != p2Ready {
		return !p1Ready
	}

	if p1Ready && p2Ready {
		p1Cond := GetPodReadyCondition(p1.Status)
		p2Cond := GetPodReadyCondition(p2.Status)
		if p1Cond != nil && p2Cond != nil && !p1Cond.LastTransitionTime.Equal(&p2Cond.LastTransitionTime) {
			return p1Cond.LastTransitionTime.Before(&p2Cond.LastTransitionTime)
		}
	}

	p1Restarts := maxContainerRestarts(p1)
	p2Restarts := maxContainerRestarts(p2)
	if p1Restarts != p2Restarts {
		return p1Restarts > p2Restarts
	}

	if !p1.CreationTimestamp.Equal(&p2.CreationTimestamp) {
		return p2.CreationTimestamp.Before(&p1.CreationTimestamp)
	}
	return p1.Name < p2.Name
}

func maxContainerRestarts(pod *v1.Pod) int32 {
	var maxRestarts int32
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.RestartCount > maxRestarts {
			maxRestarts = cs.RestartCount
		}
	}
	return maxRestarts
}
