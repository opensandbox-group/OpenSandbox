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
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

const (
	RequestVersionV1       = "v1"
	VMStateFormatVersion1  = "qemu-v1"
	VMStateCompressionZstd = "zstd"
	QEMUDiskCaptureRootfs  = "rootfs"

	VMStateManifestFilename = "manifest.json"
	VMStatePayloadFilename  = "vmstate.zst"
	VMStateLoaderFilename   = "vmstate-loader"
)

// ContainerTarget identifies a container rootfs image to commit and push.
type ContainerTarget struct {
	Name     string `json:"name"`
	ImageURI string `json:"imageUri"`
}

// Request is the structured snapshot Job input. Rootfs-only snapshots keep the
// legacy command-line interface during the compatibility transition.
type Request struct {
	Version           string            `json:"version"`
	PodName           string            `json:"podName"`
	PodUID            string            `json:"podUid,omitempty"`
	Namespace         string            `json:"namespace"`
	Provider          string            `json:"provider"`
	Containers        []ContainerTarget `json:"containers"`
	QEMU              *QEMURequest      `json:"qemu,omitempty"`
	VMStateImageURI   string            `json:"vmStateImageUri,omitempty"`
	LeaveSourceFrozen bool              `json:"leaveSourceFrozen,omitempty"`
}

// QEMURequest contains the resolved QEMU workload contract.
type QEMURequest struct {
	ContainerName      string   `json:"containerName"`
	QMPSocketPath      string   `json:"qmpSocketPath"`
	LaunchManifestPath string   `json:"launchManifestPath"`
	RequiredNodeClass  string   `json:"requiredNodeClass,omitempty"`
	VolumeMountPaths   []string `json:"volumeMountPaths,omitempty"`
}

// Result is written to the Job termination message after all referenced
// registry manifests are available by digest.
type Result struct {
	Containers     []ContainerResult `json:"containers"`
	VirtualMachine *VMStateResult    `json:"virtualMachine,omitempty"`
}

type ContainerResult struct {
	Name   string `json:"name"`
	Image  string `json:"image"`
	Digest string `json:"digest"`
}

type VMStateResult struct {
	ImageURI       string            `json:"imageUri"`
	ImageDigest    string            `json:"imageDigest"`
	PayloadDigest  string            `json:"payloadDigest"`
	SizeBytes      int64             `json:"sizeBytes"`
	Compression    string            `json:"compression"`
	ManifestDigest string            `json:"manifestDigest"`
	Compatibility  QEMUCompatibility `json:"compatibility"`
}

// QEMULaunchManifest is produced by the QEMU workload before checkpoint. Disk
// overlays marked rootfs must not resolve under Kubernetes volume mounts.
type QEMULaunchManifest struct {
	FormatVersion    string            `json:"formatVersion"`
	Architecture     string            `json:"architecture"`
	QEMUVersion      string            `json:"qemuVersion"`
	MachineType      string            `json:"machineType"`
	CPUModel         string            `json:"cpuModel"`
	VCPUs            int32             `json:"vcpus"`
	MemoryBytes      int64             `json:"memoryBytes"`
	QEMUConfigDigest string            `json:"qemuConfigDigest"`
	Disks            []QEMUDisk        `json:"disks,omitempty"`
	Metadata         map[string]string `json:"metadata,omitempty"`
}

type QEMUDisk struct {
	ID          string `json:"id"`
	OverlayPath string `json:"overlayPath"`
	BasePath    string `json:"basePath,omitempty"`
	BaseDigest  string `json:"baseDigest,omitempty"`
	Capture     string `json:"capture"`
}

type QEMUCompatibility struct {
	Architecture      string `json:"architecture"`
	QEMUVersion       string `json:"qemuVersion"`
	MachineType       string `json:"machineType"`
	CPUModel          string `json:"cpuModel"`
	VCPUs             int32  `json:"vcpus"`
	MemoryBytes       int64  `json:"memoryBytes"`
	QEMUConfigDigest  string `json:"qemuConfigDigest"`
	RequiredNodeClass string `json:"requiredNodeClass,omitempty"`
}

// VMStateManifest is stored beside the payload in the VM state image.
type VMStateManifest struct {
	FormatVersion string            `json:"formatVersion"`
	PayloadDigest string            `json:"payloadDigest"`
	PayloadSize   int64             `json:"payloadSize"`
	Compression   string            `json:"compression"`
	Compatibility QEMUCompatibility `json:"compatibility"`
	Disks         []QEMUDisk        `json:"disks,omitempty"`
}

func (m QEMULaunchManifest) Compatibility(requiredNodeClass string) QEMUCompatibility {
	return QEMUCompatibility{
		Architecture:      m.Architecture,
		QEMUVersion:       m.QEMUVersion,
		MachineType:       m.MachineType,
		CPUModel:          m.CPUModel,
		VCPUs:             m.VCPUs,
		MemoryBytes:       m.MemoryBytes,
		QEMUConfigDigest:  m.QEMUConfigDigest,
		RequiredNodeClass: requiredNodeClass,
	}
}

func (m QEMULaunchManifest) Validate() error {
	if m.FormatVersion != VMStateFormatVersion1 {
		return fmt.Errorf("unsupported QEMU launch manifest format %q", m.FormatVersion)
	}
	if strings.TrimSpace(m.Architecture) == "" || strings.TrimSpace(m.QEMUVersion) == "" ||
		strings.TrimSpace(m.MachineType) == "" || strings.TrimSpace(m.CPUModel) == "" ||
		strings.TrimSpace(m.QEMUConfigDigest) == "" {
		return errors.New("QEMU launch manifest is missing compatibility fields")
	}
	if m.VCPUs <= 0 || m.MemoryBytes <= 0 {
		return errors.New("QEMU launch manifest vCPU and memory values must be positive")
	}

	seenDisks := make(map[string]struct{}, len(m.Disks))
	for _, disk := range m.Disks {
		if strings.TrimSpace(disk.ID) == "" {
			return errors.New("QEMU disk ID is required")
		}
		if _, ok := seenDisks[disk.ID]; ok {
			return fmt.Errorf("duplicate QEMU disk ID %q", disk.ID)
		}
		seenDisks[disk.ID] = struct{}{}
		if disk.Capture != QEMUDiskCaptureRootfs {
			return fmt.Errorf("QEMU disk %q has unsupported capture mode %q", disk.ID, disk.Capture)
		}
		if !isCleanAbsolutePath(disk.OverlayPath) {
			return fmt.Errorf("QEMU disk %q overlay path must be clean and absolute", disk.ID)
		}
		if disk.BasePath != "" && !isCleanAbsolutePath(disk.BasePath) {
			return fmt.Errorf("QEMU disk %q base path must be clean and absolute", disk.ID)
		}
	}
	return nil
}

func (m VMStateManifest) Validate() error {
	if m.FormatVersion != VMStateFormatVersion1 {
		return fmt.Errorf("unsupported VM state format %q", m.FormatVersion)
	}
	if m.Compression != VMStateCompressionZstd {
		return fmt.Errorf("unsupported VM state compression %q", m.Compression)
	}
	if m.PayloadSize < 0 {
		return errors.New("VM state payload size must not be negative")
	}
	if _, err := ParseSHA256Digest(m.PayloadDigest); err != nil {
		return fmt.Errorf("invalid VM state payload digest: %w", err)
	}
	compatibility := QEMULaunchManifest{
		FormatVersion:    m.FormatVersion,
		Architecture:     m.Compatibility.Architecture,
		QEMUVersion:      m.Compatibility.QEMUVersion,
		MachineType:      m.Compatibility.MachineType,
		CPUModel:         m.Compatibility.CPUModel,
		VCPUs:            m.Compatibility.VCPUs,
		MemoryBytes:      m.Compatibility.MemoryBytes,
		QEMUConfigDigest: m.Compatibility.QEMUConfigDigest,
		Disks:            m.Disks,
	}
	return compatibility.Validate()
}

func isCleanAbsolutePath(value string) bool {
	return filepath.IsAbs(value) && filepath.Clean(value) == value
}
