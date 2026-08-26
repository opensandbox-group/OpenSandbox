// Copyright 2026 Alibaba Group Holding Ltd.
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

package snapshot

import (
	"fmt"
	"path/filepath"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

const (
	AnnotationCheckpointProvider    = "sandbox.opensandbox.io/checkpoint-provider"
	AnnotationQEMUContainer         = "sandbox.opensandbox.io/qemu-container"
	AnnotationQEMUQMPSocket         = "sandbox.opensandbox.io/qemu-qmp-socket"
	AnnotationQEMULaunchManifest    = "sandbox.opensandbox.io/qemu-launch-manifest"
	AnnotationQEMURequiredNodeClass = "sandbox.opensandbox.io/qemu-required-node-class"
	LabelQEMUNodeClass              = "sandbox.opensandbox.io/qemu-node-class"

	ProviderRootfs = "rootfs"
	ProviderQEMU   = "qemu"
)

// QEMUContract is the explicit workload contract between a QEMU-in-runc image
// and the OpenSandbox snapshot worker. OpenSandbox never detects QEMU by
// scanning process names.
type QEMUContract struct {
	ContainerName      string   `json:"containerName"`
	QMPSocketPath      string   `json:"qmpSocketPath"`
	LaunchManifestPath string   `json:"launchManifestPath"`
	RequiredNodeClass  string   `json:"requiredNodeClass,omitempty"`
	VolumeMountPaths   []string `json:"volumeMountPaths,omitempty"`
}

// WorkloadContract describes the checkpoint provider selected by a Pod.
type WorkloadContract struct {
	Provider string        `json:"provider"`
	QEMU     *QEMUContract `json:"qemu,omitempty"`
}

// ContractFromPod returns the explicit checkpoint contract. Pods without a
// provider annotation retain the existing rootfs-only behavior.
func ContractFromPod(pod *corev1.Pod) (WorkloadContract, error) {
	if pod == nil {
		return WorkloadContract{}, fmt.Errorf("pod is required")
	}

	provider := strings.ToLower(strings.TrimSpace(pod.Annotations[AnnotationCheckpointProvider]))
	if provider == "" || provider == ProviderRootfs {
		return WorkloadContract{Provider: ProviderRootfs}, nil
	}
	if provider != ProviderQEMU {
		return WorkloadContract{}, fmt.Errorf("unsupported checkpoint provider %q", provider)
	}

	contract := &QEMUContract{
		ContainerName:      strings.TrimSpace(pod.Annotations[AnnotationQEMUContainer]),
		QMPSocketPath:      strings.TrimSpace(pod.Annotations[AnnotationQEMUQMPSocket]),
		LaunchManifestPath: strings.TrimSpace(pod.Annotations[AnnotationQEMULaunchManifest]),
		RequiredNodeClass:  strings.TrimSpace(pod.Annotations[AnnotationQEMURequiredNodeClass]),
	}
	if contract.ContainerName == "" {
		return WorkloadContract{}, fmt.Errorf("%s is required for qemu checkpoints", AnnotationQEMUContainer)
	}
	if !podHasContainer(pod, contract.ContainerName) {
		return WorkloadContract{}, fmt.Errorf("qemu checkpoint container %q does not exist", contract.ContainerName)
	}
	contract.VolumeMountPaths = containerVolumePaths(pod, contract.ContainerName)
	if err := validateAbsoluteContainerPath(AnnotationQEMUQMPSocket, contract.QMPSocketPath); err != nil {
		return WorkloadContract{}, err
	}
	if err := validateAbsoluteContainerPath(AnnotationQEMULaunchManifest, contract.LaunchManifestPath); err != nil {
		return WorkloadContract{}, err
	}

	return WorkloadContract{Provider: ProviderQEMU, QEMU: contract}, nil
}

func containerVolumePaths(pod *corev1.Pod, containerName string) []string {
	for i := range pod.Spec.Containers {
		container := &pod.Spec.Containers[i]
		if container.Name != containerName {
			continue
		}
		paths := make([]string, 0, len(container.VolumeMounts)+len(container.VolumeDevices))
		for _, mount := range container.VolumeMounts {
			paths = append(paths, mount.MountPath)
		}
		for _, device := range container.VolumeDevices {
			paths = append(paths, device.DevicePath)
		}
		return paths
	}
	return nil
}

func podHasContainer(pod *corev1.Pod, name string) bool {
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == name {
			return true
		}
	}
	return false
}

func validateAbsoluteContainerPath(annotation, value string) error {
	if value == "" {
		return fmt.Errorf("%s is required for qemu checkpoints", annotation)
	}
	if !filepath.IsAbs(value) || filepath.Clean(value) != value {
		return fmt.Errorf("%s must be a clean absolute path", annotation)
	}
	return nil
}
