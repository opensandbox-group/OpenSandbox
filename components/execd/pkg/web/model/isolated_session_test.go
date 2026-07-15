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

package model

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func baseIsolatedReq() CreateIsolatedSessionRequest {
	return CreateIsolatedSessionRequest{
		Workspace: WorkspaceSpec{Path: "/tmp", Mode: "rw"},
	}
}

func TestCreateIsolatedSessionRequest_Validate_Binds(t *testing.T) {
	tests := []struct {
		name    string
		binds   []BindMount
		wantErr string // substring; "" means no error
	}{
		{"valid absolute src and dest", []BindMount{{Source: "/data/in", Dest: "/mnt/in"}}, ""},
		{"valid src only (dest defaults)", []BindMount{{Source: "/data/in"}}, ""},
		{"valid readonly", []BindMount{{Source: "/data/in", Dest: "/mnt/in", ReadOnly: true}}, ""},
		{"missing source", []BindMount{{Dest: "/mnt/in"}}, "source is required"},
		{"relative source", []BindMount{{Source: "data/in"}}, "must be an absolute path"},
		{"relative dest", []BindMount{{Source: "/data/in", Dest: "mnt/in"}}, "must be an absolute path"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := baseIsolatedReq()
			req.Binds = tt.binds
			err := req.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("expected no error, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

// TestSessionState_JSONRoundtrip verifies the creation-parameter echo fields
// serialize under the JSON keys defined in specs/execd-api.yaml so that
// SDKs (that share model definitions with the spec) deserialize them
// correctly when calling attach(sessionId).
func TestSessionState_JSONRoundtrip(t *testing.T) {
	shareNet := true
	uid := uint32(42)
	gid := uint32(43)
	idleTimeout := 900
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)

	state := SessionState{
		Status:             "active",
		CreatedAt:          now,
		LastRunAt:          now,
		Profile:            "balanced",
		Workspace:          &WorkspaceSpec{Path: "/tmp/ws", Mode: "overlay"},
		ExtraWritable:      []string{"/allowed"},
		Binds:              []BindMount{{Source: "/host/src", Dest: "/mnt/src", ReadOnly: true}},
		ShareNet:           &shareNet,
		EnvPassthrough:     &EnvPassthroughSpec{Mode: "allow", Keys: []string{"HOME"}},
		Uid:                &uid,
		Gid:                &gid,
		UidMode:            "setpriv",
		IdleTimeoutSeconds: &idleTimeout,
	}

	b, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)

	// Every attach-facing field must use the exact JSON key from the spec.
	for _, key := range []string{
		`"profile":"balanced"`,
		`"workspace":{`,
		`"extra_writable":`,
		`"binds":[`,
		`"share_net":true`,
		`"env_passthrough":{`,
		`"uid":42`,
		`"gid":43`,
		`"uid_mode":"setpriv"`,
		`"idle_timeout_seconds":900`,
	} {
		if !strings.Contains(s, key) {
			t.Errorf("marshalled JSON missing %s: %s", key, s)
		}
	}

	// Empty SessionState must omit all creation-parameter fields
	// (backward-compat: older execd builds don't emit them).
	minimal := SessionState{Status: "active", CreatedAt: now, LastRunAt: now}
	b2, err := json.Marshal(minimal)
	if err != nil {
		t.Fatal(err)
	}
	s2 := string(b2)
	for _, key := range []string{
		"profile", "workspace", "extra_writable", "binds", "share_net",
		"env_passthrough", "uid", "gid", "uid_mode", "idle_timeout_seconds",
	} {
		if strings.Contains(s2, key) {
			t.Errorf("empty SessionState should omit %q, got: %s", key, s2)
		}
	}

	// Round-trip: unmarshalling produces identical structure.
	var back SessionState
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Profile != "balanced" || back.Workspace == nil || back.Workspace.Path != "/tmp/ws" {
		t.Errorf("round-trip mismatch: %+v", back)
	}
	if back.ShareNet == nil || !*back.ShareNet {
		t.Errorf("ShareNet lost in round-trip")
	}
	if back.IdleTimeoutSeconds == nil || *back.IdleTimeoutSeconds != 900 {
		t.Errorf("IdleTimeoutSeconds lost in round-trip")
	}
}

// TestSessionState_IdleTimeoutZeroIsMeaningful ensures a session created
// with idle_timeout_seconds explicitly set to 0 (idle GC disabled — the
// long-window recovery configuration) is echoed back as a present-but-zero
// value, not omitted. Older execd builds that don't emit the field are
// distinguished by pointer nil.
func TestSessionState_IdleTimeoutZeroIsMeaningful(t *testing.T) {
	zero := 0
	state := SessionState{
		Status:             "active",
		IdleTimeoutSeconds: &zero,
	}

	b, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	// Present, with value 0 — NOT omitted.
	if !strings.Contains(s, `"idle_timeout_seconds":0`) {
		t.Errorf("expected idle_timeout_seconds:0 in JSON, got: %s", s)
	}

	var back SessionState
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.IdleTimeoutSeconds == nil {
		t.Fatal("IdleTimeoutSeconds should be non-nil (explicit 0)")
	}
	if *back.IdleTimeoutSeconds != 0 {
		t.Errorf("IdleTimeoutSeconds = %d, want 0", *back.IdleTimeoutSeconds)
	}
}
