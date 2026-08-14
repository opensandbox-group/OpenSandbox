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

package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGetImageDigestReturnsErrorOnInspectFailure(t *testing.T) {
	original := commandCombinedOutput
	t.Cleanup(func() { commandCombinedOutput = original })
	commandCombinedOutput = func(_ string, _ ...string) ([]byte, error) {
		return []byte("inspect failed"), errors.New("exit status 1")
	}

	digest, err := getImageDigest("registry.example.com/test/image:snap")

	if err == nil {
		t.Fatal("expected digest extraction error")
	}
	if digest != "" {
		t.Fatalf("expected empty digest on error, got %q", digest)
	}
	if digest == "sha256:placeholder" {
		t.Fatal("digest extraction must not return placeholder")
	}
}

func TestGetImageDigestReturnsErrorOnEmptyInspectOutput(t *testing.T) {
	original := commandCombinedOutput
	t.Cleanup(func() { commandCombinedOutput = original })
	commandCombinedOutput = func(_ string, _ ...string) ([]byte, error) {
		return []byte(" \n"), nil
	}

	digest, err := getImageDigest("registry.example.com/test/image:snap")

	if err == nil {
		t.Fatal("expected empty digest error")
	}
	if digest != "" {
		t.Fatalf("expected empty digest on error, got %q", digest)
	}
}

func TestGetImageDigestReturnsDigest(t *testing.T) {
	original := commandCombinedOutput
	t.Cleanup(func() { commandCombinedOutput = original })
	commandCombinedOutput = func(_ string, _ ...string) ([]byte, error) {
		return []byte("sha256:abc123\n"), nil
	}

	digest, err := getImageDigest("registry.example.com/test/image:snap")

	if err != nil {
		t.Fatalf("expected digest extraction to succeed, got %v", err)
	}
	if digest != "sha256:abc123" {
		t.Fatalf("unexpected digest %q", digest)
	}
}

func TestGetContainerIDByNerdctlReturnsRunningContainer(t *testing.T) {
	original := commandCombinedOutput
	t.Cleanup(func() { commandCombinedOutput = original })

	calls := 0
	commandCombinedOutput = func(name string, args ...string) ([]byte, error) {
		calls++
		if name != "nerdctl" {
			t.Fatalf("unexpected command %q", name)
		}
		if calls != 1 {
			t.Fatalf("expected a single nerdctl lookup, got %d", calls)
		}
		return []byte("container-running\n"), nil
	}

	container, err := getContainerByNerdctl("pod-1", "default", "sandbox")
	if err != nil {
		t.Fatalf("expected running container lookup to succeed, got %v", err)
	}
	if container.ID != "container-running" {
		t.Fatalf("unexpected container ID %q", container.ID)
	}
	if !container.Running {
		t.Fatal("expected container to be reported as running")
	}
}

func TestGetContainerIDByNerdctlFallsBackToStoppedContainers(t *testing.T) {
	original := commandCombinedOutput
	t.Cleanup(func() { commandCombinedOutput = original })

	var calls [][]string
	commandCombinedOutput = func(name string, args ...string) ([]byte, error) {
		if name != "nerdctl" {
			t.Fatalf("unexpected command %q", name)
		}
		calls = append(calls, append([]string(nil), args...))
		switch len(calls) {
		case 1:
			return []byte("\n"), nil
		case 2:
			return []byte("container-stopped\n"), nil
		default:
			t.Fatalf("unexpected extra nerdctl lookup #%d", len(calls))
			return nil, nil
		}
	}

	container, err := getContainerByNerdctl("pod-1", "default", "sandbox")
	if err != nil {
		t.Fatalf("expected stopped container fallback to succeed, got %v", err)
	}
	if container.ID != "container-stopped" {
		t.Fatalf("unexpected container ID %q", container.ID)
	}
	if container.Running {
		t.Fatal("expected stopped container to be reported as stopped")
	}
	if len(calls) != 2 {
		t.Fatalf("expected two nerdctl lookups, got %d", len(calls))
	}
	if contains(calls[0], "-a") {
		t.Fatalf("first lookup should only inspect running containers: %v", calls[0])
	}
	if !contains(calls[1], "-a") {
		t.Fatalf("second lookup should include stopped containers: %v", calls[1])
	}
}

func TestGetContainerIDByNerdctlReturnsHelpfulErrorWhenBothLookupsAreEmpty(t *testing.T) {
	original := commandCombinedOutput
	t.Cleanup(func() { commandCombinedOutput = original })

	commandCombinedOutput = func(_ string, _ ...string) ([]byte, error) {
		return []byte("\n"), nil
	}

	_, err := getContainerIDByNerdctl("pod-1", "default", "sandbox")
	if err == nil {
		t.Fatal("expected lookup failure when both running and stopped container searches are empty")
	}
	if got := err.Error(); got != "container 'sandbox' not found in pod default/pod-1 (nerdctl ps and nerdctl ps -a returned empty)" {
		t.Fatalf("unexpected error %q", got)
	}
}

func TestGetContainerImageReturnsSourceReferenceAfterWarning(t *testing.T) {
	original := commandCombinedOutput
	t.Cleanup(func() { commandCombinedOutput = original })
	commandCombinedOutput = func(name string, args ...string) ([]byte, error) {
		if name != "nerdctl" {
			t.Fatalf("unexpected command %q", name)
		}
		if !contains(args, "inspect") || !contains(args, "container-1") {
			t.Fatalf("unexpected inspect arguments: %v", args)
		}
		return []byte("time=\"2026-08-13T13:55:17Z\" level=warning msg=\"failed to inspect NetNS\"\nOPENSANDBOX_SOURCE_IMAGE=registry.example.com/quovy/sandbox:release-1\ntime=\"2026-08-13T13:55:18Z\" level=warning msg=\"cleanup warning\"\n"), nil
	}

	image, err := getContainerImage("container-1")
	if err != nil {
		t.Fatalf("expected source image lookup to succeed, got %v", err)
	}
	if image != "registry.example.com/quovy/sandbox:release-1" {
		t.Fatalf("unexpected source image %q", image)
	}
}

func TestPullImageContentFetchesCompressedLayers(t *testing.T) {
	original := commandCombinedOutput
	t.Cleanup(func() { commandCombinedOutput = original })
	t.Setenv("SNAPSHOT_REGISTRY_INSECURE", "false")

	var gotArgs []string
	commandCombinedOutput = func(name string, args ...string) ([]byte, error) {
		if name != "nerdctl" {
			t.Fatalf("unexpected command %q", name)
		}
		gotArgs = append([]string(nil), args...)
		return []byte("pulled"), nil
	}

	image := "registry.example.com/quovy/sandbox:release-1"
	if err := pullImageContent(image); err != nil {
		t.Fatalf("expected source content pull to succeed, got %v", err)
	}
	if !contains(gotArgs, "pull") || !contains(gotArgs, image) {
		t.Fatalf("unexpected pull arguments: %v", gotArgs)
	}
	if !contains(gotArgs, "--all-platforms") {
		t.Fatalf("source pull must bypass containerd's transfer-service cache: %v", gotArgs)
	}
}

func TestPushImagePreservesCommittedManifest(t *testing.T) {
	original := commandCombinedOutput
	t.Cleanup(func() { commandCombinedOutput = original })
	t.Setenv("SNAPSHOT_REGISTRY_INSECURE", "false")

	var gotArgs []string
	commandCombinedOutput = func(name string, args ...string) ([]byte, error) {
		if name != "nerdctl" {
			t.Fatalf("unexpected command %q", name)
		}
		gotArgs = append([]string(nil), args...)
		return []byte("pushed"), nil
	}

	image := "registry.example.com/quovy/sandbox:snapshot-1"
	if err := pushImage(image); err != nil {
		t.Fatalf("expected snapshot push to succeed, got %v", err)
	}
	if !contains(gotArgs, "push") || !contains(gotArgs, image) {
		t.Fatalf("unexpected push arguments: %v", gotArgs)
	}
	if !contains(gotArgs, "--all-platforms") {
		t.Fatalf("snapshot push must preserve the committed manifest: %v", gotArgs)
	}
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func TestSyncRunningContainerFilesystemsSyncsEveryRunningContainerAndSkipsStopped(t *testing.T) {
	original := commandCombinedOutput
	t.Cleanup(func() { commandCombinedOutput = original })
	t.Setenv("CONTAINERD_SOCKET", "/test/containerd.sock")
	t.Setenv("CONTAINERD_NAMESPACE", "test-ns")

	var calls [][]string
	commandCombinedOutput = func(name string, args ...string) ([]byte, error) {
		if name != "nerdctl" {
			t.Fatalf("unexpected command %q", name)
		}
		calls = append(calls, append([]string(nil), args...))
		if contains(args, "container-main") {
			return []byte("guest sync failed"), errors.New("exit status 1")
		}
		return nil, nil
	}

	err := syncRunningContainerFilesystems(
		[]ContainerSpec{{Name: "main"}, {Name: "sidecar"}, {Name: "stopped"}},
		map[string]discoveredContainer{
			"main":    {ID: "container-main", Running: true},
			"sidecar": {ID: "container-sidecar", Running: true},
			"stopped": {ID: "container-stopped"},
		},
	)

	if err == nil {
		t.Fatal("expected a running container sync failure to be reported")
	}
	if !strings.Contains(err.Error(), `container "main"`) || !strings.Contains(err.Error(), "guest sync failed") {
		t.Fatalf("expected contextual sync failure, got %q", err)
	}
	if len(calls) != 2 {
		t.Fatalf("expected every running container and no stopped containers to be synced, got %d calls", len(calls))
	}
	wantMain := []string{"--address", "/test/containerd.sock", "--namespace", "test-ns", "exec", "container-main", "sync"}
	wantSidecar := []string{"--address", "/test/containerd.sock", "--namespace", "test-ns", "exec", "container-sidecar", "sync"}
	for i, want := range [][]string{wantMain, wantSidecar} {
		if strings.Join(calls[i], "\x00") != strings.Join(want, "\x00") {
			t.Fatalf("unexpected nerdctl call %d: got %v, want %v", i+1, calls[i], want)
		}
	}
}

func TestWriteSnapshotResultWritesTerminationMessage(t *testing.T) {
	original := terminationMessagePath
	t.Cleanup(func() { terminationMessagePath = original })
	terminationMessagePath = filepath.Join(t.TempDir(), "termination.log")

	err := writeSnapshotResult(
		[]ContainerSpec{
			{Name: "main", URI: "registry.example.com/main:snap"},
			{Name: "sidecar", URI: "registry.example.com/sidecar:snap"},
		},
		map[string]string{
			"main":    "sha256:main",
			"sidecar": "sha256:sidecar",
		},
	)
	if err != nil {
		t.Fatalf("writeSnapshotResult failed: %v", err)
	}

	data, err := os.ReadFile(terminationMessagePath)
	if err != nil {
		t.Fatalf("failed to read termination message: %v", err)
	}

	var result snapshotResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("termination message is not valid JSON: %v", err)
	}
	if len(result.Containers) != 2 {
		t.Fatalf("expected 2 container results, got %d", len(result.Containers))
	}
	if result.Containers[0].Name != "main" || result.Containers[0].Digest != "sha256:main" {
		t.Fatalf("unexpected first result: %#v", result.Containers[0])
	}
	if result.Containers[1].Name != "sidecar" || result.Containers[1].Digest != "sha256:sidecar" {
		t.Fatalf("unexpected second result: %#v", result.Containers[1])
	}
}
