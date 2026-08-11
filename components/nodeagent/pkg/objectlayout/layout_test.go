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

package objectlayout

import "testing"

func TestObjectFamilyLayout(t *testing.T) {
	family := FamilyPrefix("logs", "prod", "ns", "sb", "uid")
	if family != "logs/prod/ns/sb/uid" {
		t.Fatalf("family=%q", family)
	}
	if got := DataKey(family, "sandbox", 0); got != "logs/prod/ns/sb/uid/sandbox.log" {
		t.Fatalf("generation zero=%q", got)
	}
	if got := DataKey(family, "sandbox", 2); got != "logs/prod/ns/sb/uid/sandbox.2.log" {
		t.Fatalf("generation two=%q", got)
	}
	if got := MarkerPrefix(family, "sandbox"); got != "logs/prod/ns/sb/uid/sandbox.finalized." {
		t.Fatalf("marker prefix=%q", got)
	}
	if got := MarkerKey(family, "sandbox", 3); got != "logs/prod/ns/sb/uid/sandbox.finalized.3.json" {
		t.Fatalf("marker=%q", got)
	}
	if got := StreamRef("uid", "sandbox"); got != "container-logs/uid/sandbox" {
		t.Fatalf("stream ref=%q", got)
	}
}
