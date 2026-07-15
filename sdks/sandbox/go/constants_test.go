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

package opensandbox

import "testing"

// TestVersion_MatchesReleasedTag is a regression guard: the Version constant is
// reported in the User-Agent header and must be bumped together with the
// released module tag. Update this expectation when releasing.
func TestVersion_MatchesReleasedTag(t *testing.T) {
	const want = "1.0.4"
	if Version != want {
		t.Fatalf("Version = %q, want %q; bump this together with the release tag", Version, want)
	}
}
