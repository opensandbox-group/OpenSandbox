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
	legacyPrefix := "logs/prod/ns/sb/uid"
	if got := DataKey(legacyPrefix, "sandbox", 2); got != "logs/prod/ns/sb/uid/sandbox.2.log" {
		t.Fatalf("legacy generation=%q", got)
	}
	if got := MarkerKey(legacyPrefix, "sandbox", 3); got != "logs/prod/ns/sb/uid/sandbox.finalized.3.json" {
		t.Fatalf("legacy marker=%q", got)
	}
	if got := StreamRef("uid", "sandbox"); got != "container-logs/uid/sandbox" {
		t.Fatalf("legacy stream ref=%q", got)
	}

	family, err := NewFamily("logs", []string{"prod", "ns", "sb", "uid"}, "sandbox", ".log")
	if err != nil {
		t.Fatal(err)
	}
	if got := family.DataKey(0); got != "logs/prod/ns/sb/uid/sandbox.log" {
		t.Fatalf("generation zero=%q", got)
	}
	if got := family.DataKey(2); got != "logs/prod/ns/sb/uid/sandbox.2.log" {
		t.Fatalf("generation two=%q", got)
	}
	if got := family.MarkerKey(3); got != "logs/prod/ns/sb/uid/sandbox.finalized.3.json" {
		t.Fatalf("marker=%q", got)
	}
}

func TestFamilySupportsDistinctStreamFormats(t *testing.T) {
	family, err := NewFamily("logs", []string{"prod", "_streams", "test-audit", "ns", "sb", "uid"}, "sandbox.audit", ".jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if got := family.DataKey(0); got != "logs/prod/_streams/test-audit/ns/sb/uid/sandbox.audit.jsonl" {
		t.Fatalf("generation zero=%q", got)
	}
	if got := family.DataKey(2); got != "logs/prod/_streams/test-audit/ns/sb/uid/sandbox.audit.2.jsonl" {
		t.Fatalf("generation two=%q", got)
	}
	if got := family.MarkerKey(3); got != "logs/prod/_streams/test-audit/ns/sb/uid/sandbox.audit.finalized.3.json" {
		t.Fatalf("marker=%q", got)
	}
	unprefixed, err := NewFamily("", []string{"prod", "stream"}, "events", ".bin")
	if err != nil {
		t.Fatal(err)
	}
	prefixed, err := unprefixed.WithPrefix("archive/nodeagent")
	if err != nil {
		t.Fatal(err)
	}
	if got := prefixed.DataKey(0); got != "archive/nodeagent/prod/stream/events.bin" || !prefixed.Within("archive/nodeagent/prod") || unprefixed.Within("archive/nodeagent/prod") {
		t.Fatalf("unexpected prefix result: unprefixed=%q prefixed=%q", unprefixed.DataKey(0), got)
	}
}

func TestFamilyRejectsUnsafeComponents(t *testing.T) {
	for _, test := range []struct {
		prefix, directory, base, extension string
	}{
		{prefix: "../escape", directory: "safe", base: "sandbox", extension: ".log"},
		{prefix: "/absolute", directory: "safe", base: "sandbox", extension: ".log"},
		{prefix: "safe", directory: "..", base: "sandbox", extension: ".log"},
		{prefix: "safe", directory: "safe/escape", base: "sandbox", extension: ".log"},
		{prefix: "safe", directory: "safe", base: "../sandbox", extension: ".log"},
		{prefix: "safe", directory: "safe", base: "sandbox", extension: "log"},
		{prefix: "safe", directory: "safe", base: "sandbox", extension: "../log"},
	} {
		if _, err := NewFamily(test.prefix, []string{test.directory}, test.base, test.extension); err == nil {
			t.Fatalf("NewFamily(%q, %q, %q, %q) succeeded", test.prefix, test.directory, test.base, test.extension)
		}
	}
}

func TestFamilyRejectsDataExtensionThatCollidesWithMarker(t *testing.T) {
	if _, err := NewFamily("", []string{"cluster", "stream"}, "events", ".finalized.1.json"); err == nil {
		t.Fatal("NewFamily() accepted a data extension that collides with Revision 1 marker")
	}
	if _, err := NewFamily("", []string{"cluster", "stream"}, "events", ".finalized.18446744073709551615.json"); err == nil {
		t.Fatal("NewFamily() accepted a data extension that collides with a marker")
	}
}
