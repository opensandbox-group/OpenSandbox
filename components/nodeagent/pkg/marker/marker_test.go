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

package marker

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/nodeagent/pkg/api"
	"github.com/alibaba/opensandbox/nodeagent/pkg/state"
)

func TestEncodeMarkerIsDeterministic(t *testing.T) {
	request := api.FinalizeRequest{FinalizeID: "sha256:final", TargetID: "sha256:target", StreamRef: api.StreamRef{ID: "container-logs/u123/sandbox"}, Revision: 1, CoverageStartedAt: time.Date(2026, 7, 23, 9, 58, 0, 0, time.UTC), Resource: api.Resource{SandboxID: "sb-abc", ClusterName: "prod-a", Namespace: "team-a", PodName: "pod", PodUID: "u123", NodeName: "node-1", Container: "sandbox"}, Outcome: api.SourceOutcome{HadDrops: true, LossReasons: []string{"z", "a", "a"}}, FinalizedAt: time.Date(2026, 7, 23, 10, 5, 0, 123, time.UTC)}
	value := New(request, []state.ClosedObject{{Key: "key", Generation: 0, Size: 5, CRC64: "7"}})
	first, err := Encode(value)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Encode(value)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) || bytes.HasSuffix(first, []byte("\n")) {
		t.Fatalf("non-canonical output: %q", first)
	}
	if value.Status != "complete-with-drops" || value.FinalizedAt != "2026-07-23T10:05:00Z" {
		t.Fatalf("unexpected marker: %+v", value)
	}
	want := `{"schema_version":1,"target_id":"sha256:target","finalize_id":"sha256:final","revision":1,"stream_ref":"container-logs/u123/sandbox","resource":{"sandbox_id":"sb-abc","k8s.namespace.name":"team-a","k8s.pod.name":"pod","k8s.pod.uid":"u123","k8s.container.name":"sandbox","k8s.node.name":"node-1","k8s.cluster.name":"prod-a"},"coverage_started_at":"2026-07-23T09:58:00Z","status":"complete-with-drops","had_drops":true,"had_source_gaps":false,"loss_reasons":["a","z"],"finalized_at":"2026-07-23T10:05:00Z","objects":[{"key":"key","generation":0,"size":5,"crc64":"7"}]}`
	if string(first) != want {
		t.Fatalf("marker bytes mismatch\n got: %s\nwant: %s", first, want)
	}
}

func TestStatusPriority(t *testing.T) {
	if got := Status(api.SourceOutcome{HadDrops: true, HadSourceGaps: true}); got != "incomplete" {
		t.Fatalf("status=%q", got)
	}
}

func TestEncodeRejectsMissingCoverageBoundary(t *testing.T) {
	request := api.FinalizeRequest{FinalizeID: "f", TargetID: "t", StreamRef: api.StreamRef{ID: "s"}, Revision: 1, Resource: api.Resource{SandboxID: "sb", ClusterName: "c", Namespace: "n", PodName: "p", PodUID: "u", NodeName: "node", Container: "sandbox"}, FinalizedAt: time.Date(2026, 7, 23, 10, 5, 0, 0, time.UTC)}
	if _, err := Encode(New(request, nil)); err == nil || !strings.Contains(err.Error(), "coverage_started_at") {
		t.Fatalf("Encode() error=%v", err)
	}
}

func TestEncodeRejectsSubsecondCoverageBoundary(t *testing.T) {
	request := api.FinalizeRequest{FinalizeID: "f", TargetID: "t", StreamRef: api.StreamRef{ID: "s"}, Revision: 1, CoverageStartedAt: time.Date(2026, 7, 23, 9, 58, 0, 1, time.UTC), Resource: api.Resource{SandboxID: "sb", ClusterName: "c", Namespace: "n", PodName: "p", PodUID: "u", NodeName: "node", Container: "sandbox"}, FinalizedAt: time.Date(2026, 7, 23, 10, 5, 0, 0, time.UTC)}
	if _, err := Encode(New(request, nil)); err == nil || !strings.Contains(err.Error(), "second precision") {
		t.Fatalf("Encode() error=%v", err)
	}
}

func TestEncodeRejectsLossFlagReasonMismatch(t *testing.T) {
	for _, outcome := range []api.SourceOutcome{
		{HadSourceGaps: true},
		{LossReasons: []string{"unknown-loss"}},
	} {
		request := api.FinalizeRequest{FinalizeID: "f", TargetID: "t", StreamRef: api.StreamRef{ID: "s"}, Revision: 1, CoverageStartedAt: time.Date(2026, 7, 23, 9, 58, 0, 0, time.UTC), Resource: api.Resource{SandboxID: "sb", ClusterName: "c", Namespace: "n", PodName: "p", PodUID: "u", NodeName: "node", Container: "sandbox"}, Outcome: outcome, FinalizedAt: time.Date(2026, 7, 23, 10, 5, 0, 0, time.UTC)}
		if _, err := Encode(New(request, nil)); err == nil || !strings.Contains(err.Error(), "loss flags") {
			t.Fatalf("Encode() error=%v", err)
		}
	}
}

func TestCanonicalStringEscapingAndStrictDecode(t *testing.T) {
	request := api.FinalizeRequest{FinalizeID: "f", TargetID: "t", StreamRef: api.StreamRef{ID: "s/\u2028"}, Revision: 1, CoverageStartedAt: time.Date(2026, 7, 23, 9, 58, 0, 0, time.UTC), Resource: api.Resource{SandboxID: "sb", ClusterName: "c", Namespace: "n", PodName: "p", PodUID: "u", NodeName: "node", Container: "sandbox"}, FinalizedAt: time.Date(2026, 7, 23, 10, 5, 0, 0, time.UTC)}
	raw, err := Encode(New(request, nil))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(`\u2028`)) || !bytes.Contains(raw, []byte("s/\u2028")) {
		t.Fatalf("unexpected escaping: %s", raw)
	}
	decoded, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip, err := Encode(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(roundTrip, raw) {
		t.Fatalf("decoded marker did not round-trip\n got: %s\nwant: %s", roundTrip, raw)
	}
	duplicate := strings.Replace(string(raw), `"revision":1`, `"revision":1,"revision":1`, 1)
	if _, err := Decode([]byte(duplicate)); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate member error=%v", err)
	}
	missing := strings.Replace(string(raw), `,"had_drops":false`, "", 1)
	if _, err := Decode([]byte(missing)); err == nil || !strings.Contains(err.Error(), "missing required") {
		t.Fatalf("missing member error=%v", err)
	}
	nullObjects := strings.Replace(string(raw), `"objects":[]`, `"objects":null`, 1)
	if _, err := Decode([]byte(nullObjects)); err == nil || !strings.Contains(err.Error(), "must not be null") {
		t.Fatalf("null member error=%v", err)
	}
	if _, err := Decode(append(raw, '\n')); err == nil {
		t.Fatal("trailing newline accepted")
	}
}

func TestDecodeIgnoresCaseAliasesAndUnknownMembers(t *testing.T) {
	request := api.FinalizeRequest{FinalizeID: "f", TargetID: "t", StreamRef: api.StreamRef{ID: "s"}, Revision: 1, CoverageStartedAt: time.Date(2026, 7, 23, 9, 58, 0, 0, time.UTC), Resource: api.Resource{SandboxID: "sb", ClusterName: "c", Namespace: "n", PodName: "p", PodUID: "u", NodeName: "node", Container: "sandbox"}, FinalizedAt: time.Date(2026, 7, 23, 10, 5, 0, 0, time.UTC)}
	raw, err := Encode(New(request, []state.ClosedObject{{Key: "object-key", Generation: 0, Size: 5, CRC64: "7"}}))
	if err != nil {
		t.Fatal(err)
	}
	withAliases := strings.Replace(string(raw), `"target_id":"t"`, `"target_id":"t","TARGET_ID":"wrong","future_top":{"enabled":true}`, 1)
	withAliases = strings.Replace(withAliases, `"sandbox_id":"sb"`, `"sandbox_id":"sb","Sandbox_ID":"wrong","future_resource":1`, 1)
	withAliases = strings.Replace(withAliases, `"key":"object-key"`, `"key":"object-key","Key":"wrong","future_object":null`, 1)

	value, err := Decode([]byte(withAliases))
	if err != nil {
		t.Fatalf("Decode() rejected unknown members: %v", err)
	}
	if value.TargetID != "t" || value.Resource.SandboxID != "sb" || len(value.Objects) != 1 || value.Objects[0].Key != "object-key" {
		t.Fatalf("case aliases changed known fields: %+v", value)
	}
}

func TestDecodeRejectsExcessiveUnknownMemberNesting(t *testing.T) {
	request := api.FinalizeRequest{FinalizeID: "f", TargetID: "t", StreamRef: api.StreamRef{ID: "s"}, Revision: 1, CoverageStartedAt: time.Date(2026, 7, 23, 9, 58, 0, 0, time.UTC), Resource: api.Resource{SandboxID: "sb", ClusterName: "c", Namespace: "n", PodName: "p", PodUID: "u", NodeName: "node", Container: "sandbox"}, FinalizedAt: time.Date(2026, 7, 23, 10, 5, 0, 0, time.UTC)}
	raw, err := Encode(New(request, nil))
	if err != nil {
		t.Fatal(err)
	}
	nested := strings.Repeat("[", maxJSONNestingDepth+2) + "0" + strings.Repeat("]", maxJSONNestingDepth+2)
	withDeepUnknown := strings.Replace(string(raw), `"target_id":"t"`, `"target_id":"t","future":`+nested, 1)
	if _, err := Decode([]byte(withDeepUnknown)); err == nil || !strings.Contains(err.Error(), "nesting is too deep") {
		t.Fatalf("deep nesting error=%v", err)
	}
}
