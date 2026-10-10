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

package opensandbox

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSandboxInfoResolvedImageDigest(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	for _, test := range []struct{ payload, want string }{
		{`{"id":"sbx","image":{"uri":"python:3.11"}}`, ""},
		{`{"id":"sbx","resolvedImageDigest":null}`, ""},
		{`{"id":"sbx","resolvedImageDigest":"` + digest + `"}`, digest},
	} {
		var info SandboxInfo
		if err := json.Unmarshal([]byte(test.payload), &info); err != nil {
			t.Fatal(err)
		}
		if info.ResolvedImageDigest != test.want {
			t.Fatalf("got %q, want %q", info.ResolvedImageDigest, test.want)
		}
	}
}
