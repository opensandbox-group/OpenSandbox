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

// nodeagent-oss-cleanup is an offline, marker-aware object-family cleanup
// command. It must never run with the Node Agent ServiceAccount credentials.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/alibaba/opensandbox/nodeagent/pkg/identity"
	"github.com/alibaba/opensandbox/nodeagent/pkg/marker"
	"github.com/alibaba/opensandbox/nodeagent/pkg/objectlayout"
	aliyunoss "github.com/aliyun/aliyun-oss-go-sdk/oss"
	bolt "go.etcd.io/bbolt"
)

type manifest struct {
	Endpoint         string   `json:"endpoint"`
	Bucket           string   `json:"bucket"`
	TargetID         string   `json:"target_id"`
	FamilyPrefix     string   `json:"family_prefix"`
	Container        string   `json:"container"`
	MarkerKeys       []string `json:"marker_keys"`
	DataKeys         []string `json:"data_keys"`
	UnmarkedDataKeys []string `json:"unmarked_data_keys,omitempty"`
	MarkerDigest     string   `json:"marker_digest"`
	Phase            string   `json:"phase"`
}

func main() {
	endpoint := flag.String("endpoint", "", "OSS HTTPS endpoint")
	bucketName := flag.String("bucket", "", "OSS bucket")
	familyPrefix := flag.String("family-prefix", "", "object-family prefix ending at pod UID")
	container := flag.String("container", "sandbox", "container object-family name")
	targetID := flag.String("target-id", "", "expected Node Agent target ID")
	confirmDrain := flag.String("confirm-target-drained", "", "must exactly equal target-id after the operator completes target-wide drain")
	stateFile := flag.String("state-file", "", "durable local cleanup task database")
	apply := flag.Bool("apply", false, "execute the persisted plan; without this flag only plan")
	extendDataPlan := flag.Bool("extend-data-plan", false, "after marker deletion, persist newly visible data objects for review without deleting them")
	flag.Parse()
	if *endpoint == "" || *bucketName == "" || *familyPrefix == "" || *container == "" || *targetID == "" || *stateFile == "" {
		fatal(errors.New("endpoint, bucket, family-prefix, container, target-id, and state-file are required"))
	}
	normalizedPrefix, err := normalizeFamilyPrefix(*familyPrefix)
	if err != nil {
		fatal(err)
	}
	if err := validateContainer(*container); err != nil {
		fatal(err)
	}
	canonicalEndpoint, err := identity.CanonicalOSSEndpoint(*endpoint)
	if err != nil {
		fatal(err)
	}
	if *apply && *extendDataPlan {
		fatal(errors.New("apply and extend-data-plan are separate steps and cannot be used together"))
	}
	if (*apply || *extendDataPlan) && *confirmDrain != *targetID {
		fatal(errors.New("apply and extend-data-plan require --confirm-target-drained to exactly match --target-id"))
	}
	accessKeyID := os.Getenv("OSS_ACCESS_KEY_ID")
	accessKeySecret := os.Getenv("OSS_ACCESS_KEY_SECRET")
	if accessKeyID == "" || accessKeySecret == "" {
		fatal(errors.New("OSS credentials are required in the environment"))
	}
	opts := []aliyunoss.ClientOption{aliyunoss.Timeout(10, 30)}
	if token := os.Getenv("OSS_SESSION_TOKEN"); token != "" {
		opts = append(opts, aliyunoss.SecurityToken(token))
	}
	client, err := aliyunoss.New(canonicalEndpoint, accessKeyID, accessKeySecret, opts...)
	if err != nil {
		fatal(err)
	}
	versioning, err := client.GetBucketVersioning(*bucketName)
	if err != nil {
		fatal(fmt.Errorf("read OSS bucket versioning: %w", err))
	}
	if versioning.Status != "" {
		fatal(fmt.Errorf("OSS bucket versioning must be disabled, got %q", versioning.Status))
	}
	bucket, err := client.Bucket(*bucketName)
	if err != nil {
		fatal(err)
	}
	db, err := bolt.Open(*stateFile, 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		fatal(err)
	}
	defer db.Close()

	key := []byte(taskKey(canonicalEndpoint, *bucketName, *targetID, normalizedPrefix, *container))
	plan, err := loadOrRefreshManifest(db, key, *apply, canonicalEndpoint, *bucketName, *targetID, normalizedPrefix, *container, func() (manifest, error) {
		return buildManifest(bucket, canonicalEndpoint, *bucketName, *targetID, normalizedPrefix, *container)
	})
	if err != nil {
		fatal(err)
	}
	if *extendDataPlan {
		if plan.Phase != "markers-deleted" && plan.Phase != "objects-deleted" {
			fatal(fmt.Errorf("cleanup phase %q cannot extend its data plan", plan.Phase))
		}
		if err := reconcilePostMarkerData(bucket, db, key, &plan, true); err != nil {
			fatal(err)
		}
		printManifest(os.Stdout, plan)
		fmt.Println("cleanup data plan persisted; review it, then rerun with --apply without --extend-data-plan")
		return
	}
	printManifest(os.Stdout, plan)
	if !*apply {
		return
	}
	if plan.Phase == "planned" {
		fresh, err := buildManifest(bucket, canonicalEndpoint, *bucketName, *targetID, normalizedPrefix, *container)
		if err != nil {
			fatal(err)
		}
		if fresh.MarkerDigest != plan.MarkerDigest || !sameKeys(fresh.MarkerKeys, plan.MarkerKeys) || !sameKeys(fresh.DataKeys, plan.DataKeys) {
			fatal(errors.New("object family changed since the cleanup plan was created; rerun without --apply to refresh the plan"))
		}
	}
	if err := execute(bucket, db, key, &plan); err != nil {
		fatal(err)
	}
	fmt.Println("cleanup complete")
}

func loadOrRefreshManifest(db *bolt.DB, key []byte, apply bool, endpoint, bucketName, targetID, familyPrefix, container string, build func() (manifest, error)) (manifest, error) {
	plan, found, err := readManifest(db, key)
	if err != nil {
		return manifest{}, err
	}
	if found {
		if err := validateManifest(plan, endpoint, bucketName, targetID, familyPrefix, container); err != nil {
			return manifest{}, err
		}
	}
	if !found || (!apply && plan.Phase == "planned") {
		plan, err = build()
		if err != nil {
			return manifest{}, err
		}
		if err := validateManifest(plan, endpoint, bucketName, targetID, familyPrefix, container); err != nil {
			return manifest{}, err
		}
		if err := writeManifest(db, key, plan); err != nil {
			return manifest{}, err
		}
	}
	return plan, nil
}

func buildManifest(bucket *aliyunoss.Bucket, endpoint, bucketName, targetID, familyPrefix, container string) (manifest, error) {
	markerPattern := markerKeyPattern(familyPrefix, container)
	markerKeys, err := listMatchingKeys(bucket, objectlayout.MarkerPrefix(familyPrefix, container), markerPattern)
	if err != nil {
		return manifest{}, err
	}
	if len(markerKeys) == 0 {
		return manifest{}, errors.New("no finalization markers found")
	}
	revisions := make(map[string]uint64, len(markerKeys))
	for _, key := range markerKeys {
		revision, err := markerRevision(markerPattern, key)
		if err != nil {
			return manifest{}, err
		}
		revisions[key] = revision
	}
	sort.Slice(markerKeys, func(i, j int) bool { return revisions[markerKeys[i]] < revisions[markerKeys[j]] })
	var latest marker.Marker
	var previous marker.Marker
	h := sha256.New()
	for index, key := range markerKeys {
		if revisions[key] != uint64(index+1) {
			return manifest{}, errors.New("marker revisions are not continuous")
		}
		reader, err := bucket.GetObject(key)
		if err != nil {
			return manifest{}, err
		}
		raw, readErr := io.ReadAll(reader)
		_ = reader.Close()
		if readErr != nil {
			return manifest{}, readErr
		}
		value, err := marker.Decode(raw)
		if err != nil {
			return manifest{}, fmt.Errorf("validate %s: %w", key, err)
		}
		if err := validateMarkerIdentity(value, key, targetID, familyPrefix, container, uint64(index+1)); err != nil {
			return manifest{}, err
		}
		if index > 0 {
			if err := validateCumulative(previous, value); err != nil {
				return manifest{}, fmt.Errorf("validate cumulative marker %s: %w", key, err)
			}
		}
		previous = value
		latest = value
		_, _ = h.Write(raw)
	}
	knownData := make(map[string]struct{}, len(latest.Objects))
	for _, object := range latest.Objects {
		if object.Key != objectlayout.DataKey(familyPrefix, container, object.Generation) {
			return manifest{}, fmt.Errorf("marker object %q is outside the requested object family", object.Key)
		}
		header, err := bucket.GetObjectDetailedMeta(object.Key)
		if err != nil {
			return manifest{}, err
		}
		size, err := strconv.ParseInt(header.Get("Content-Length"), 10, 64)
		if err != nil || size != object.Size || header.Get(aliyunoss.HTTPHeaderOssCRC64) != object.CRC64 {
			return manifest{}, fmt.Errorf("object %s no longer matches marker", object.Key)
		}
		knownData[object.Key] = struct{}{}
	}
	dataKeys, err := listMatchingKeys(bucket, dataPrefix(familyPrefix, container), dataKeyPattern(familyPrefix, container))
	if err != nil {
		return manifest{}, err
	}
	for key := range knownData {
		if !containsKey(dataKeys, key) {
			return manifest{}, fmt.Errorf("finalized object %s is missing from object family listing", key)
		}
	}
	sort.Strings(dataKeys)
	var unmarkedDataKeys []string
	for _, key := range dataKeys {
		if _, found := knownData[key]; !found {
			unmarkedDataKeys = append(unmarkedDataKeys, key)
		}
	}
	return manifest{Endpoint: endpoint, Bucket: bucketName, TargetID: targetID, FamilyPrefix: familyPrefix, Container: container, MarkerKeys: markerKeys, DataKeys: dataKeys, UnmarkedDataKeys: unmarkedDataKeys, MarkerDigest: hex.EncodeToString(h.Sum(nil)), Phase: "planned"}, nil
}

func validateMarkerIdentity(value marker.Marker, key, targetID, familyPrefix, container string, expectedRevision uint64) error {
	expectedKey := objectlayout.MarkerKey(familyPrefix, container, expectedRevision)
	if path.Dir(key) != familyPrefix || key != expectedKey {
		return errors.New("marker key is outside the requested object family")
	}
	segments := strings.Split(familyPrefix, "/")
	if len(segments) < 4 {
		return errors.New("object family prefix does not contain cluster, namespace, sandbox, and pod UID")
	}
	cluster := segments[len(segments)-4]
	namespace := segments[len(segments)-3]
	sandboxID := segments[len(segments)-2]
	podUID := segments[len(segments)-1]
	expectedStreamRef := objectlayout.StreamRef(podUID, container)
	resource := value.Resource
	if value.TargetID != targetID || value.Revision != expectedRevision ||
		resource.ClusterName != cluster || resource.Namespace != namespace || resource.SandboxID != sandboxID ||
		resource.PodUID != podUID || resource.Container != container ||
		value.StreamRef != expectedStreamRef ||
		value.FinalizeID != identity.FinalizeID(expectedStreamRef, expectedRevision, targetID) {
		return errors.New("marker identity does not match cleanup target")
	}
	return nil
}

func listMatchingKeys(bucket *aliyunoss.Bucket, prefix string, pattern *regexp.Regexp) ([]string, error) {
	var keys []string
	err := visitMatchingKeys(bucket, prefix, pattern, func(key string) bool {
		keys = append(keys, key)
		return true
	})
	return keys, err
}

func visitMatchingKeys(bucket *aliyunoss.Bucket, prefix string, pattern *regexp.Regexp, visit func(string) bool) error {
	cursor := ""
	for {
		result, err := bucket.ListObjects(aliyunoss.Prefix(prefix), aliyunoss.Marker(cursor), aliyunoss.MaxKeys(1000))
		if err != nil {
			return err
		}
		for _, object := range result.Objects {
			if pattern.MatchString(object.Key) && !visit(object.Key) {
				return nil
			}
		}
		if !result.IsTruncated {
			return nil
		}
		next, err := nextListMarker(cursor, result.NextMarker, result.Objects)
		if err != nil {
			return err
		}
		cursor = next
	}
}

func nextListMarker(current, serviceNext string, objects []aliyunoss.ObjectProperties) (string, error) {
	next := serviceNext
	if next == "" && len(objects) > 0 {
		next = objects[len(objects)-1].Key
	}
	if next == "" || next <= current {
		return "", errors.New("OSS listing made no progress")
	}
	return next, nil
}

func markerRevision(pattern *regexp.Regexp, key string) (uint64, error) {
	match := pattern.FindStringSubmatch(key)
	if len(match) != 2 {
		return 0, fmt.Errorf("marker key %q is not canonical", key)
	}
	revision, err := strconv.ParseUint(match[1], 10, 64)
	if err != nil || revision == 0 {
		return 0, fmt.Errorf("marker key %q has an invalid revision", key)
	}
	return revision, nil
}

func containsKey(keys []string, key string) bool {
	for _, candidate := range keys {
		if candidate == key {
			return true
		}
	}
	return false
}

func validateCumulative(previous, current marker.Marker) error {
	if previous.TargetID != current.TargetID || previous.StreamRef != current.StreamRef || previous.Resource != current.Resource || previous.CoverageStartedAt != current.CoverageStartedAt {
		return errors.New("marker identity changed between revisions")
	}
	if previous.HadDrops && !current.HadDrops {
		return errors.New("cumulative drop flag regressed")
	}
	if len(current.Objects) < len(previous.Objects) {
		return errors.New("cumulative object list shrank")
	}
	for index, object := range previous.Objects {
		if current.Objects[index] != object {
			return errors.New("previously finalized object changed")
		}
	}
	return nil
}

func sameKeys(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func printManifest(output io.Writer, plan manifest) {
	fmt.Fprintf(output, "cleanup plan phase=%s markers=%d objects=%d digest=%s\n", plan.Phase, len(plan.MarkerKeys), len(plan.DataKeys), plan.MarkerDigest)
	for _, key := range plan.MarkerKeys {
		fmt.Fprintf(output, "marker key=%q\n", key)
	}
	unmarked := make(map[string]struct{}, len(plan.UnmarkedDataKeys))
	for _, key := range plan.UnmarkedDataKeys {
		unmarked[key] = struct{}{}
	}
	for _, key := range plan.DataKeys {
		coverage := "covered"
		if _, found := unmarked[key]; found {
			coverage = "not-covered"
		}
		fmt.Fprintf(output, "data key=%q latest-marker=%s\n", key, coverage)
	}
}

func execute(bucket *aliyunoss.Bucket, db *bolt.DB, key []byte, plan *manifest) error {
	if plan.Phase == "planned" {
		plan.Phase = "deleting-markers"
		if err := writeManifest(db, key, *plan); err != nil {
			return err
		}
	}
	if plan.Phase == "deleting-markers" {
		for _, objectKey := range reversedKeys(plan.MarkerKeys) {
			if err := bucket.DeleteObject(objectKey); err != nil {
				return err
			}
		}
		for _, objectKey := range plan.MarkerKeys {
			if err := assertMissing(bucket, objectKey); err != nil {
				return err
			}
		}
		plan.Phase = "markers-deleted"
		if err := writeManifest(db, key, *plan); err != nil {
			return err
		}
	}
	if plan.Phase == "markers-deleted" || plan.Phase == "objects-deleted" {
		if err := reconcilePostMarkerData(bucket, db, key, plan, false); err != nil {
			return err
		}
	}
	if plan.Phase == "markers-deleted" {
		// Recheck after reconciliation persisted any newly visible data. No data
		// deletion may begin while a canonical marker is visible.
		if err := assertNoMatchingObjects(bucket, objectlayout.MarkerPrefix(plan.FamilyPrefix, plan.Container), markerKeyPattern(plan.FamilyPrefix, plan.Container), "finalization marker"); err != nil {
			return err
		}
		for _, objectKey := range plan.DataKeys {
			if err := bucket.DeleteObject(objectKey); err != nil {
				return err
			}
		}
		for _, objectKey := range plan.DataKeys {
			if err := assertMissing(bucket, objectKey); err != nil {
				return err
			}
		}
		if err := assertNoMatchingObjects(bucket, dataPrefix(plan.FamilyPrefix, plan.Container), dataKeyPattern(plan.FamilyPrefix, plan.Container), "data object"); err != nil {
			return err
		}
		plan.Phase = "objects-deleted"
		if err := writeManifest(db, key, *plan); err != nil {
			return err
		}
	}
	if plan.Phase == "objects-deleted" {
		if err := assertNoMatchingObjects(bucket, objectlayout.MarkerPrefix(plan.FamilyPrefix, plan.Container), markerKeyPattern(plan.FamilyPrefix, plan.Container), "finalization marker"); err != nil {
			return err
		}
		if err := assertNoMatchingObjects(bucket, dataPrefix(plan.FamilyPrefix, plan.Container), dataKeyPattern(plan.FamilyPrefix, plan.Container), "data object"); err != nil {
			return err
		}
		plan.Phase = "complete"
		if err := writeManifest(db, key, *plan); err != nil {
			return err
		}
	}
	if plan.Phase != "complete" {
		return fmt.Errorf("unknown cleanup phase %q", plan.Phase)
	}
	return errors.Join(
		assertNoMatchingObjects(bucket, objectlayout.MarkerPrefix(plan.FamilyPrefix, plan.Container), markerKeyPattern(plan.FamilyPrefix, plan.Container), "finalization marker"),
		assertNoMatchingObjects(bucket, dataPrefix(plan.FamilyPrefix, plan.Container), dataKeyPattern(plan.FamilyPrefix, plan.Container), "data object"),
	)
}

func reconcilePostMarkerData(bucket *aliyunoss.Bucket, db *bolt.DB, key []byte, plan *manifest, extendDataPlan bool) error {
	// Marker absence is the prerequisite for resuming any data deletion.
	if err := assertNoMatchingObjects(bucket, objectlayout.MarkerPrefix(plan.FamilyPrefix, plan.Container), markerKeyPattern(plan.FamilyPrefix, plan.Container), "finalization marker"); err != nil {
		return err
	}
	remaining, err := listMatchingKeys(bucket, dataPrefix(plan.FamilyPrefix, plan.Container), dataKeyPattern(plan.FamilyPrefix, plan.Container))
	if err != nil {
		return err
	}
	changed, err := mergeRemainingDataKeys(plan, remaining, extendDataPlan)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	return writeManifest(db, key, *plan)
}

func mergeRemainingDataKeys(plan *manifest, remaining []string, extendDataPlan bool) (bool, error) {
	if plan.Phase != "markers-deleted" && plan.Phase != "objects-deleted" {
		return false, fmt.Errorf("cleanup phase %q cannot reconcile data objects", plan.Phase)
	}
	if len(remaining) == 0 {
		return false, nil
	}
	known := make(map[string]struct{}, len(plan.DataKeys))
	for _, objectKey := range plan.DataKeys {
		known[objectKey] = struct{}{}
	}
	var unexpected []string
	var reappeared []string
	for _, objectKey := range remaining {
		if _, found := known[objectKey]; found {
			if plan.Phase == "objects-deleted" {
				reappeared = append(reappeared, objectKey)
			}
			continue
		}
		known[objectKey] = struct{}{}
		unexpected = append(unexpected, objectKey)
	}
	if len(reappeared) != 0 && !extendDataPlan {
		return false, fmt.Errorf("data object %q reappeared after deletion; verify the target remains drained, then persist and review it with --extend-data-plan before applying again", reappeared[0])
	}
	if len(unexpected) != 0 && !extendDataPlan {
		return false, fmt.Errorf("unplanned data object %q appeared after marker deletion; verify the target remains drained, then persist and review it with --extend-data-plan before applying again", unexpected[0])
	}
	changed := false
	for _, objectKey := range unexpected {
		plan.DataKeys = append(plan.DataKeys, objectKey)
		plan.UnmarkedDataKeys = append(plan.UnmarkedDataKeys, objectKey)
		changed = true
	}
	if plan.Phase != "markers-deleted" {
		plan.Phase = "markers-deleted"
		changed = true
	}
	if !changed {
		return false, nil
	}
	sort.Strings(plan.DataKeys)
	sort.Strings(plan.UnmarkedDataKeys)
	return true, nil
}

func assertMissing(bucket *aliyunoss.Bucket, key string) error {
	exists, err := bucket.IsObjectExist(key)
	if err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("object %s still exists", key)
	}
	return nil
}

func assertNoMatchingObjects(bucket *aliyunoss.Bucket, prefix string, pattern *regexp.Regexp, kind string) error {
	found := ""
	err := visitMatchingKeys(bucket, prefix, pattern, func(key string) bool {
		found = key
		return false
	})
	if err != nil {
		return err
	}
	if found != "" {
		return fmt.Errorf("%s %s still exists", kind, found)
	}
	return nil
}

func readManifest(db *bolt.DB, key []byte) (manifest, bool, error) {
	var value manifest
	found := false
	err := db.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte("cleanup"))
		if bucket == nil {
			return nil
		}
		raw := bucket.Get(key)
		if raw == nil {
			return nil
		}
		found = true
		return json.Unmarshal(raw, &value)
	})
	return value, found, err
}

func writeManifest(db *bolt.DB, key []byte, value manifest) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return db.Update(func(tx *bolt.Tx) error {
		bucket, err := tx.CreateBucketIfNotExists([]byte("cleanup"))
		if err != nil {
			return err
		}
		return bucket.Put(key, raw)
	})
}

func taskKey(endpoint, bucket, targetID, familyPrefix, container string) string {
	digest := sha256.Sum256([]byte(strings.Join([]string{endpoint, bucket, targetID, familyPrefix, container}, "\x00")))
	return hex.EncodeToString(digest[:])
}

func validateManifest(value manifest, endpoint, bucket, targetID, familyPrefix, container string) error {
	if value.Endpoint != endpoint || value.Bucket != bucket || value.TargetID != targetID || value.FamilyPrefix != familyPrefix || value.Container != container {
		return errors.New("persisted cleanup manifest does not match the requested OSS object family")
	}
	switch value.Phase {
	case "planned", "deleting-markers", "markers-deleted", "objects-deleted", "complete":
	default:
		return fmt.Errorf("persisted cleanup manifest has unknown phase %q", value.Phase)
	}
	if len(value.MarkerKeys) == 0 {
		return errors.New("persisted cleanup manifest has no finalization markers")
	}
	for index, key := range value.MarkerKeys {
		expected := objectlayout.MarkerKey(familyPrefix, container, uint64(index+1))
		if key != expected {
			return fmt.Errorf("persisted cleanup marker key %q is not canonical", key)
		}
	}
	dataPattern := dataKeyPattern(familyPrefix, container)
	dataKeys := make(map[string]struct{}, len(value.DataKeys))
	for index, key := range value.DataKeys {
		if !dataPattern.MatchString(key) || index > 0 && key <= value.DataKeys[index-1] {
			return fmt.Errorf("persisted cleanup data key %q is not canonical", key)
		}
		dataKeys[key] = struct{}{}
	}
	for index, key := range value.UnmarkedDataKeys {
		if !dataPattern.MatchString(key) || index > 0 && key <= value.UnmarkedDataKeys[index-1] {
			return fmt.Errorf("persisted cleanup unmarked data key %q is not canonical", key)
		}
		if _, found := dataKeys[key]; !found {
			return fmt.Errorf("persisted cleanup unmarked data key %q is absent from data keys", key)
		}
	}
	digest, err := hex.DecodeString(value.MarkerDigest)
	if err != nil || len(digest) != sha256.Size {
		return errors.New("persisted cleanup manifest has an invalid marker digest")
	}
	return nil
}

func markerKeyPattern(familyPrefix, container string) *regexp.Regexp {
	return regexp.MustCompile(`^` + regexp.QuoteMeta(objectlayout.MarkerPrefix(familyPrefix, container)) + `([1-9][0-9]*)\.json$`)
}

func dataKeyPattern(familyPrefix, container string) *regexp.Regexp {
	return regexp.MustCompile(`^` + regexp.QuoteMeta(dataPrefix(familyPrefix, container)) + `(?:\.[0-9]+)?\.log$`)
}

func dataPrefix(familyPrefix, container string) string {
	return path.Join(familyPrefix, container)
}

func normalizeFamilyPrefix(value string) (string, error) {
	normalized := strings.Trim(value, "/")
	if normalized == "" {
		return "", errors.New("family-prefix must contain at least one non-slash path segment")
	}
	if path.Clean(normalized) != normalized {
		return "", errors.New("family-prefix must be canonical and contain no empty, dot, or parent segments")
	}
	for _, segment := range strings.Split(normalized, "/") {
		if segment == "." || segment == ".." {
			return "", errors.New("family-prefix must be canonical and contain no empty, dot, or parent segments")
		}
	}
	return normalized, nil
}

func validateContainer(value string) error {
	if value == "" || value == "." || value == ".." || strings.ContainsAny(value, `/\`) {
		return errors.New("container must be one canonical path segment")
	}
	return nil
}

func reversedKeys(keys []string) []string {
	reversed := make([]string, len(keys))
	for index := range keys {
		reversed[len(keys)-1-index] = keys[index]
	}
	return reversed
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "nodeagent-oss-cleanup:", err)
	os.Exit(1)
}
