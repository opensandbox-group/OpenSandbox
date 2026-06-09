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

package isolation

import (
	"testing"
)

func TestParseBwrapVersion(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"bubblewrap 0.8.0\n", "0.8.0"},
		{"bwrap 0.10.0\n", "0.10.0"},
		{"bwrap: unrecognized option '--version'\n", ""},
		{"", ""},
		{"some unrelated output\nbubblewrap 0.11.2-dev\nmore output", "0.11.2"},
	}

	for _, tt := range tests {
		got := parseBwrapVersion(tt.in)
		if got != tt.want {
			t.Errorf("parseBwrapVersion(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestProbeConfigDefaults(t *testing.T) {
	cfg := ProbeConfig{
		UpperRoot:     "/var/lib/execd/isolation",
		UpperMaxBytes: 8 * 1024 * 1024 * 1024,
	}
	if cfg.UpperRoot == "" {
		t.Error("UpperRoot should not be empty")
	}
}

func TestProbeResult_Defaults(t *testing.T) {
	result := ProbeResult{}
	if result.Available {
		t.Error("default ProbeResult should have Available=false")
	}
	if result.CommitSupported {
		t.Error("default ProbeResult should have CommitSupported=false")
	}
	if result.DiffSupported {
		t.Error("default ProbeResult should have DiffSupported=false")
	}
}
