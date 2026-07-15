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
	"strings"
	"testing"
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
