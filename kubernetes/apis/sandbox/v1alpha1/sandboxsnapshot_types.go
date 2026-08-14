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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:validation:Enum=Pending;Committing;Succeed;Failed
// SandboxSnapshotPhase defines the phase of a snapshot.
type SandboxSnapshotPhase string

const (
	SandboxSnapshotPhasePending    SandboxSnapshotPhase = "Pending"
	SandboxSnapshotPhaseCommitting SandboxSnapshotPhase = "Committing"
	SandboxSnapshotPhaseSucceed    SandboxSnapshotPhase = "Succeed"
	SandboxSnapshotPhaseFailed     SandboxSnapshotPhase = "Failed"
)

// SandboxSnapshotConditionType represents the type of SandboxSnapshot condition.
// +kubebuilder:validation:Enum=Ready;Failed
type SandboxSnapshotConditionType string

const (
	// SandboxSnapshotConditionReady indicates the snapshot is ready for use.
	SandboxSnapshotConditionReady SandboxSnapshotConditionType = "Ready"
	// SandboxSnapshotConditionFailed indicates the snapshot has failed.
	SandboxSnapshotConditionFailed SandboxSnapshotConditionType = "Failed"
)

// SandboxSnapshotFormat identifies the persisted snapshot representation.
// Existing snapshots without this field are interpreted as rootfs-v1.
// +kubebuilder:validation:Enum=rootfs-v1;qemu-v1
type SandboxSnapshotFormat string

const (
	SandboxSnapshotFormatRootfsV1 SandboxSnapshotFormat = "rootfs-v1"
	SandboxSnapshotFormatQEMUV1   SandboxSnapshotFormat = "qemu-v1"
)

// ContainerSnapshot records the snapshot result for a single container.
type ContainerSnapshot struct {
	// ContainerName is the name of the container.
	ContainerName string `json:"containerName"`
	// ImageURI is the snapshot image URI for this container.
	ImageURI string `json:"imageUri"`
	// ImageDigest is the config digest of the pushed snapshot image.
	// +optional
	ImageDigest string `json:"imageDigest,omitempty"`
}

// QEMUCompatibility captures the immutable VM configuration required to load a
// QEMU migration stream safely. The complete launch manifest is stored in the
// VM state image; this status object contains the scheduling and validation
// summary needed by the control plane.
type QEMUCompatibility struct {
	// Architecture is the guest host architecture, for example amd64.
	Architecture string `json:"architecture"`
	// QEMUVersion is the source QEMU version string.
	QEMUVersion string `json:"qemuVersion"`
	// MachineType is the versioned QEMU machine type.
	MachineType string `json:"machineType"`
	// CPUModel is the configured virtual CPU model.
	CPUModel string `json:"cpuModel"`
	// VCPUs is the number of virtual CPUs.
	// +kubebuilder:validation:Minimum=1
	VCPUs int32 `json:"vcpus"`
	// MemoryBytes is the configured guest RAM in bytes.
	// +kubebuilder:validation:Minimum=1
	MemoryBytes int64 `json:"memoryBytes"`
	// QEMUConfigDigest identifies the normalized QEMU launch configuration.
	QEMUConfigDigest string `json:"qemuConfigDigest"`
	// RequiredNodeClass optionally constrains restore to a compatible node class.
	// +optional
	RequiredNodeClass string `json:"requiredNodeClass,omitempty"`
}

// VirtualMachineSnapshot records the separately persisted QEMU migration
// stream. ImageURI and ImageDigest form an immutable pull reference.
type VirtualMachineSnapshot struct {
	// ImageURI is the repository and tag used when the VM state image was pushed.
	ImageURI string `json:"imageUri"`
	// ImageDigest is the digest of the pushed standard container image.
	ImageDigest string `json:"imageDigest"`
	// PayloadDigest is the digest of the compressed VM state payload.
	PayloadDigest string `json:"payloadDigest"`
	// SizeBytes is the compressed VM state payload size.
	// +kubebuilder:validation:Minimum=0
	SizeBytes int64 `json:"sizeBytes"`
	// Compression identifies the payload compression algorithm.
	// +kubebuilder:validation:Enum=zstd
	Compression string `json:"compression"`
	// ManifestDigest identifies the compatibility manifest stored in the image.
	ManifestDigest string `json:"manifestDigest"`
	// Compatibility contains the restore compatibility summary.
	Compatibility QEMUCompatibility `json:"compatibility"`
}

// SandboxSnapshotCondition represents a condition of a SandboxSnapshot.
type SandboxSnapshotCondition struct {
	// Type is the condition type.
	// +kubebuilder:validation:Required
	Type SandboxSnapshotConditionType `json:"type"`
	// Status is the condition status.
	// +kubebuilder:validation:Enum=True;False
	// +kubebuilder:validation:Required
	Status string `json:"status"`
	// Reason is a brief reason for the condition.
	// +optional
	Reason string `json:"reason,omitempty"`
	// Message is a human-readable message about the condition.
	// +optional
	Message string `json:"message,omitempty"`
	// LastTransitionTime is the last time the condition transitioned.
	// +optional
	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`
}

// SandboxSnapshotSpec defines the desired state of SandboxSnapshot.
// Pure atomic capability: caller fills spec, Controller only reads spec.
// Registry/snapshotPushSecret/snapshotType come from Controller Manager startup params.
type SandboxSnapshotSpec struct {
	// SandboxName is the name of the target BatchSandbox (same namespace as SandboxSnapshot).
	// Controller uses this to find BatchSandbox -> find Pod -> dispatch commit Job.
	// +kubebuilder:validation:Required
	SandboxName string `json:"sandboxName"`
}

// SandboxSnapshotStatus defines the observed state of SandboxSnapshot.
// Status is written by Controller, read-only for callers.
type SandboxSnapshotStatus struct {
	// Phase indicates the current phase of the snapshot.
	Phase SandboxSnapshotPhase `json:"phase,omitempty"`

	// Format identifies the snapshot representation. Empty means rootfs-v1 for
	// backward compatibility with snapshots created before this field existed.
	// +optional
	Format SandboxSnapshotFormat `json:"format,omitempty"`

	// Containers holds per-container snapshot results, filled after Succeed.
	// +optional
	Containers []ContainerSnapshot `json:"containers,omitempty"`

	// VirtualMachine contains the QEMU VM state artifact for qemu-v1 snapshots.
	// +optional
	VirtualMachine *VirtualMachineSnapshot `json:"virtualMachine,omitempty"`

	// Conditions records the readiness or failure of the snapshot.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []SandboxSnapshotCondition `json:"conditions,omitempty"`

	// SourcePodName is the name of the source Pod (resolved by Controller).
	// +optional
	SourcePodName string `json:"sourcePodName,omitempty"`

	// SourceNodeName is the node where the source Pod runs (for Job scheduling).
	// +optional
	SourceNodeName string `json:"sourceNodeName,omitempty"`

	// ObservedGeneration is the most recent spec generation observed by the Controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=sbxsnap
// +kubebuilder:printcolumn:name="PHASE",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="SANDBOX",type="string",JSONPath=".spec.sandboxName"
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"
type SandboxSnapshot struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SandboxSnapshotSpec   `json:"spec,omitempty"`
	Status SandboxSnapshotStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SandboxSnapshotList contains a list of SandboxSnapshot.
type SandboxSnapshotList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SandboxSnapshot `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SandboxSnapshot{}, &SandboxSnapshotList{})
}
