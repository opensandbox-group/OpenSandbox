// Copyright 2026 The OpenSandbox Authors
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

//go:build !windows

package runtime

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestWrapCommandWithRequestIDExecutesInShell verifies the wrapped command
// actually executes in a POSIX shell: the request id is exported into the
// environment and the original code still runs.
func TestWrapCommandWithRequestIDExecutesInShell(t *testing.T) {
	wrapped := WrapCommandWithRequestID("printf '%s' \"$SANDBOX_REQUEST_ID\"", "req-xyz")
	out, err := exec.Command("sh", "-c", wrapped).Output()
	if err != nil {
		t.Fatalf("wrapped command failed: %v", err)
	}
	if got := string(out); got != "req-xyz" {
		t.Fatalf("wrapped command output = %q, want %q", got, "req-xyz")
	}
}

// TestWrapCommandWithRequestIDNeutralizesInjection verifies a hostile
// request id cannot break out of the export and run extra commands.
func TestWrapCommandWithRequestIDNeutralizesInjection(t *testing.T) {
	// The hostile payload tries to create a marker file: escaping correctly
	// contains it as a literal string, so the marker must never appear.
	marker := filepath.Join(t.TempDir(), "pwned")
	wrapped := WrapCommandWithRequestID("echo ok", "'; touch "+marker+"; '")
	out, err := exec.Command("sh", "-c", wrapped).Output()
	if err != nil {
		t.Fatalf("wrapped command failed: %v", err)
	}
	if got := string(out); got != "ok\n" {
		t.Fatalf("wrapped command output = %q, want %q", got, "ok\n")
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("injected command executed: marker %s exists (%v)", marker, statErr)
	}
}
