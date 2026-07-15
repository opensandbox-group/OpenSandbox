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

	containerID, err := getContainerIDByNerdctl("pod-1", "default", "sandbox")
	if err != nil {
		t.Fatalf("expected running container lookup to succeed, got %v", err)
	}
	if containerID != "container-running" {
		t.Fatalf("unexpected container ID %q", containerID)
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

	containerID, err := getContainerIDByNerdctl("pod-1", "default", "sandbox")
	if err != nil {
		t.Fatalf("expected stopped container fallback to succeed, got %v", err)
	}
	if containerID != "container-stopped" {
		t.Fatalf("unexpected container ID %q", containerID)
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

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
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
