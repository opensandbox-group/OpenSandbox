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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// RecycleType defines the type of recycle policy.
type RecycleType string

const (
	// RecycleTypeNoop does nothing when a pod is returned to the pool.
	RecycleTypeNoop RecycleType = "Noop"
	// RecycleTypeDelete deletes the pod when it is returned to the pool.
	RecycleTypeDelete RecycleType = "Delete"
	// RecycleTypeRestart restarts the pod containers when it is returned to the pool.
	RecycleTypeRestart RecycleType = "Restart"
)

// RecycleStrategy controls how pods are handled when returned to the pool.
type RecycleStrategy struct {
	// Type specifies the recycle policy type.
	// Default is Delete.
	// +kubebuilder:validation:Enum=Delete;Restart;Noop
	// +kubebuilder:default=Delete
	// +optional
	Type RecycleType `json:"type,omitempty"`
}

// PoolSpec defines the desired state of Pool.
type PoolSpec struct {
	// Pod Template used to create pre-warmed nodes in the pool.
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:validation:Optional
	Template *corev1.PodTemplateSpec `json:"template"`
	// CapacitySpec controls the size of the resource pool.
	// +kubebuilder:validation:Required
	CapacitySpec CapacitySpec `json:"capacitySpec"`
	// ScaleStrategy controls the scaling behavior.
	// +optional
	ScaleStrategy *ScaleStrategy `json:"scaleStrategy,omitempty"`
	// UpdateStrategy controls how pool pods are updated when the template changes.
	// +optional
	UpdateStrategy *UpdateStrategy `json:"updateStrategy,omitempty"`
	// RecycleStrategy controls how pods are handled when returned to the pool.
	// Default is Delete, which deletes the pod.
	// Restart strategy restarts the pod containers instead of deleting.
	// +optional
	RecycleStrategy *RecycleStrategy `json:"recycleStrategy,omitempty"`
}

type CapacitySpec struct {
	// BufferMax is the maximum number of nodes kept in the warm buffer.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Required
	BufferMax int32 `json:"bufferMax"`
	// BufferMin is the minimum number of nodes that must remain in the buffer.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Required
	BufferMin int32 `json:"bufferMin"`
	// PoolMax is the maximum total number of nodes allowed in the entire pool.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Required
	PoolMax int32 `json:"poolMax"`
	// PoolMin is the minimum total size of the pool.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Required
	PoolMin int32 `json:"poolMin"`
}

// ScaleStrategy controls the pace of scaling operations.
type ScaleStrategy struct {
	// MaxUnavailable is the maximum number of pods that can be unavailable during scaling.
	// Can be an absolute number (ex: 5) or a percentage of desired pods (ex: "20%").
	// Defaults to 25%.
	// +optional
	MaxUnavailable *intstr.IntOrString `json:"maxUnavailable,omitempty"`
}

// UpdateStrategy controls how pool pods are updated when the pool template changes.
type UpdateStrategy struct {
	// MaxUnavailable is the maximum number of pods that can be unavailable during an update.
	// Can be an absolute number (ex: 5) or a percentage of desired pods (ex: "20%").
	// Defaults to 25%.
	// +optional
	MaxUnavailable *intstr.IntOrString `json:"maxUnavailable,omitempty"`
}

// PoolStatus defines the observed state of Pool.
type PoolStatus struct {
	// ObservedGeneration is the most recent generation observed for this BatchSandbox. It corresponds to the
	// BatchSandbox's generation, which is updated on mutation by the API Server.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Revision is the latest version of pool
	Revision string `json:"revision"`
	// Total is the total number of nodes in the pool.
	Total int32 `json:"total"`
	// Allocated is the number of nodes currently allocated to sandboxes.
	Allocated int32 `json:"allocated"`
	// Available is the number of nodes currently available in the pool.
	Available int32 `json:"available"`
	// Updated is the number of nodes that have been updated to the latest revision.
	Updated int32 `json:"updated,omitempty"`
}

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="TOTAL",type="integer",JSONPath=".status.total",description="The number of all nodes in pool."
// +kubebuilder:printcolumn:name="ALLOCATED",type="integer",JSONPath=".status.allocated",description="The number of allocated nodes in pool."
// +kubebuilder:printcolumn:name="AVAILABLE",type="integer",JSONPath=".status.available",description="The number of available nodes in pool."
// +kubebuilder:printcolumn:name="UPDATED",type="integer",JSONPath=".status.updated",description="The number of nodes updated to the latest revision."
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"
// Pool is the Schema for the pools API.
type Pool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PoolSpec   `json:"spec,omitempty"`
	Status PoolStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PoolList contains a list of Pool.
type PoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Pool `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Pool{}, &PoolList{})
}
