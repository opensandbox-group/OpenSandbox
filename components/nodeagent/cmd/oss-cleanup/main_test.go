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

package main

import (
	"bytes"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/nodeagent/pkg/api"
	"github.com/alibaba/opensandbox/nodeagent/pkg/identity"
	"github.com/alibaba/opensandbox/nodeagent/pkg/marker"
	"github.com/alibaba/opensandbox/nodeagent/pkg/objectlayout"
	"github.com/alibaba/opensandbox/nodeagent/pkg/state"
	bolt "go.etcd.io/bbolt"
)

func TestCleanupManifestPersistsAndResumes(t *testing.T) {
	db, err := bolt.Open(filepath.Join(t.TempDir(), "cleanup.db"), 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	key := []byte(taskKey("https://oss.example.com", "bucket", "target", "logs/prod/ns/sb/uid", "sandbox"))
	want := manifest{Endpoint: "https://oss.example.com", Bucket: "bucket", TargetID: "target", FamilyPrefix: "logs/prod/ns/sb/uid", Container: "sandbox", MarkerKeys: []string{"marker"}, DataKeys: []string{"data"}, UnmarkedDataKeys: []string{"data"}, MarkerDigest: "digest", Phase: "markers-deleted"}
	if err := writeManifest(db, key, want); err != nil {
		t.Fatal(err)
	}
	got, found, err := readManifest(db, key)
	if err != nil || !found || got.Phase != want.Phase || got.MarkerDigest != want.MarkerDigest || !sameKeys(got.UnmarkedDataKeys, want.UnmarkedDataKeys) {
		t.Fatalf("manifest=%+v found=%v err=%v", got, found, err)
	}
}

func TestLoadOrRefreshManifestRefreshesOnlyUnappliedPlans(t *testing.T) {
	db, err := bolt.Open(filepath.Join(t.TempDir(), "cleanup.db"), 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	endpoint := "https://oss.example.com"
	bucketName := "bucket"
	targetID := "target"
	familyPrefix := "logs/prod/ns/sb/uid"
	container := "sandbox"
	key := []byte(taskKey(endpoint, bucketName, targetID, familyPrefix, container))
	oldPlan := manifest{Endpoint: endpoint, Bucket: bucketName, TargetID: targetID, FamilyPrefix: familyPrefix, Container: container, MarkerKeys: []string{familyPrefix + "/sandbox.finalized.1.json"}, DataKeys: []string{familyPrefix + "/sandbox.log"}, MarkerDigest: strings.Repeat("0", 64), Phase: "planned"}
	if err := writeManifest(db, key, oldPlan); err != nil {
		t.Fatal(err)
	}
	freshPlan := oldPlan
	freshPlan.MarkerKeys = append(append([]string(nil), oldPlan.MarkerKeys...), familyPrefix+"/sandbox.finalized.2.json")
	freshPlan.DataKeys = []string{familyPrefix + "/sandbox.1.log", familyPrefix + "/sandbox.log"}
	freshPlan.MarkerDigest = strings.Repeat("1", 64)

	buildCalls := 0
	build := func() (manifest, error) {
		buildCalls++
		return freshPlan, nil
	}
	got, err := loadOrRefreshManifest(db, key, false, endpoint, bucketName, targetID, familyPrefix, container, build)
	if err != nil {
		t.Fatal(err)
	}
	if buildCalls != 1 || got.MarkerDigest != freshPlan.MarkerDigest {
		t.Fatalf("refreshed manifest = %+v, build calls = %d", got, buildCalls)
	}
	persisted, found, err := readManifest(db, key)
	if err != nil || !found || persisted.MarkerDigest != freshPlan.MarkerDigest {
		t.Fatalf("persisted manifest = %+v, found = %v, err = %v", persisted, found, err)
	}

	buildCalls = 0
	got, err = loadOrRefreshManifest(db, key, true, endpoint, bucketName, targetID, familyPrefix, container, build)
	if err != nil {
		t.Fatal(err)
	}
	if buildCalls != 0 || got.MarkerDigest != freshPlan.MarkerDigest {
		t.Fatalf("apply manifest = %+v, build calls = %d", got, buildCalls)
	}

	freshPlan.Phase = "markers-deleted"
	if err := writeManifest(db, key, freshPlan); err != nil {
		t.Fatal(err)
	}
	got, err = loadOrRefreshManifest(db, key, false, endpoint, bucketName, targetID, familyPrefix, container, build)
	if err != nil {
		t.Fatal(err)
	}
	if buildCalls != 0 || got.Phase != "markers-deleted" {
		t.Fatalf("resumable manifest = %+v, build calls = %d", got, buildCalls)
	}
}

func TestValidateMarkerIdentityMatchesObjectFamily(t *testing.T) {
	familyPrefix := "logs/prod/ns/sb/uid"
	container := "sandbox"
	resource := api.Resource{SandboxID: "sb", ClusterName: "prod", Namespace: "ns", PodName: "pod", PodUID: "uid", NodeName: "node", Container: container}
	streamRef := objectlayout.StreamRef(resource.PodUID, container)
	request := api.FinalizeRequest{FinalizeID: identity.FinalizeID(streamRef, 1, "target"), TargetID: "target", StreamRef: api.StreamRef{ID: streamRef}, Revision: 1, CoverageStartedAt: time.Date(2026, 7, 23, 9, 58, 0, 0, time.UTC), Resource: resource, FinalizedAt: time.Date(2026, 7, 23, 10, 0, 0, 0, time.UTC)}
	value := marker.New(request, nil)
	key := objectlayout.MarkerKey(familyPrefix, container, 1)
	if err := validateMarkerIdentity(value, key, "target", familyPrefix, container, 1); err != nil {
		t.Fatal(err)
	}

	mutations := []func(*marker.Marker){
		func(value *marker.Marker) { value.TargetID = "other" },
		func(value *marker.Marker) { value.FinalizeID = "sha256:other" },
		func(value *marker.Marker) { value.Revision = 2 },
		func(value *marker.Marker) { value.StreamRef = "container-logs/other/sandbox" },
		func(value *marker.Marker) { value.Resource.ClusterName = "other" },
		func(value *marker.Marker) { value.Resource.Namespace = "other" },
		func(value *marker.Marker) { value.Resource.SandboxID = "other" },
		func(value *marker.Marker) { value.Resource.PodUID = "other" },
		func(value *marker.Marker) { value.Resource.Container = "other" },
	}
	for index, mutate := range mutations {
		changed := value
		mutate(&changed)
		if err := validateMarkerIdentity(changed, key, "target", familyPrefix, container, 1); err == nil {
			t.Fatalf("mutation %d unexpectedly matched object family", index)
		}
	}
	if err := validateMarkerIdentity(value, "logs/other/sandbox.finalized.1.json", "target", familyPrefix, container, 1); err == nil {
		t.Fatal("marker key outside family unexpectedly matched")
	}
	if err := validateMarkerIdentity(value, "prod/ns/sb/uid/sandbox.finalized.1.json", "target", "prod/ns/sb/uid", container, 1); err != nil {
		t.Fatalf("family without an additional base prefix should remain valid: %v", err)
	}
	if err := validateMarkerIdentity(value, "ns/sb/uid/sandbox.finalized.1.json", "target", "ns/sb/uid", container, 1); err == nil {
		t.Fatal("short object family unexpectedly matched")
	}
}

func TestValidateCumulativeMarkers(t *testing.T) {
	resource := api.Resource{SandboxID: "sb", ClusterName: "prod", Namespace: "ns", PodName: "pod", PodUID: "uid", NodeName: "node", Container: "sandbox"}
	request := api.FinalizeRequest{FinalizeID: "f1", TargetID: "target", StreamRef: api.StreamRef{ID: "stream"}, Revision: 1, CoverageStartedAt: time.Date(2026, 7, 23, 9, 58, 0, 0, time.UTC), Resource: resource, FinalizedAt: time.Date(2026, 7, 23, 10, 0, 0, 0, time.UTC)}
	first := marker.New(request, []state.ClosedObject{{Key: "logs/prod/ns/sb/uid/sandbox.log", Generation: 0, Size: 10, CRC64: "1"}})
	request.FinalizeID = "f2"
	request.Revision = 2
	second := marker.New(request, []state.ClosedObject{{Key: "logs/prod/ns/sb/uid/sandbox.log", Generation: 0, Size: 10, CRC64: "1"}, {Key: "logs/prod/ns/sb/uid/sandbox.1.log", Generation: 1, Size: 20, CRC64: "2"}})
	if err := validateCumulative(first, second); err != nil {
		t.Fatal(err)
	}
	second.Objects[0].Size++
	if err := validateCumulative(first, second); err == nil {
		t.Fatal("changed finalized object accepted")
	}
	second = marker.New(request, []state.ClosedObject{{Key: "logs/prod/ns/sb/uid/sandbox.log", Generation: 0, Size: 10, CRC64: "1"}, {Key: "logs/prod/ns/sb/uid/sandbox.1.log", Generation: 1, Size: 20, CRC64: "2"}})
	second.CoverageStartedAt = "2026-07-23T09:59:00Z"
	if err := validateCumulative(first, second); err == nil {
		t.Fatal("changed coverage boundary accepted")
	}
}

func TestNormalizeFamilyPrefix(t *testing.T) {
	if got, err := normalizeFamilyPrefix("/logs/prod/ns/sb/uid/"); err != nil || got != "logs/prod/ns/sb/uid" {
		t.Fatalf("normalizeFamilyPrefix()=%q err=%v", got, err)
	}
	for _, value := range []string{"", "/", "///", ".", "..", "logs//uid", "logs/./uid", "logs/../other"} {
		if _, err := normalizeFamilyPrefix(value); err == nil {
			t.Fatalf("normalizeFamilyPrefix(%q) unexpectedly succeeded", value)
		}
	}
}

func TestCleanupTargetIdentity(t *testing.T) {
	want := manifest{Endpoint: "https://oss.example.com", Bucket: "bucket", TargetID: "target", FamilyPrefix: "logs/prod/ns/sb/uid", Container: "sandbox", MarkerKeys: []string{"logs/prod/ns/sb/uid/sandbox.finalized.1.json"}, DataKeys: []string{"logs/prod/ns/sb/uid/sandbox.log"}, MarkerDigest: strings.Repeat("0", 64), Phase: "planned"}
	if err := validateManifest(want, want.Endpoint, want.Bucket, want.TargetID, want.FamilyPrefix, want.Container); err != nil {
		t.Fatal(err)
	}
	if err := validateManifest(want, want.Endpoint, "other-bucket", want.TargetID, want.FamilyPrefix, want.Container); err == nil {
		t.Fatal("manifest identity accepted a different bucket")
	}
	base := taskKey(want.Endpoint, want.Bucket, want.TargetID, want.FamilyPrefix, want.Container)
	if base == taskKey("https://other.example.com", want.Bucket, want.TargetID, want.FamilyPrefix, want.Container) || base == taskKey(want.Endpoint, "other-bucket", want.TargetID, want.FamilyPrefix, want.Container) {
		t.Fatal("cleanup task key did not bind endpoint and bucket")
	}
}

func TestValidateManifestRejectsUnsafeState(t *testing.T) {
	base := manifest{Endpoint: "https://oss.example.com", Bucket: "bucket", TargetID: "target", FamilyPrefix: "logs/prod/ns/sb/uid", Container: "sandbox", MarkerKeys: []string{"logs/prod/ns/sb/uid/sandbox.finalized.1.json"}, DataKeys: []string{"logs/prod/ns/sb/uid/sandbox.log"}, MarkerDigest: strings.Repeat("0", 64), Phase: "planned"}
	for _, mutate := range []func(*manifest){
		func(value *manifest) { value.Phase = "markers-gone-maybe" },
		func(value *manifest) { value.MarkerKeys[0] = "logs/other/sandbox.finalized.1.json" },
		func(value *manifest) { value.DataKeys[0] = "logs/other/sandbox.log" },
		func(value *manifest) { value.UnmarkedDataKeys = []string{"logs/prod/ns/sb/uid/sandbox.1.log"} },
		func(value *manifest) { value.MarkerDigest = "bad" },
	} {
		value := base
		value.MarkerKeys = append([]string(nil), base.MarkerKeys...)
		value.DataKeys = append([]string(nil), base.DataKeys...)
		mutate(&value)
		if err := validateManifest(value, base.Endpoint, base.Bucket, base.TargetID, base.FamilyPrefix, base.Container); err == nil {
			t.Fatalf("validateManifest() accepted unsafe state: %+v", value)
		}
	}
}

func TestValidateContainer(t *testing.T) {
	if err := validateContainer("sandbox"); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"", ".", "..", "a/b", `a\b`} {
		if err := validateContainer(value); err == nil {
			t.Fatalf("validateContainer(%q) unexpectedly succeeded", value)
		}
	}
}

func TestMarkerDeletionOrderIsNewestFirst(t *testing.T) {
	got := reversedKeys([]string{"revision-1", "revision-2", "revision-3"})
	want := []string{"revision-3", "revision-2", "revision-1"}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("reversedKeys()=%v", got)
		}
	}
}

func TestMarkerPatternAcceptsOnlyCanonicalRevisions(t *testing.T) {
	familyPrefix := "logs/prod/ns/sb/uid"
	container := "sandbox"
	pattern := markerKeyPattern(familyPrefix, container)
	canonical := objectlayout.MarkerKey(familyPrefix, container, 12)
	if !pattern.MatchString(canonical) {
		t.Fatalf("canonical marker %q did not match", canonical)
	}
	for _, key := range []string{
		objectlayout.MarkerPrefix(familyPrefix, container) + "0.json",
		objectlayout.MarkerPrefix(familyPrefix, container) + "01.json",
		objectlayout.MarkerPrefix(familyPrefix, container) + "nested/sandbox.finalized.1.json",
		familyPrefix + "/other.finalized.1.json",
	} {
		if pattern.MatchString(key) {
			t.Fatalf("non-canonical marker %q matched", key)
		}
	}
	if revision, err := markerRevision(pattern, canonical); err != nil || revision != 12 {
		t.Fatalf("revision=%d err=%v", revision, err)
	}
	overflow := objectlayout.MarkerPrefix(familyPrefix, container) + strings.Repeat("9", 32) + ".json"
	if !pattern.MatchString(overflow) {
		t.Fatalf("overflow marker %q should reach revision validation", overflow)
	}
	if _, err := markerRevision(pattern, overflow); err == nil {
		t.Fatalf("overflow marker %q was accepted", overflow)
	}
}

func TestMergeRemainingDataKeysRequiresExplicitExtension(t *testing.T) {
	plan := testCleanupPlan("markers-deleted")
	lateKey := objectlayout.DataKey(plan.FamilyPrefix, plan.Container, 1)
	if changed, err := mergeRemainingDataKeys(&plan, []string{lateKey}, false); err == nil || changed {
		t.Fatalf("unplanned data changed manifest without consent: changed=%v err=%v", changed, err)
	}
	if containsKey(plan.DataKeys, lateKey) || plan.Phase != "markers-deleted" {
		t.Fatalf("rejected extension mutated plan=%+v", plan)
	}
	if changed, err := mergeRemainingDataKeys(&plan, []string{lateKey, lateKey}, true); err != nil || !changed {
		t.Fatalf("explicit extension changed=%v err=%v", changed, err)
	}
	if len(plan.DataKeys) != 2 || len(plan.UnmarkedDataKeys) != 1 || !containsKey(plan.DataKeys, lateKey) || !containsKey(plan.UnmarkedDataKeys, lateKey) {
		t.Fatalf("explicit extension did not record unmarked data: %+v", plan)
	}
	if changed, err := mergeRemainingDataKeys(&plan, []string{lateKey}, false); err != nil || changed {
		t.Fatalf("known data changed manifest: changed=%v err=%v", changed, err)
	}
}

func TestMergeRemainingDataKeysOnlyReopensUnfinishedPlan(t *testing.T) {
	plan := testCleanupPlan("objects-deleted")
	if changed, err := mergeRemainingDataKeys(&plan, plan.DataKeys, false); err == nil || !strings.Contains(err.Error(), "reappeared") || changed || plan.Phase != "objects-deleted" {
		t.Fatalf("deleted phase reopened without consent: changed=%v err=%v plan=%+v", changed, err, plan)
	}
	lateKey := objectlayout.DataKey(plan.FamilyPrefix, plan.Container, 1)
	if changed, err := mergeRemainingDataKeys(&plan, []string{lateKey}, false); err == nil || !strings.Contains(err.Error(), "unplanned") || changed || plan.Phase != "objects-deleted" {
		t.Fatalf("new object was reported as reappeared: changed=%v err=%v plan=%+v", changed, err, plan)
	}
	if changed, err := mergeRemainingDataKeys(&plan, plan.DataKeys, true); err != nil || !changed || plan.Phase != "markers-deleted" {
		t.Fatalf("explicit resume failed: changed=%v err=%v plan=%+v", changed, err, plan)
	}
	if len(plan.UnmarkedDataKeys) != 0 {
		t.Fatalf("authorized data became unmarked: %+v", plan.UnmarkedDataKeys)
	}

	complete := testCleanupPlan("complete")
	if changed, err := mergeRemainingDataKeys(&complete, complete.DataKeys, true); err == nil || changed || complete.Phase != "complete" {
		t.Fatalf("completed cleanup reopened: changed=%v err=%v plan=%+v", changed, err, complete)
	}
	if changed, err := mergeRemainingDataKeys(&complete, nil, true); err == nil || changed || complete.Phase != "complete" {
		t.Fatalf("empty completed cleanup was accepted: changed=%v err=%v plan=%+v", changed, err, complete)
	}
}

func TestPrintManifestListsKeysAndLatestMarkerCoverage(t *testing.T) {
	plan := testCleanupPlan("planned")
	unmarked := objectlayout.DataKey(plan.FamilyPrefix, plan.Container, 1)
	plan.DataKeys = append(plan.DataKeys, unmarked)
	sort.Strings(plan.DataKeys)
	plan.UnmarkedDataKeys = []string{unmarked}
	var output bytes.Buffer
	printManifest(&output, plan)
	text := output.String()
	for _, want := range []string{
		"marker key=\"" + plan.MarkerKeys[0] + "\"",
		"data key=\"" + objectlayout.DataKey(plan.FamilyPrefix, plan.Container, 0) + "\" latest-marker=covered",
		"data key=\"" + unmarked + "\" latest-marker=not-covered",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("output %q does not contain %q", text, want)
		}
	}
}

func testCleanupPlan(phase string) manifest {
	familyPrefix := "logs/prod/ns/sb/uid"
	container := "sandbox"
	return manifest{
		Endpoint:     "https://oss.example.com",
		Bucket:       "bucket",
		TargetID:     "target",
		FamilyPrefix: familyPrefix,
		Container:    container,
		MarkerKeys:   []string{objectlayout.MarkerKey(familyPrefix, container, 1)},
		DataKeys:     []string{objectlayout.DataKey(familyPrefix, container, 0)},
		MarkerDigest: strings.Repeat("0", 64),
		Phase:        phase,
	}
}
