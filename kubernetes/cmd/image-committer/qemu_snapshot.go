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

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/klauspost/compress/zstd"

	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/snapshot"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/snapshot/registryimage"
)

const (
	defaultVMStateWorkDir = "/workspace/checkpoint"
	defaultVMStateMaxSize = int64(64 << 30)
)

var (
	qemuCheckpointHelperPath = "/usr/local/bin/qemu-checkpoint-helper"
	vmStateLoaderPath        = "/usr/local/bin/vmstate-loader"
)

type qemuCapture struct {
	workDir        string
	payloadPath    string
	manifestPath   string
	imageArchive   string
	payloadDigest  string
	payloadSize    int64
	manifestDigest string
	manifest       snapshot.VMStateManifest
}

func runSnapshotRequest(args []string, recovery *snapshotRecovery) error {
	request, err := parseSnapshotRequest(args)
	if err != nil {
		return err
	}
	switch request.Provider {
	case snapshot.ProviderQEMU:
		return runQEMUSnapshot(request, recovery)
	default:
		return fmt.Errorf("unsupported snapshot provider %q", request.Provider)
	}
}

func runQEMUSnapshot(request snapshot.Request, recovery *snapshotRecovery) error {
	if request.QEMU == nil {
		return errors.New("QEMU snapshot request is missing qemu configuration")
	}

	containerIDs := make(map[string]string, len(request.Containers))
	for _, container := range request.Containers {
		containerID, err := getContainerIDByNerdctl(request.PodName, request.Namespace, request.PodUID, container.Name)
		if err != nil {
			return fmt.Errorf("find container %q: %w", container.Name, err)
		}
		containerIDs[container.Name] = containerID
	}
	qemuContainerID, ok := containerIDs[request.QEMU.ContainerName]
	if !ok {
		return fmt.Errorf("QEMU container %q is not included in snapshot containers", request.QEMU.ContainerName)
	}

	capture, err := captureQEMUState(qemuContainerID, *request.QEMU, recovery)
	if err != nil {
		return err
	}
	defer os.RemoveAll(capture.workDir)

	for _, container := range request.Containers {
		containerID := containerIDs[container.Name]
		if err := pauseContainer(containerID); err != nil {
			return fmt.Errorf("pause container %q after QEMU checkpoint: %w", container.Name, err)
		}
		recovery.trackPausedContainer(containerID)
	}

	for _, container := range request.Containers {
		if err := commitContainer(containerIDs[container.Name], container.ImageURI); err != nil {
			return fmt.Errorf("commit container %q: %w", container.Name, err)
		}
	}

	if _, err := registryimage.Build(
		capture.imageArchive,
		request.VMStateImageURI,
		vmStateLoaderPath,
		capture.manifestPath,
		capture.payloadPath,
	); err != nil {
		return fmt.Errorf("build VM state image: %w", err)
	}
	if err := loadImageArchive(capture.imageArchive); err != nil {
		return err
	}

	for _, container := range request.Containers {
		if err := pushImage(container.ImageURI); err != nil {
			return fmt.Errorf("push container image %q: %w", container.ImageURI, err)
		}
	}
	if err := pushImage(request.VMStateImageURI); err != nil {
		return fmt.Errorf("push VM state image: %w", err)
	}

	result := snapshot.Result{Containers: make([]snapshot.ContainerResult, 0, len(request.Containers))}
	for _, container := range request.Containers {
		digest, err := getImageDigest(container.ImageURI)
		if err != nil {
			return err
		}
		result.Containers = append(result.Containers, snapshot.ContainerResult{
			Name: container.Name, Image: container.ImageURI, Digest: digest,
		})
	}
	vmImageDigest, err := getImageDigest(request.VMStateImageURI)
	if err != nil {
		return err
	}
	result.VirtualMachine = &snapshot.VMStateResult{
		ImageURI:       request.VMStateImageURI,
		ImageDigest:    vmImageDigest,
		PayloadDigest:  capture.payloadDigest,
		SizeBytes:      capture.payloadSize,
		Compression:    snapshot.VMStateCompressionZstd,
		ManifestDigest: capture.manifestDigest,
		Compatibility:  capture.manifest.Compatibility,
	}
	if err := writeSnapshotRequestResult(result); err != nil {
		return err
	}

	if request.LeaveSourceFrozen {
		// The source Pod is deleted by the pause controller after it observes this
		// result. Keep it frozen so a supervisor cannot restart the postmigrate VM.
		recovery.disarm()
		return nil
	}

	// A standalone public snapshot does not own the source Pod lifecycle. Resume
	// both the outer container and QEMU after the registry artifacts are durable.
	if err := recovery.resumeSource(); err != nil {
		return fmt.Errorf("resume source after standalone snapshot: %w", err)
	}
	return nil
}

func parseSnapshotRequest(args []string) (snapshot.Request, error) {
	flags := flag.NewFlagSet("snapshot", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	encoded := flags.String("request-base64", "", "base64 encoded snapshot request")
	if err := flags.Parse(args); err != nil {
		return snapshot.Request{}, err
	}
	if *encoded == "" || flags.NArg() != 0 {
		return snapshot.Request{}, errors.New("snapshot requires exactly --request-base64")
	}
	data, err := base64.StdEncoding.DecodeString(*encoded)
	if err != nil {
		return snapshot.Request{}, fmt.Errorf("decode snapshot request: %w", err)
	}
	var request snapshot.Request
	if err := json.Unmarshal(data, &request); err != nil {
		return snapshot.Request{}, fmt.Errorf("parse snapshot request: %w", err)
	}
	if request.Version != snapshot.RequestVersionV1 {
		return snapshot.Request{}, fmt.Errorf("unsupported snapshot request version %q", request.Version)
	}
	if request.PodName == "" || request.Namespace == "" || len(request.Containers) == 0 {
		return snapshot.Request{}, errors.New("snapshot request requires podName, namespace, and containers")
	}
	seen := make(map[string]struct{}, len(request.Containers))
	for _, container := range request.Containers {
		if container.Name == "" || container.ImageURI == "" {
			return snapshot.Request{}, errors.New("snapshot container name and imageUri are required")
		}
		if _, ok := seen[container.Name]; ok {
			return snapshot.Request{}, fmt.Errorf("duplicate snapshot container %q", container.Name)
		}
		seen[container.Name] = struct{}{}
	}
	if request.Provider == snapshot.ProviderQEMU {
		if request.QEMU == nil || request.VMStateImageURI == "" {
			return snapshot.Request{}, errors.New("QEMU snapshot requires qemu configuration and vmStateImageUri")
		}
		if request.QEMU.ContainerName == "" || request.QEMU.QMPSocketPath == "" || request.QEMU.LaunchManifestPath == "" {
			return snapshot.Request{}, errors.New("QEMU snapshot contract is incomplete")
		}
	}
	return request, nil
}

func captureQEMUState(containerID string, request snapshot.QEMURequest, recovery *snapshotRecovery) (*qemuCapture, error) {
	workRoot := os.Getenv("SNAPSHOT_VMSTATE_WORK_DIR")
	if workRoot == "" {
		workRoot = defaultVMStateWorkDir
	}
	if err := os.MkdirAll(workRoot, 0750); err != nil {
		return nil, fmt.Errorf("create VM state work directory: %w", err)
	}
	workDir, err := os.MkdirTemp(workRoot, "qemu-vmstate-")
	if err != nil {
		return nil, fmt.Errorf("create QEMU checkpoint directory: %w", err)
	}
	capture := &qemuCapture{
		workDir:      workDir,
		payloadPath:  filepath.Join(workDir, snapshot.VMStatePayloadFilename),
		manifestPath: filepath.Join(workDir, snapshot.VMStateManifestFilename),
		imageArchive: filepath.Join(workDir, "vmstate-image.tar"),
	}

	remoteHelper := fmt.Sprintf("/tmp/.opensandbox-qemu-checkpoint-helper-%d", os.Getpid())
	if err := copyIntoContainer(containerID, qemuCheckpointHelperPath, remoteHelper); err != nil {
		os.RemoveAll(workDir)
		return nil, err
	}
	defer removeContainerFile(containerID, remoteHelper)

	launchPath := filepath.Join(workDir, "launch.json")
	if err := copyFromContainer(containerID, request.LaunchManifestPath, launchPath); err != nil {
		os.RemoveAll(workDir)
		return nil, err
	}
	launchData, err := os.ReadFile(launchPath)
	if err != nil {
		os.RemoveAll(workDir)
		return nil, err
	}
	var launch snapshot.QEMULaunchManifest
	if err := json.Unmarshal(launchData, &launch); err != nil {
		os.RemoveAll(workDir)
		return nil, fmt.Errorf("decode QEMU launch manifest: %w", err)
	}
	if err := launch.Validate(); err != nil {
		os.RemoveAll(workDir)
		return nil, err
	}
	resolvePath := func(path string) (string, error) {
		output, err := runInContainer(containerID, remoteHelper, "resolve-path", path)
		if err != nil {
			return "", err
		}
		resolved := strings.TrimSpace(string(output))
		if resolved == "" {
			return "", fmt.Errorf("resolved container path %q is empty", path)
		}
		return resolved, nil
	}
	if err := validateRootfsDiskCapture(launch.Disks, request.VolumeMountPaths, resolvePath); err != nil {
		os.RemoveAll(workDir)
		return nil, err
	}

	probeOutput, err := runInContainer(containerID, remoteHelper, "probe", "--socket", request.QMPSocketPath, "--timeout", "30s")
	if err != nil {
		os.RemoveAll(workDir)
		return nil, fmt.Errorf("probe QEMU: %w", err)
	}
	var probe struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(probeOutput), &probe); err != nil {
		os.RemoveAll(workDir)
		return nil, fmt.Errorf("decode QEMU probe response: %w", err)
	}
	if probe.Version != launch.QEMUVersion {
		os.RemoveAll(workDir)
		return nil, fmt.Errorf("QEMU version mismatch: launch manifest has %q, process reports %q", launch.QEMUVersion, probe.Version)
	}

	// Install recovery before migration starts: an interrupted or failed export
	// may already have moved QEMU into a non-running migration state.
	recovery.setVirtualMachineResume(func() error {
		return resumeSourceQEMU(containerID, request.QMPSocketPath)
	})
	if err := exportCompressedMigration(containerID, remoteHelper, request.QMPSocketPath, capture.payloadPath); err != nil {
		os.RemoveAll(workDir)
		return nil, err
	}
	capture.payloadDigest, capture.payloadSize, err = digestFile(capture.payloadPath)
	if err != nil {
		os.RemoveAll(workDir)
		return nil, err
	}
	capture.manifest = snapshot.VMStateManifest{
		FormatVersion: snapshot.VMStateFormatVersion1,
		PayloadDigest: capture.payloadDigest,
		PayloadSize:   capture.payloadSize,
		Compression:   snapshot.VMStateCompressionZstd,
		Compatibility: launch.Compatibility(request.RequiredNodeClass),
		Disks:         launch.Disks,
	}
	manifestData, err := json.Marshal(capture.manifest)
	if err != nil {
		os.RemoveAll(workDir)
		return nil, err
	}
	if err := os.WriteFile(capture.manifestPath, manifestData, 0640); err != nil {
		os.RemoveAll(workDir)
		return nil, err
	}
	capture.manifestDigest = digestBytes(manifestData)
	return capture, nil
}

func exportCompressedMigration(containerID, helperPath, qmpSocket, outputPath string) error {
	output, err := os.Create(outputPath)
	if err != nil {
		return err
	}
	maxBytes, err := vmStateMaxBytes()
	if err != nil {
		output.Close()
		return err
	}
	limited := &limitedWriter{writer: output, remaining: maxBytes}
	encoder, err := zstd.NewWriter(limited, zstd.WithEncoderConcurrency(1))
	if err != nil {
		output.Close()
		return err
	}

	args := append(nerdctlBaseArgs(), "exec", containerID, helperPath, "export", "--socket", qmpSocket, "--timeout", "10m")
	command := exec.Command("nerdctl", args...)
	command.Stdout = encoder
	var stderr bytes.Buffer
	command.Stderr = &stderr
	runErr := command.Run()
	closeEncoderErr := encoder.Close()
	closeFileErr := output.Close()
	if runErr != nil {
		return fmt.Errorf("export QEMU migration: %w, output: %s", runErr, strings.TrimSpace(stderr.String()))
	}
	if closeEncoderErr != nil {
		return fmt.Errorf("compress QEMU migration: %w", closeEncoderErr)
	}
	if closeFileErr != nil {
		return closeFileErr
	}
	return nil
}

func resumeSourceQEMU(containerID, qmpSocket string) error {
	remoteHelper := fmt.Sprintf("/tmp/.opensandbox-qemu-recovery-helper-%d", os.Getpid())
	if err := copyIntoContainer(containerID, qemuCheckpointHelperPath, remoteHelper); err != nil {
		return err
	}
	defer removeContainerFile(containerID, remoteHelper)
	_, err := runInContainer(containerID, remoteHelper, "resume", "--socket", qmpSocket, "--timeout", "30s")
	return err
}

func runRecoverQEMU(args []string) {
	if len(args) < 5 {
		fmt.Fprintln(os.Stderr, "Usage: image-committer recover-qemu <pod_name> <namespace> <qemu_container> <qmp_socket> <container_name> [container_name...]")
		os.Exit(2)
	}
	podName, namespace := args[0], args[1]
	podUID := strings.TrimSpace(os.Getenv("SOURCE_POD_UID"))
	qemuContainerName, qmpSocket := args[2], args[3]
	containerNames := args[4:]
	containerIDs := make(map[string]string, len(containerNames))
	errorsSeen := 0
	for _, containerName := range containerNames {
		containerID, err := getContainerIDByNerdctl(podName, namespace, podUID, containerName)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: failed to find container %q: %v\n", containerName, err)
			errorsSeen++
			continue
		}
		containerIDs[containerName] = containerID
		if err := resumeContainer(containerID); err != nil {
			// A worker may have recovered the container before this best-effort Job.
			fmt.Fprintf(os.Stderr, "WARNING: container %q was not unpaused: %v\n", containerName, err)
		}
	}
	qemuContainerID, ok := containerIDs[qemuContainerName]
	if !ok {
		fmt.Fprintf(os.Stderr, "ERROR: QEMU container %q was not found\n", qemuContainerName)
		errorsSeen++
	} else if err := resumeSourceQEMU(qemuContainerID, qmpSocket); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: failed to resume source QEMU: %v\n", err)
		errorsSeen++
	}
	if errorsSeen > 0 {
		os.Exit(1)
	}
}

func copyIntoContainer(containerID, sourcePath, targetPath string) error {
	args := append(nerdctlBaseArgs(), "cp", sourcePath, containerID+":"+targetPath)
	output, err := commandCombinedOutput("nerdctl", args...)
	if err != nil {
		return fmt.Errorf("copy checkpoint helper into container: %w, output: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func copyFromContainer(containerID, sourcePath, targetPath string) error {
	args := append(nerdctlBaseArgs(), "cp", containerID+":"+sourcePath, targetPath)
	output, err := commandCombinedOutput("nerdctl", args...)
	if err != nil {
		return fmt.Errorf("copy QEMU launch manifest from container: %w, output: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func removeContainerFile(containerID, path string) {
	_, _ = runInContainer(containerID, "rm", "-f", path)
}

func runInContainer(containerID string, commandAndArgs ...string) ([]byte, error) {
	args := append(nerdctlBaseArgs(), "exec", containerID)
	args = append(args, commandAndArgs...)
	output, err := commandCombinedOutput("nerdctl", args...)
	if err != nil {
		return nil, fmt.Errorf("nerdctl exec failed: %w, output: %s", err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

type pathResolver func(string) (string, error)

func validateRootfsDiskCapture(disks []snapshot.QEMUDisk, volumeMountPaths []string, resolve pathResolver) error {
	resolvedMounts := make([]string, 0, len(volumeMountPaths))
	for _, mountPath := range volumeMountPaths {
		resolved, err := resolve(mountPath)
		if err != nil {
			return fmt.Errorf("resolve volume mount %q in source container: %w", mountPath, err)
		}
		resolvedMounts = append(resolvedMounts, resolved)
	}
	for _, disk := range disks {
		resolvedOverlay, err := resolve(disk.OverlayPath)
		if err != nil {
			return fmt.Errorf("resolve QEMU disk %q writable overlay %q in source container: %w", disk.ID, disk.OverlayPath, err)
		}
		for i, mountPath := range resolvedMounts {
			if pathWithinMount(resolvedOverlay, mountPath) {
				return fmt.Errorf("QEMU disk %q writable overlay %q resolves to %q under volume mount %q (resolved to %q); qemu-v1 requires it in the container rootfs", disk.ID, disk.OverlayPath, resolvedOverlay, volumeMountPaths[i], mountPath)
			}
		}
	}
	return nil
}

func pathWithinMount(path, mountPath string) bool {
	if path == "" || mountPath == "" {
		return false
	}
	relative, err := filepath.Rel(filepath.Clean(mountPath), filepath.Clean(path))
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func loadImageArchive(path string) error {
	args := append(nerdctlBaseArgs(), "image", "load", "--input", path)
	output, err := commandCombinedOutput("nerdctl", args...)
	if err != nil {
		return fmt.Errorf("load VM state image: %w, output: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func writeSnapshotRequestResult(result snapshot.Result) error {
	data, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return os.WriteFile(terminationMessagePath, append(data, '\n'), 0644)
}

func digestFile(path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return "", 0, err
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), size, nil
}

func digestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func vmStateMaxBytes() (int64, error) {
	raw := strings.TrimSpace(os.Getenv("SNAPSHOT_VMSTATE_MAX_BYTES"))
	if raw == "" {
		return defaultVMStateMaxSize, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("invalid SNAPSHOT_VMSTATE_MAX_BYTES %q", raw)
	}
	return value, nil
}

type limitedWriter struct {
	writer    io.Writer
	remaining int64
}

func (w *limitedWriter) Write(data []byte) (int, error) {
	if int64(len(data)) > w.remaining {
		return 0, fmt.Errorf("compressed VM state exceeds configured limit")
	}
	n, err := w.writer.Write(data)
	w.remaining -= int64(n)
	return n, err
}
