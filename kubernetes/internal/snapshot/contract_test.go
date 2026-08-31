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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestContractFromPodDefaultsToRootfs(t *testing.T) {
	contract, err := ContractFromPod(&corev1.Pod{})
	if err != nil {
		t.Fatalf("ContractFromPod returned error: %v", err)
	}
	if contract.Provider != ProviderRootfs || contract.QEMU != nil {
		t.Fatalf("unexpected contract: %#v", contract)
	}
}

func TestContractFromPodParsesQEMU(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
			AnnotationCheckpointProvider:    ProviderQEMU,
			AnnotationQEMUContainer:         "main",
			AnnotationQEMUQMPSocket:         "/run/opensandbox/qemu/qmp.sock",
			AnnotationQEMULaunchManifest:    "/run/opensandbox/qemu/launch.json",
			AnnotationQEMURequiredNodeClass: "shenlong-v1",
		}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name:          "main",
			VolumeMounts:  []corev1.VolumeMount{{MountPath: "/data"}},
			VolumeDevices: []corev1.VolumeDevice{{DevicePath: "/dev/vdb"}},
		}}},
	}

	contract, err := ContractFromPod(pod)
	if err != nil {
		t.Fatalf("ContractFromPod returned error: %v", err)
	}
	if contract.Provider != ProviderQEMU || contract.QEMU == nil {
		t.Fatalf("unexpected contract: %#v", contract)
	}
	if contract.QEMU.ContainerName != "main" || contract.QEMU.RequiredNodeClass != "shenlong-v1" {
		t.Fatalf("unexpected qemu contract: %#v", contract.QEMU)
	}
	if len(contract.QEMU.VolumeMountPaths) != 2 || contract.QEMU.VolumeMountPaths[0] != "/data" || contract.QEMU.VolumeMountPaths[1] != "/dev/vdb" {
		t.Fatalf("unexpected QEMU volume paths: %#v", contract.QEMU.VolumeMountPaths)
	}
}

func TestContractFromPodRejectsInvalidQEMUContract(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		wantError   string
	}{
		{
			name: "missing container",
			annotations: map[string]string{
				AnnotationCheckpointProvider: ProviderQEMU,
			},
			wantError: AnnotationQEMUContainer,
		},
		{
			name: "relative qmp socket",
			annotations: map[string]string{
				AnnotationCheckpointProvider: ProviderQEMU,
				AnnotationQEMUContainer:      "main",
				AnnotationQEMUQMPSocket:      "run/qmp.sock",
				AnnotationQEMULaunchManifest: "/run/launch.json",
			},
			wantError: "clean absolute path",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Annotations: tt.annotations},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "main"}}},
			}
			_, err := ContractFromPod(pod)
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("expected error containing %q, got %v", tt.wantError, err)
			}
		})
	}
}
