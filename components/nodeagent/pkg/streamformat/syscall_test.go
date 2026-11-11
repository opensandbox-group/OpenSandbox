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

package streamformat

import (
	"testing"

	"github.com/alibaba/opensandbox/nodeagent/pkg/api"
)

func TestSyscallFormatEncodesNDJSONAndLayout(t *testing.T) {
	resource := api.Resource{ClusterName: "prod-a", Namespace: "team-a", SandboxID: "sb-1", PodUID: "u1", Container: "sandbox"}
	streamRef := api.StreamRef{ID: SyscallStreamID(resource.PodUID, resource.Container), Kind: api.RecordKindSyscall}
	format, _, encoded, err := EncodeBatch(api.Batch{StreamRef: streamRef, Items: []api.BatchItem{{Record: api.Record{Kind: api.RecordKindSyscall, Resource: resource, Body: []byte(`{"schema_version":1,"syscall_nr":63}`)}}}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(encoded), "{\"schema_version\":1,\"syscall_nr\":63}\n"; got != want {
		t.Fatalf("encoded=%q, want %q", got, want)
	}
	family, err := ResolveFamily(format, "", streamRef, resource, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := family.DataKey(0), "prod-a/_streams/syscall/team-a/sb-1/u1/sandbox.syscalls.jsonl"; got != want {
		t.Fatalf("data key=%q, want %q", got, want)
	}
}

func TestSyscallFormatRejectsNonObjectBody(t *testing.T) {
	resource := api.Resource{ClusterName: "prod-a", Namespace: "team-a", SandboxID: "sb-1", PodUID: "u1", Container: "sandbox"}
	streamRef := api.StreamRef{ID: SyscallStreamID(resource.PodUID, resource.Container), Kind: api.RecordKindSyscall}
	_, _, _, err := EncodeBatch(api.Batch{StreamRef: streamRef, Items: []api.BatchItem{{Record: api.Record{Kind: api.RecordKindSyscall, Resource: resource, Body: []byte(`[1]`)}}}})
	if err == nil {
		t.Fatal("non-object syscall body was accepted")
	}
}
