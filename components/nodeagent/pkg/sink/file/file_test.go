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

//go:build linux

package file

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/nodeagent/pkg/api"
	"github.com/alibaba/opensandbox/nodeagent/pkg/marker"
	"github.com/alibaba/opensandbox/nodeagent/pkg/state"
)

type failFinalCheckpointStore struct {
	*state.DB
	fail bool
}

func (s *failFinalCheckpointStore) PutSinkStream(sinkName string, stream state.SinkStream) error {
	if s.fail && stream.Position > 0 && stream.AppendIntent == nil {
		s.fail = false
		return errors.New("injected final checkpoint failure")
	}
	return s.DB.PutSinkStream(sinkName, stream)
}

type failGenerationTransitionStore struct {
	*state.DB
	fail bool
}

func (s *failGenerationTransitionStore) PutSinkStream(sinkName string, stream state.SinkStream) error {
	if s.fail && stream.GenerationTransition != nil {
		s.fail = false
		return errors.New("injected generation transition failure")
	}
	return s.DB.PutSinkStream(sinkName, stream)
}

func TestDurableFileConsumeAndFinalize(t *testing.T) {
	root := t.TempDir()
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sink, err := New(Config{Root: root, ClusterID: "prod-a", MaxFileBytes: 1 << 20, MaxFiles: 4, MaxTotalBytes: 1 << 24}, db)
	if err != nil {
		t.Fatal(err)
	}
	resource := api.Resource{SandboxID: "sb-abc", ClusterName: "prod-a", Namespace: "team-a", PodName: "pod", PodUID: "u123", NodeName: "node-1", Container: "sandbox"}
	streamRef := api.StreamRef{ID: "container-logs/u123/sandbox"}
	batch := api.Batch{StreamRef: streamRef, Items: []api.BatchItem{{RecordID: "r1", Record: api.Record{Kind: api.RecordKindContainerLog, Timestamp: time.Date(2026, 7, 23, 10, 0, 0, 0, time.UTC), Body: []byte("hello"), Resource: resource, Attributes: map[string]string{"stream": "stdout"}}}}}
	if err := sink.Consume(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(root, "prod-a", "team-a", "sb-abc", "u123", "sandbox.log")
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(raw); got != "2026-07-23T10:00:00Z stdout hello\n" {
		t.Fatalf("log=%q", got)
	}
	request := api.FinalizeRequest{FinalizeID: "sha256:final", TargetID: "target", StreamRef: streamRef, Revision: 1, CoverageStartedAt: time.Date(2026, 7, 23, 9, 58, 0, 0, time.UTC), Resource: resource, FinalizedAt: time.Date(2026, 7, 23, 10, 5, 0, 0, time.UTC)}
	if err := sink.Finalize(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(filepath.Dir(logPath), "sandbox.finalized.1.json")
	markerRaw, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	value, err := marker.Decode(markerRaw)
	if err != nil {
		t.Fatal(err)
	}
	if value.Status != "complete" || len(value.Objects) != 1 || value.Objects[0].Size != int64(len(raw)) {
		t.Fatalf("marker=%+v", value)
	}

	batch.Items[0].Record.Body = []byte("late")
	if err := sink.Consume(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	latePath := filepath.Join(filepath.Dir(logPath), "sandbox.1.log")
	late, err := os.ReadFile(latePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(late), " late\n") {
		t.Fatalf("late generation=%q", late)
	}
	used, err := measureCapacity(root)
	if err != nil {
		t.Fatal(err)
	}
	if !sink.capacityKnown || sink.capacityUsed != used {
		t.Fatalf("capacity known=%v used=%d measured=%d", sink.capacityKnown, sink.capacityUsed, used)
	}
}

func TestDurableFileFinalizeHonorsCanceledContext(t *testing.T) {
	root := t.TempDir()
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sink, err := New(Config{Root: root, ClusterID: "prod-a", MaxFileBytes: 1 << 20, MaxFiles: 4, MaxTotalBytes: 1 << 24}, db)
	if err != nil {
		t.Fatal(err)
	}
	resource := api.Resource{SandboxID: "sb-abc", ClusterName: "prod-a", Namespace: "team-a", PodName: "pod", PodUID: "u123", NodeName: "node-1", Container: "sandbox"}
	streamRef := api.StreamRef{ID: "container-logs/u123/sandbox"}
	batch := api.Batch{StreamRef: streamRef, Items: []api.BatchItem{{RecordID: "r1", Record: api.Record{Kind: api.RecordKindContainerLog, Timestamp: time.Now().UTC(), Body: []byte("hello"), Resource: resource, Attributes: map[string]string{"stream": "stdout"}}}}}
	if err := sink.Consume(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := api.FinalizeRequest{FinalizeID: "final", TargetID: "target", StreamRef: streamRef, Revision: 1, CoverageStartedAt: time.Now().UTC().Add(-time.Minute).Truncate(time.Second), Resource: resource, FinalizedAt: time.Now().UTC()}
	if err := sink.Finalize(ctx, request); !errors.Is(err, context.Canceled) {
		t.Fatalf("Finalize() error=%v, want context canceled", err)
	}
	markerPath := filepath.Join(root, "prod-a", "team-a", "sb-abc", "u123", "sandbox.finalized.1.json")
	if _, err := os.Stat(markerPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("marker exists after canceled finalization: %v", err)
	}
}

func TestDurableFilePermanentCapacityErrorsAreNonRetryable(t *testing.T) {
	for _, test := range []struct {
		name          string
		maxFileBytes  int64
		maxTotalBytes int64
	}{
		{name: "batch-limit", maxFileBytes: 1, maxTotalBytes: 1 << 20},
		{name: "total-limit", maxFileBytes: 1 << 20, maxTotalBytes: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, err := state.Open(t.TempDir(), "target", 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			sink, err := New(Config{Root: t.TempDir(), ClusterID: "cluster", MaxFileBytes: test.maxFileBytes, MaxFiles: 2, MaxTotalBytes: test.maxTotalBytes}, db)
			if err != nil {
				t.Fatal(err)
			}
			resource := api.Resource{SandboxID: "sb", ClusterName: "cluster", Namespace: "ns", PodUID: "uid", Container: "sandbox"}
			batch := api.Batch{StreamRef: api.StreamRef{ID: "stream"}, Items: []api.BatchItem{{RecordID: "record", Record: api.Record{Kind: api.RecordKindContainerLog, Timestamp: time.Now().UTC(), Body: []byte("data"), Resource: resource, Attributes: map[string]string{"stream": "stdout"}}}}}
			err = sink.Consume(context.Background(), batch)
			if err == nil || api.IsRetryableError(err) {
				t.Fatalf("error=%v retryable=%v", err, api.IsRetryableError(err))
			}
		})
	}
}

func TestDurableFileCapacityExhaustionIsRetryable(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "filler"), make([]byte, 1024), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	resource := api.Resource{SandboxID: "sb", ClusterName: "cluster", Namespace: "ns", PodUID: "uid", Container: "sandbox"}
	batch := api.Batch{StreamRef: api.StreamRef{ID: "stream"}, Items: []api.BatchItem{{RecordID: "record", Record: api.Record{Kind: api.RecordKindContainerLog, Timestamp: time.Now().UTC(), Body: []byte("data"), Resource: resource, Attributes: map[string]string{"stream": "stdout"}}}}}
	encoded := lineBytes(batch)
	sink, err := New(Config{Root: root, ClusterID: "cluster", MaxFileBytes: 1 << 20, MaxFiles: 2, MaxTotalBytes: 1024 + int64(len(encoded)) - 1}, db)
	if err != nil {
		t.Fatal(err)
	}
	err = sink.Consume(context.Background(), batch)
	if err == nil || !api.IsRetryableError(err) {
		t.Fatalf("error=%v retryable=%v", err, api.IsRetryableError(err))
	}
}

func TestDurableFileGenerationLimitPrecedesRetryableCapacityError(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "filler"), make([]byte, 1024), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	resource := api.Resource{SandboxID: "sb", ClusterName: "cluster", Namespace: "ns", PodUID: "uid", Container: "sandbox"}
	sink, err := New(Config{Root: root, ClusterID: "cluster", MaxFileBytes: 1 << 20, MaxFiles: 1, MaxTotalBytes: 1024}, db)
	if err != nil {
		t.Fatal(err)
	}
	sink.writers["stream"] = &writer{stream: state.SinkStream{StreamRef: "stream", CurrentClosed: true}, resource: resource}
	batch := api.Batch{StreamRef: api.StreamRef{ID: "stream"}, Items: []api.BatchItem{{RecordID: "record", Record: api.Record{Kind: api.RecordKindContainerLog, Timestamp: time.Now().UTC(), Body: []byte("data"), Resource: resource, Attributes: map[string]string{"stream": "stdout"}}}}}
	err = sink.Consume(context.Background(), batch)
	if err == nil || api.IsRetryableError(err) || !strings.Contains(err.Error(), "generation limit") {
		t.Fatalf("error=%v retryable=%v", err, api.IsRetryableError(err))
	}
}

func TestDurableFileRejectsInconsistentBatchResources(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sink, err := New(Config{Root: t.TempDir(), ClusterID: "cluster", MaxFileBytes: 1 << 20, MaxFiles: 2, MaxTotalBytes: 1 << 20}, db)
	if err != nil {
		t.Fatal(err)
	}
	resource := api.Resource{SandboxID: "sb", ClusterName: "cluster", Namespace: "ns", PodUID: "uid", Container: "sandbox"}
	batch := api.Batch{StreamRef: api.StreamRef{ID: "stream"}, Items: []api.BatchItem{
		{RecordID: "first", Record: api.Record{Resource: resource}},
		{RecordID: "second", Record: api.Record{Resource: resource}},
	}}
	batch.Items[1].Record.Resource.PodUID = "other-pod"
	err = sink.Consume(context.Background(), batch)
	if err == nil || api.IsRetryableError(err) {
		t.Fatalf("error=%v retryable=%v", err, api.IsRetryableError(err))
	}
}

func TestDurableFileRejectsPersistedObjectKeyOutsideStreamLayout(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	root := t.TempDir()
	sink, err := New(Config{Root: root, ClusterID: "cluster", MaxFileBytes: 1 << 20, MaxFiles: 2, MaxTotalBytes: 1 << 20}, db)
	if err != nil {
		t.Fatal(err)
	}
	resource := api.Resource{SandboxID: "sb", ClusterName: "cluster", Namespace: "ns", PodUID: "uid", Container: "sandbox"}
	streamRef := api.StreamRef{ID: "stream"}
	if err := db.PutSinkStream(name, state.SinkStream{StreamRef: streamRef.ID, ObjectKey: "other/family.log"}); err != nil {
		t.Fatal(err)
	}
	batch := api.Batch{StreamRef: streamRef, Items: []api.BatchItem{{RecordID: "record", Record: api.Record{Resource: resource}}}}
	err = sink.Consume(context.Background(), batch)
	if err == nil || api.IsRetryableError(err) || !strings.Contains(err.Error(), "object key") {
		t.Fatalf("error=%v retryable=%v", err, api.IsRetryableError(err))
	}
}

func TestDurableFileRejectsClosedObjectCountMismatchBeforeAppend(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	root := t.TempDir()
	sink, err := New(Config{Root: root, ClusterID: "cluster", MaxFileBytes: 1 << 20, MaxFiles: 2, MaxTotalBytes: 1 << 20}, db)
	if err != nil {
		t.Fatal(err)
	}
	resource := api.Resource{SandboxID: "sb", ClusterName: "cluster", Namespace: "ns", PodUID: "uid", Container: "sandbox"}
	streamRef := api.StreamRef{ID: "stream"}
	stream := state.SinkStream{StreamRef: streamRef.ID, ObjectKey: "cluster/ns/sb/uid/sandbox.log", CurrentClosed: true}
	if err := db.PutSinkStream(name, stream); err != nil {
		t.Fatal(err)
	}
	batch := api.Batch{StreamRef: streamRef, Items: []api.BatchItem{{RecordID: "record", Record: api.Record{Resource: resource}}}}
	err = sink.Consume(context.Background(), batch)
	if err == nil || api.IsRetryableError(err) || !strings.Contains(err.Error(), "closed objects") {
		t.Fatalf("layout error=%v retryable=%v", err, api.IsRetryableError(err))
	}
	if _, err := os.Stat(filepath.Join(root, "cluster", "ns", "sb", "uid", "sandbox.1.log")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid checkpoint created a new generation: %v", err)
	}
}

func TestDurableFileRejectsResourceChangeAcrossBatches(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sink, err := New(Config{Root: t.TempDir(), ClusterID: "cluster", MaxFileBytes: 1 << 20, MaxFiles: 2, MaxTotalBytes: 1 << 20}, db)
	if err != nil {
		t.Fatal(err)
	}
	resource := api.Resource{SandboxID: "sb", ClusterName: "cluster", Namespace: "ns", PodName: "pod", PodUID: "uid", NodeName: "node", Container: "sandbox", LogDirectory: "/logs"}
	batch := api.Batch{StreamRef: api.StreamRef{ID: "stream"}, Items: []api.BatchItem{{RecordID: "record", Record: api.Record{Timestamp: time.Now().UTC(), Resource: resource}}}}
	if err := sink.Consume(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	batch.Items[0].Record.Resource.PodName = "different-pod"
	err = sink.Consume(context.Background(), batch)
	if err == nil || api.IsRetryableError(err) || !strings.Contains(err.Error(), "resource identity changed") {
		t.Fatalf("error=%v retryable=%v", err, api.IsRetryableError(err))
	}
}

func TestDurableFileRejectsResourceChangeAtFinalize(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	root := t.TempDir()
	sink, err := New(Config{Root: root, ClusterID: "cluster", MaxFileBytes: 1 << 20, MaxFiles: 2, MaxTotalBytes: 1 << 20}, db)
	if err != nil {
		t.Fatal(err)
	}
	resource := api.Resource{SandboxID: "sb", ClusterName: "cluster", Namespace: "ns", PodName: "pod", PodUID: "uid", NodeName: "node", Container: "sandbox", LogDirectory: "/logs"}
	streamRef := api.StreamRef{ID: "stream"}
	batch := api.Batch{StreamRef: streamRef, Items: []api.BatchItem{{RecordID: "record", Record: api.Record{Timestamp: time.Now().UTC(), Resource: resource}}}}
	if err := sink.Consume(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	resource.PodName = "different-pod"
	request := api.FinalizeRequest{FinalizeID: "final", TargetID: "target", StreamRef: streamRef, Revision: 1, CoverageStartedAt: time.Now().UTC().Add(-time.Minute).Truncate(time.Second), Resource: resource, FinalizedAt: time.Now().UTC()}
	err = sink.Finalize(context.Background(), request)
	if err == nil || api.IsRetryableError(err) || !strings.Contains(err.Error(), "resource identity changed") {
		t.Fatalf("finalize error=%v retryable=%v", err, api.IsRetryableError(err))
	}
	if _, err := os.Stat(filepath.Join(root, "cluster", "ns", "sb", "uid", "sandbox.finalized.1.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("resource mismatch published a marker: %v", err)
	}
}

func TestDurableFileRecoversPartialAppendIntent(t *testing.T) {
	root := t.TempDir()
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cfg := Config{Root: root, ClusterID: "prod-a", MaxFileBytes: 1 << 20, MaxFiles: 4, MaxTotalBytes: 1 << 24}
	sink, err := New(cfg, db)
	if err != nil {
		t.Fatal(err)
	}
	resource := api.Resource{SandboxID: "sb", ClusterName: "prod-a", Namespace: "ns", PodName: "pod", PodUID: "uid", NodeName: "node", Container: "sandbox"}
	streamRef := api.StreamRef{ID: "container-logs/uid/sandbox"}
	batch := api.Batch{StreamRef: streamRef, Items: []api.BatchItem{{RecordID: "r1", Record: api.Record{Kind: api.RecordKindContainerLog, Timestamp: time.Date(2026, 7, 23, 10, 0, 0, 0, time.UTC), Body: []byte("first"), Resource: resource, Attributes: map[string]string{"stream": "stdout"}}}}}
	if err := sink.Consume(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	stream, found, err := db.GetSinkStream(name, streamRef.ID)
	if err != nil || !found {
		t.Fatalf("stream=%+v found=%v err=%v", stream, found, err)
	}
	replay := api.Batch{StreamRef: streamRef, Items: []api.BatchItem{{RecordID: "r2", Record: api.Record{Kind: api.RecordKindContainerLog, Timestamp: time.Date(2026, 7, 23, 10, 0, 1, 0, time.UTC), Body: []byte("second"), Resource: resource, Attributes: map[string]string{"stream": "stdout"}}}}}
	encoded := lineBytes(replay)
	digest := sha256.Sum256(encoded)
	stream.AppendIntent = &state.AppendIntent{
		Position: stream.Position,
		Length:   int64(len(encoded)),
		SHA256:   hex.EncodeToString(digest[:]),
		Device:   stream.Device,
		Inode:    stream.Inode,
	}
	if err := db.PutSinkStream(name, stream); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, stream.ObjectKey)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(encoded[:len(encoded)/2]); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()

	// The committed bytes plus this replay exactly fit the total capacity.
	// Recovery must truncate the uncommitted tail before reserving the replay.
	cfg.MaxTotalBytes = stream.Position + int64(len(encoded))
	recovered, err := New(cfg, db)
	if err != nil {
		t.Fatal(err)
	}
	if err := recovered.Consume(context.Background(), replay); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := append(lineBytes(batch), encoded...)
	if string(raw) != string(want) {
		t.Fatalf("partial append was not truncated before replay: got %q want %q", raw, want)
	}
}

func TestDurableFileFinalizeClosesRestoredGeneration(t *testing.T) {
	root := t.TempDir()
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cfg := Config{Root: root, ClusterID: "prod-a", MaxFileBytes: 1 << 20, MaxFiles: 4, MaxTotalBytes: 1 << 24}
	resource := api.Resource{SandboxID: "sb", ClusterName: "prod-a", Namespace: "ns", PodName: "pod", PodUID: "uid", NodeName: "node", Container: "sandbox"}
	streamRef := api.StreamRef{ID: "container-logs/uid/sandbox"}
	batch := api.Batch{StreamRef: streamRef, Items: []api.BatchItem{{RecordID: "r1", Record: api.Record{Kind: api.RecordKindContainerLog, Timestamp: time.Date(2026, 7, 23, 10, 0, 0, 0, time.UTC), Body: []byte("before restart"), Resource: resource, Attributes: map[string]string{"stream": "stdout"}}}}}
	first, err := New(cfg, db)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Consume(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	recovered, err := New(cfg, db)
	if err != nil {
		t.Fatal(err)
	}
	request := api.FinalizeRequest{FinalizeID: "finalize", TargetID: "target", StreamRef: streamRef, Revision: 1, CoverageStartedAt: time.Date(2026, 7, 23, 9, 58, 0, 0, time.UTC), Resource: resource, FinalizedAt: time.Date(2026, 7, 23, 10, 5, 0, 0, time.UTC)}
	if err := recovered.Finalize(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(root, "prod-a", "ns", "sb", "uid", "sandbox.finalized.1.json")
	raw, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	value, err := marker.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	wantSize := int64(len(lineBytes(batch)))
	if len(value.Objects) != 1 || value.Objects[0].Generation != 0 || value.Objects[0].Size != wantSize {
		t.Fatalf("marker=%+v", value)
	}
	stream, found, err := db.GetSinkStream(name, streamRef.ID)
	if err != nil || !found || !stream.CurrentClosed || len(stream.ClosedObjects) != 1 {
		t.Fatalf("stream=%+v found=%v err=%v", stream, found, err)
	}
	if value.Objects[0].CRC64 == "" || value.Objects[0].CRC64 != stream.ClosedObjects[0].CRC64 {
		t.Fatalf("marker crc=%q stream crc=%q", value.Objects[0].CRC64, stream.ClosedObjects[0].CRC64)
	}
}

func TestDurableFileRetryAfterFinalCheckpointFailureDoesNotDuplicate(t *testing.T) {
	root := t.TempDir()
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := &failFinalCheckpointStore{DB: db, fail: true}
	sink, err := New(Config{Root: root, ClusterID: "prod-a", MaxFileBytes: 1 << 20, MaxFiles: 4, MaxTotalBytes: 1 << 24}, store)
	if err != nil {
		t.Fatal(err)
	}
	resource := api.Resource{SandboxID: "sb", ClusterName: "prod-a", Namespace: "ns", PodName: "pod", PodUID: "uid", NodeName: "node", Container: "sandbox"}
	streamRef := api.StreamRef{ID: "container-logs/uid/sandbox"}
	batch := api.Batch{StreamRef: streamRef, Items: []api.BatchItem{{RecordID: "r1", Record: api.Record{Kind: api.RecordKindContainerLog, Timestamp: time.Date(2026, 7, 23, 10, 0, 0, 0, time.UTC), Body: []byte("once"), Resource: resource, Attributes: map[string]string{"stream": "stdout"}}}}}
	if err := sink.Consume(context.Background(), batch); err == nil || !strings.Contains(err.Error(), "injected final checkpoint failure") {
		t.Fatalf("first consume error=%v", err)
	}
	request := api.FinalizeRequest{FinalizeID: "finalize", TargetID: "target", StreamRef: streamRef, Revision: 1, CoverageStartedAt: time.Now().UTC().Add(-time.Minute).Truncate(time.Second), Resource: resource, FinalizedAt: time.Now().UTC()}
	if err := sink.Finalize(context.Background(), request); err == nil || !strings.Contains(err.Error(), "unresolved append intent") {
		t.Fatalf("finalize with unresolved append error=%v", err)
	}
	if err := sink.Consume(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	if err := sink.Finalize(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "prod-a", "ns", "sb", "uid", "sandbox.log")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := lineBytes(batch); string(raw) != string(want) {
		t.Fatalf("retry duplicated data: got %q want %q", raw, want)
	}
}

func TestDurableFileFinalizeRecoversExistingTemporaryMarkerAtCapacity(t *testing.T) {
	for _, test := range []struct {
		name          string
		persistIntent bool
		markerExists  bool
		partialTemp   bool
	}{{name: "persisted-intent", persistIntent: true}, {name: "persisted-partial-temp", persistIntent: true, partialTemp: true}, {name: "missing-intent"}, {name: "published-marker", persistIntent: true, markerExists: true}} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			db, err := state.Open(t.TempDir(), "target", 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			cfg := Config{Root: root, ClusterID: "prod-a", MaxFileBytes: 1 << 20, MaxFiles: 4, MaxTotalBytes: 1 << 24}
			sink, err := New(cfg, db)
			if err != nil {
				t.Fatal(err)
			}
			resource := api.Resource{SandboxID: "sb", ClusterName: "prod-a", Namespace: "ns", PodName: "pod", PodUID: "uid", NodeName: "node", Container: "sandbox"}
			streamRef := api.StreamRef{ID: "container-logs/uid/sandbox"}
			batch := api.Batch{StreamRef: streamRef, Items: []api.BatchItem{{RecordID: "r1", Record: api.Record{Kind: api.RecordKindContainerLog, Timestamp: time.Date(2026, 7, 23, 10, 0, 0, 0, time.UTC), Body: []byte("data"), Resource: resource, Attributes: map[string]string{"stream": "stdout"}}}}}
			if err := sink.Consume(context.Background(), batch); err != nil {
				t.Fatal(err)
			}
			writer := sink.writers[streamRef.ID]
			if err := sink.closeGeneration(writer); err != nil {
				t.Fatal(err)
			}
			request := api.FinalizeRequest{FinalizeID: "finalize", TargetID: "target", StreamRef: streamRef, Revision: 1, CoverageStartedAt: time.Date(2026, 7, 23, 9, 58, 0, 0, time.UTC), Resource: resource, FinalizedAt: time.Date(2026, 7, 23, 10, 5, 0, 0, time.UTC)}
			raw, err := marker.Encode(marker.New(request, writer.stream.ClosedObjects))
			if err != nil {
				t.Fatal(err)
			}
			dir, err := familyDir(root, resource)
			if err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(raw)
			markerPath := filepath.Join(dir, "sandbox.finalized.1.json")
			tmpName := filepath.Join(dir, ".sandbox.finalized.1."+hex.EncodeToString(digest[:8])+".tmp")
			temporaryBytes := raw
			if test.partialTemp {
				temporaryBytes = raw[:len(raw)/2]
			}
			if err := os.WriteFile(tmpName, temporaryBytes, 0o640); err != nil {
				t.Fatal(err)
			}
			if test.markerExists {
				if err := os.WriteFile(markerPath, raw, 0o640); err != nil {
					t.Fatal(err)
				}
			}
			if test.persistIntent {
				writer.stream.MarkerIntent = &state.MarkerIntent{Revision: 1, Path: markerPath, TempPath: tmpName, SHA256: hex.EncodeToString(digest[:])}
				if err := db.PutSinkStream(name, writer.stream); err != nil {
					t.Fatal(err)
				}
			}
			if err := sink.Close(context.Background()); err != nil {
				t.Fatal(err)
			}

			cfg.MaxTotalBytes = int64(len(lineBytes(batch)) + len(raw))
			recovered, err := New(cfg, db)
			if err != nil {
				t.Fatal(err)
			}
			if err := recovered.Finalize(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			if existing, err := os.ReadFile(markerPath); err != nil || !bytes.Equal(existing, raw) {
				t.Fatalf("marker=%q err=%v", existing, err)
			}
			if _, err := os.Stat(tmpName); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("temporary marker was not removed: %v", err)
			}
		})
	}
}

func TestPublishMarkerCleansTemporaryOnMatchingNoReplaceConflict(t *testing.T) {
	dir := t.TempDir()
	raw := []byte("marker")
	temporaryPath := filepath.Join(dir, ".marker.tmp")
	markerPath := filepath.Join(dir, "marker.json")
	if err := os.WriteFile(temporaryPath, raw, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(markerPath, raw, 0o640); err != nil {
		t.Fatal(err)
	}
	removed, err := publishMarker(temporaryPath, markerPath, raw)
	if err != nil {
		t.Fatal(err)
	}
	if !removed {
		t.Fatal("matching temporary marker was not reported as removed")
	}
	if _, err := os.Stat(temporaryPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary marker was not removed: %v", err)
	}
}

func TestPublishMarkerAcceptsDurableMarkerWhenTemporaryCleanupFails(t *testing.T) {
	dir := t.TempDir()
	raw := []byte("marker")
	temporaryPath := filepath.Join(dir, ".marker.tmp")
	markerPath := filepath.Join(dir, "marker.json")
	if err := os.Mkdir(temporaryPath, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(temporaryPath, "child"), []byte("leftover"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(markerPath, raw, 0o640); err != nil {
		t.Fatal(err)
	}
	uncertain, err := publishMarker(temporaryPath, markerPath, raw)
	if err != nil {
		t.Fatal(err)
	}
	if !uncertain {
		t.Fatal("cleanup failure did not invalidate capacity accounting")
	}
	if _, err := os.Stat(temporaryPath); err != nil {
		t.Fatalf("temporary path unexpectedly removed: %v", err)
	}
}

func TestDurableFileQuarantinesUnknownNonEmptyObject(t *testing.T) {
	root := t.TempDir()
	resource := api.Resource{SandboxID: "sb", ClusterName: "prod-a", Namespace: "ns", PodName: "pod", PodUID: "uid", NodeName: "node", Container: "sandbox"}
	dir := filepath.Join(root, "prod-a", "ns", "sb", "uid")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "sandbox.log")
	if err := os.WriteFile(path, []byte("orphan"), 0o640); err != nil {
		t.Fatal(err)
	}
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sink, err := New(Config{Root: root, ClusterID: "prod-a", MaxFileBytes: 1 << 20, MaxFiles: 4, MaxTotalBytes: 1 << 24}, db)
	if err != nil {
		t.Fatal(err)
	}
	batch := api.Batch{StreamRef: api.StreamRef{ID: "container-logs/uid/sandbox"}, Items: []api.BatchItem{{RecordID: "r", Record: api.Record{Kind: api.RecordKindContainerLog, Timestamp: time.Now().UTC(), Body: []byte("new"), Resource: resource, Attributes: map[string]string{"stream": "stdout"}}}}}
	if err := sink.Consume(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(root, ".quarantine"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("quarantine entries=%v err=%v", entries, err)
	}
	orphan, err := os.ReadFile(filepath.Join(root, ".quarantine", entries[0].Name()))
	if err != nil || string(orphan) != "orphan" {
		t.Fatalf("orphan=%q err=%v", orphan, err)
	}
}

func TestDurableFileCleanupStagesWholeFamily(t *testing.T) {
	root := t.TempDir()
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sink, err := New(Config{Root: root, ClusterID: "prod-a", MaxFileBytes: 1 << 20, MaxFiles: 4, MaxTotalBytes: 1 << 24}, db)
	if err != nil {
		t.Fatal(err)
	}
	resource := api.Resource{SandboxID: "sb", ClusterName: "prod-a", Namespace: "ns", PodName: "pod", PodUID: "uid", NodeName: "node", Container: "sandbox", LogDirectory: filepath.Join(t.TempDir(), "gone")}
	streamRef := api.StreamRef{ID: "container-logs/uid/sandbox"}
	batch := api.Batch{StreamRef: streamRef, Items: []api.BatchItem{{RecordID: "r", Record: api.Record{Kind: api.RecordKindContainerLog, Timestamp: time.Now().UTC(), Body: []byte("data"), Resource: resource, Attributes: map[string]string{"stream": "stdout"}}}}}
	if err := sink.Consume(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	request := api.FinalizeRequest{FinalizeID: "f", TargetID: "target", StreamRef: streamRef, Revision: 1, CoverageStartedAt: time.Now().UTC().Add(-time.Minute).Truncate(time.Second), Resource: resource, FinalizedAt: time.Now().UTC().Truncate(time.Second)}
	if err := sink.Finalize(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(-time.Hour)
	if err := db.PutSourceStream(state.SourceStream{StreamRef: streamRef.ID, Resource: state.FrozenResource{SandboxID: resource.SandboxID, ClusterName: resource.ClusterName, Namespace: resource.Namespace, PodName: resource.PodName, PodUID: resource.PodUID, NodeName: resource.NodeName, Container: resource.Container, LogDirectory: resource.LogDirectory, Terminated: true}, Revision: 1, AcknowledgedRevision: 1, Ended: true, RepairDeadline: &deadline}); err != nil {
		t.Fatal(err)
	}
	if err := sink.CollectExpired(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	family := filepath.Join(root, "prod-a", "ns", "sb", "uid")
	if _, err := os.Stat(family); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("family still exists: %v", err)
	}
	if _, found, err := db.GetSourceStream(streamRef.ID); err != nil || found {
		t.Fatalf("source state found=%v err=%v", found, err)
	}
	if _, found, err := db.GetSinkStream(name, streamRef.ID); err != nil || found {
		t.Fatalf("sink state found=%v err=%v", found, err)
	}
}

func TestDurableFileCleanupCheckpointConflictsAreNonRetryable(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*state.SinkStream, string)
	}{
		{
			name: "staging-path",
			mutate: func(stream *state.SinkStream, _ string) {
				stream.CleanupPath = "wrong-staging-path"
			},
		},
		{
			name: "cleanup-phase",
			mutate: func(stream *state.SinkStream, staging string) {
				stream.CleanupPath = staging
				stream.CleanupPhase = "unknown"
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			db, err := state.Open(t.TempDir(), "target", 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()

			streamRef := "container-logs/uid/sandbox"
			deadline := time.Now().Add(-time.Hour)
			logDirectory := filepath.Join(t.TempDir(), "missing")
			if err := db.PutSourceStream(state.SourceStream{
				StreamRef:            streamRef,
				Resource:             state.FrozenResource{SandboxID: "sb", ClusterName: "prod-a", Namespace: "ns", PodName: "pod", PodUID: "uid", NodeName: "node", Container: "sandbox", LogDirectory: logDirectory},
				Revision:             1,
				AcknowledgedRevision: 1,
				Ended:                true,
				RepairDeadline:       &deadline,
			}); err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256([]byte(streamRef))
			staging := filepath.Join(root, ".gc", hex.EncodeToString(digest[:]))
			stream := state.SinkStream{StreamRef: streamRef, FinalizedRevision: 1, CleanupPhase: "planned", CleanupPath: staging}
			test.mutate(&stream, staging)
			if err := db.PutSinkStream(name, stream); err != nil {
				t.Fatal(err)
			}

			sink := &Sink{cfg: Config{Root: root}, state: db, writers: make(map[string]*writer)}
			err = sink.CollectExpired(context.Background(), time.Now())
			if err == nil || api.IsRetryableError(err) {
				t.Fatalf("CollectExpired() error=%v retryable=%v", err, api.IsRetryableError(err))
			}
		})
	}
}

func TestDurableFileCleanupContinuesAfterPoisonedStream(t *testing.T) {
	root := t.TempDir()
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	deadline := time.Now().Add(-time.Hour)
	poisonedRef := "a-poisoned"
	cleanRef := "z-clean"
	for _, item := range []struct {
		streamRef string
		sandboxID string
		podUID    string
	}{
		{streamRef: poisonedRef, sandboxID: "sb-poisoned", podUID: "uid-poisoned"},
		{streamRef: cleanRef, sandboxID: "sb-clean", podUID: "uid-clean"},
	} {
		resource := state.FrozenResource{SandboxID: item.sandboxID, ClusterName: "prod-a", Namespace: "ns", PodName: "pod", PodUID: item.podUID, NodeName: "node", Container: "sandbox", LogDirectory: filepath.Join(t.TempDir(), "missing")}
		if err := db.PutSourceStream(state.SourceStream{StreamRef: item.streamRef, Resource: resource, Revision: 1, AcknowledgedRevision: 1, Ended: true, RepairDeadline: &deadline}); err != nil {
			t.Fatal(err)
		}
		stream := state.SinkStream{StreamRef: item.streamRef, FinalizedRevision: 1}
		if item.streamRef == poisonedRef {
			stream.CleanupPhase = "planned"
			stream.CleanupPath = "wrong-staging-path"
		}
		if err := db.PutSinkStream(name, stream); err != nil {
			t.Fatal(err)
		}
	}
	cleanFamily := filepath.Join(root, "prod-a", "ns", "sb-clean", "uid-clean")
	if err := os.MkdirAll(cleanFamily, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cleanFamily, "sandbox.log"), []byte("data"), 0o640); err != nil {
		t.Fatal(err)
	}
	sink := &Sink{cfg: Config{Root: root}, state: db, writers: make(map[string]*writer)}
	err = sink.CollectExpired(context.Background(), time.Now())
	if err == nil || api.IsRetryableError(err) || !strings.Contains(err.Error(), poisonedRef) {
		t.Fatalf("CollectExpired() error=%v retryable=%v", err, api.IsRetryableError(err))
	}
	if _, found, err := db.GetSourceStream(cleanRef); err != nil || found {
		t.Fatalf("clean source found=%v err=%v", found, err)
	}
	if _, found, err := db.GetSinkStream(name, cleanRef); err != nil || found {
		t.Fatalf("clean sink found=%v err=%v", found, err)
	}
	if _, err := os.Stat(cleanFamily); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("clean family still exists: %v", err)
	}
	if _, found, err := db.GetSourceStream(poisonedRef); err != nil || !found {
		t.Fatalf("poisoned source found=%v err=%v", found, err)
	}
}

func TestStartNextGenerationRejectsOverflowedCheckpoint(t *testing.T) {
	sink := &Sink{cfg: Config{MaxFiles: 2}}
	w := &writer{stream: state.SinkStream{Generation: ^uint64(0)}}
	err := sink.startNextGeneration(w)
	if err == nil || api.IsRetryableError(err) || !strings.Contains(err.Error(), "generation limit") {
		t.Fatalf("startNextGeneration() error=%v retryable=%v", err, api.IsRetryableError(err))
	}
}

func TestStartNextGenerationRetriesTransitionCheckpoint(t *testing.T) {
	root := t.TempDir()
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := &failGenerationTransitionStore{DB: db, fail: true}
	resource := api.Resource{SandboxID: "sb", ClusterName: "cluster", Namespace: "ns", PodUID: "uid", Container: "sandbox"}
	dir, err := familyDir(root, resource)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	w := &writer{stream: state.SinkStream{StreamRef: "stream", ObjectKey: "cluster/ns/sb/uid/sandbox.log", CurrentClosed: true}, resource: resource}
	sink := &Sink{cfg: Config{Root: root, MaxFiles: 2}, state: store}
	if err := sink.startNextGeneration(w); err == nil || !strings.Contains(err.Error(), "injected generation transition failure") {
		t.Fatalf("first startNextGeneration() error=%v", err)
	}
	if w.stream.GenerationTransition != nil {
		t.Fatalf("failed transition remained in memory: %+v", w.stream.GenerationTransition)
	}
	if err := sink.startNextGeneration(w); err != nil {
		t.Fatal(err)
	}
	defer w.file.Close()
	persisted, found, err := db.GetSinkStream(name, w.stream.StreamRef)
	if err != nil || !found || persisted.Generation != 1 || persisted.GenerationTransition != nil {
		t.Fatalf("persisted stream=%+v found=%v err=%v", persisted, found, err)
	}
}

func lineBytes(batch api.Batch) []byte {
	item := batch.Items[0]
	return []byte(item.Record.Timestamp.Format(time.RFC3339Nano) + " " + item.Record.Attributes["stream"] + " " + string(item.Record.Body) + "\n")
}
