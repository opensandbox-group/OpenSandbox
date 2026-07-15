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

package telemetry

import "testing"

func TestAppendAttrsFromKeyValuePairs(t *testing.T) {
	t.Parallel()

	attrs := AppendAttrsFromKeyValuePairs(nil, "tenant=t1, zone=cn-hz,invalid,noeq=, =blank")
	if len(attrs) != 2 {
		t.Fatalf("attrs len = %d, want 2", len(attrs))
	}

	if string(attrs[0].Key) != "tenant" || attrs[0].Value.AsString() != "t1" {
		t.Fatalf("unexpected first attr: %v", attrs[0])
	}
	if string(attrs[1].Key) != "zone" || attrs[1].Value.AsString() != "cn-hz" {
		t.Fatalf("unexpected second attr: %v", attrs[1])
	}
}

func TestSharedAttrsFromEnv(t *testing.T) {
	t.Setenv("OSBX_SANDBOX_ID", "sb-123")
	t.Setenv("OSBX_EXTRA_ATTRS", "tenant=t1,env=dev")

	attrs := SharedAttrsFromEnv(SharedAttrsEnvConfig{
		SandboxIDEnv:  "OSBX_SANDBOX_ID",
		ExtraAttrsEnv: "OSBX_EXTRA_ATTRS",
		SandboxAttr:   "sandbox_id",
	})
	if len(attrs) != 3 {
		t.Fatalf("attrs len = %d, want 3", len(attrs))
	}
}
