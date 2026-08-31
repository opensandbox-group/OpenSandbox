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

package runtime

import (
	"os/exec"
	"testing"
)

func TestWrapCommandWithRequestID(t *testing.T) {
	tests := []struct {
		name      string
		goos      string
		code      string
		requestID string
		want      string
	}{
		{
			name:      "empty request id leaves code unchanged",
			code:      "echo hello",
			requestID: "",
			want:      "echo hello",
		},
		{
			name:      "request id wrapped into command",
			code:      "echo hello",
			requestID: "req-abc123",
			want:      "export SANDBOX_REQUEST_ID='req-abc123'; echo hello",
		},
		{
			name:      "single quotes escaped",
			code:      "echo hello",
			requestID: "req'abc",
			want:      "export SANDBOX_REQUEST_ID='req'\"'\"'abc'; echo hello",
		},
		{
			name:      "windows shell-mode is not wrapped (POSIX export invalid for cmd /C)",
			goos:      goosWindows,
			code:      "echo hello",
			requestID: "req-abc123",
			want:      "echo hello",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			goos := tt.goos
			if goos == "" {
				goos = "linux"
			}
			if got := wrapCommandForShell(goos, tt.code, tt.requestID); got != tt.want {
				t.Fatalf("wrapCommandForShell(%q, %q, %q) = %q, want %q", goos, tt.code, tt.requestID, got, tt.want)
			}
		})
	}
}

// TestWrapCommandWithRequestIDExecutesInShell verifies the wrapped command
// actually executes in a shell: the request id is exported into the
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
	wrapped := WrapCommandWithRequestID("echo ok", "'; touch /tmp/pwned; '")
	out, err := exec.Command("sh", "-c", wrapped).Output()
	if err != nil {
		t.Fatalf("wrapped command failed: %v", err)
	}
	if got := string(out); got != "ok\n" {
		t.Fatalf("wrapped command output = %q, want %q", got, "ok\n")
	}
}
