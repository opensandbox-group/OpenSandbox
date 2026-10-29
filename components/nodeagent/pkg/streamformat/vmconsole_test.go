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
	"bytes"
	"testing"

	"github.com/alibaba/opensandbox/nodeagent/pkg/api"
)

func TestVMConsolePreservesBytesAndSeparatesSandboxes(t *testing.T) {
	f := vmConsoleFormat{}
	want := []byte("kernel\r\n\x00stdout\nstderr")
	got, err := f.EncodeBatch(api.Batch{Items: []api.BatchItem{{Record: api.Record{Body: want[:8]}}, {Record: api.Record{Body: want[8:]}}}})
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("console changed: %q, %v", got, err)
	}
	r := api.Resource{ClusterName: "test", Namespace: "ns", SandboxID: "sandbox"}
	first, err := f.ObjectFamily(api.StreamRef{ID: "vm-console/uid", Kind: api.RecordKindVMConsole}, r, api.StreamMetadata{"sandbox-uid": "uid"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.ObjectFamily(api.StreamRef{ID: "vm-console/other-uid", Kind: api.RecordKindVMConsole}, r, api.StreamMetadata{"sandbox-uid": "other-uid"})
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("sandboxes share an output object")
	}
}
