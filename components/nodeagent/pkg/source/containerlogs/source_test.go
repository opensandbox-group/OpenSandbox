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

package containerlogs

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/internal/logger"
	"github.com/alibaba/opensandbox/nodeagent/pkg/api"
	"github.com/alibaba/opensandbox/nodeagent/pkg/config"
	"github.com/alibaba/opensandbox/nodeagent/pkg/state"
	"github.com/alibaba/opensandbox/nodeagent/pkg/store"
	"github.com/fsnotify/fsnotify"
)

type fakeView struct {
	resources []streamResource
	changes   chan struct{}
}

func testResource(resource api.Resource, logDirectory ...any) streamResource {
	result := streamResource{Resource: resource}
	for _, value := range logDirectory {
		switch value := value.(type) {
		case string:
			result.LogDirectory = value
		case bool:
			result.Terminated = value
		}
	}
	return result
}

func testLogDirectory(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	directory := filepath.Join(root, "ns_pod_uid", "sandbox")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	return directory
}

func newTestSource(cfg sourceConfig, view store.View, stateValue any, log logger.Logger, onError func(error)) *containerLogSource {
	checkpoints, ok := stateValue.(checkpointStore)
	if stateValue == nil {
		checkpoints = nil
	} else if !ok {
		db, dbOK := stateValue.(*state.DB)
		if !dbOK {
			panic(fmt.Sprintf("unsupported test checkpoint store %T", stateValue))
		}
		sourceState, err := db.SourceState(sourceName)
		if err != nil {
			panic(err)
		}
		checkpoints, err = sourceState.LegacyContainerLogCheckpoint()
		if err != nil {
			panic(err)
		}
	}
	if cfg.LogRoot == "" {
		if resources, ok := view.(*fakeView); ok && len(resources.resources) != 0 {
			cfg.LogRoot = filepath.Dir(filepath.Dir(resources.resources[0].LogDirectory))
			for _, resource := range resources.resources {
				expected := filepath.Join(cfg.LogRoot, fmt.Sprintf("%s_%s_%s", resource.Namespace, resource.PodName, resource.PodUID), resource.Container)
				if filepath.Clean(resource.LogDirectory) != expected {
					panic(fmt.Sprintf("test resource log directory %q does not match LogRoot layout %q", resource.LogDirectory, expected))
				}
			}
		}
	}
	return newSource(cfg, view, checkpoints, log, onError)
}

func newTestCheckpointStore(t testing.TB, db *state.DB) checkpointStore {
	t.Helper()
	sourceState, err := db.SourceState(sourceName)
	if err != nil {
		t.Fatal(err)
	}
	checkpoints, err := sourceState.LegacyContainerLogCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	return checkpoints
}

type countingCheckpointStore struct {
	checkpointStore
	putSourceCalls    int
	commitSourceCalls int
	failPutSourceCall int
	failCommitCall    int
}

func (s *countingCheckpointStore) PutSourceStream(stream state.SourceStream) error {
	s.putSourceCalls++
	if s.putSourceCalls == s.failPutSourceCall {
		s.failPutSourceCall = 0
		return errors.New("injected PutSourceStream failure")
	}
	return s.checkpointStore.PutSourceStream(stream)
}

func (s *countingCheckpointStore) CommitSource(checkpoints []state.FileCheckpoint, stream state.SourceStream) error {
	s.commitSourceCalls++
	if s.commitSourceCalls == s.failCommitCall {
		s.failCommitCall = 0
		return errors.New("injected CommitSource failure")
	}
	return s.checkpointStore.CommitSource(checkpoints, stream)
}

func TestAcknowledgeClassifiesInvalidTokenAsNonRetryable(t *testing.T) {
	source := &containerLogSource{}
	err := source.Acknowledge(context.Background(), []api.AckResult{{Token: api.AckToken{Source: "other"}}})
	if err == nil || api.IsRetryableError(err) {
		t.Fatalf("error=%v retryable=%v", err, api.IsRetryableError(err))
	}
}

func testEndToken(streamID string, revision uint64) api.EndToken {
	ref := api.StreamRef{ID: streamID, Kind: api.RecordKindContainerLog}
	value := []byte(strconv.FormatUint(revision, 10))
	return api.EndToken{ID: endTokenID(streamID, value), Source: sourceName, StreamRef: ref, Value: value}
}

func TestAcknowledgeEndClassifiesValidationErrorsAsNonRetryable(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	runtime := &streamRuntime{
		revision:  2,
		persisted: state.SourceStream{StreamRef: "stream", Resource: testFrozenResourceState(), Revision: 2, AcknowledgedRevision: 1, FinalizingRevision: 2, FinalizingOutcome: &state.OutcomeSnapshot{LossReasons: []string{}}},
		pending:   []*pendingSpan{{}},
	}
	source := &containerLogSource{state: newTestCheckpointStore(t, db), streams: map[string]*streamRuntime{"stream": runtime}}
	wrongSource := testEndToken("stream", 2)
	wrongSource.Source = "other"
	badValue := testEndToken("stream", 2)
	badValue.Value = []byte("bad")
	missingStream := testEndToken("missing", 1)
	wrongRevision := testEndToken("stream", 3)
	badKind := testEndToken("stream", 2)
	badKind.StreamRef.Kind = "other"
	badID := testEndToken("stream", 2)
	badID.ID = "wrong"
	for _, test := range []struct {
		name  string
		token api.EndToken
	}{
		{name: "source", token: wrongSource},
		{name: "value", token: badValue},
		{name: "stream", token: missingStream},
		{name: "revision", token: wrongRevision},
		{name: "kind", token: badKind},
		{name: "id", token: badID},
		{name: "pending", token: testEndToken("stream", 2)},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := source.AcknowledgeEnd(context.Background(), test.token)
			if err == nil || api.IsRetryableError(err) {
				t.Fatalf("AcknowledgeEnd() error=%v retryable=%v", err, api.IsRetryableError(err))
			}
		})
	}
}

func TestAcknowledgeEndLeavesStateErrorsRetryable(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	runtime := &streamRuntime{revision: 1, persisted: state.SourceStream{StreamRef: "stream", Resource: testFrozenResourceState(), Revision: 1, FinalizingRevision: 1, FinalizingOutcome: &state.OutcomeSnapshot{LossReasons: []string{}}}}
	source := &containerLogSource{state: newTestCheckpointStore(t, db), streams: map[string]*streamRuntime{"stream": runtime}}
	err = source.AcknowledgeEnd(context.Background(), testEndToken("stream", 1))
	if err == nil || !api.IsRetryableError(err) {
		t.Fatalf("AcknowledgeEnd() error=%v retryable=%v", err, api.IsRetryableError(err))
	}
}

func TestAcknowledgeEndRecoversWithoutRuntime(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	persisted := state.SourceStream{
		StreamRef:          "stream",
		Resource:           testFrozenResourceState(),
		Revision:           1,
		FinalizingRevision: 1,
		FinalizingOutcome:  &state.OutcomeSnapshot{LossReasons: []string{}},
	}
	if err := db.PutSourceStream(persisted); err != nil {
		t.Fatal(err)
	}
	source := &containerLogSource{
		cfg:     sourceConfig{EndedStateRetention: time.Hour},
		state:   newTestCheckpointStore(t, db),
		streams: make(map[string]*streamRuntime),
		epochID: "agent-epoch",
	}
	token := testEndToken("stream", 1)
	if err := source.AcknowledgeEnd(context.Background(), token); err != nil {
		t.Fatal(err)
	}
	if err := source.AcknowledgeEnd(context.Background(), token); err != nil {
		t.Fatalf("idempotent AcknowledgeEnd() error = %v", err)
	}
	got, found, err := db.GetSourceStream("stream")
	if err != nil || !found {
		t.Fatalf("GetSourceStream() found=%v err=%v", found, err)
	}
	if got.AcknowledgedRevision != 1 || got.FinalizingRevision != 0 || !got.Ended || got.FinalizingOutcome != nil || got.RepairDeadline == nil {
		t.Fatalf("source stream after recovered acknowledgement = %+v", got)
	}
}

func TestSourceDoesNotReportContextCancellation(t *testing.T) {
	log, err := logger.New(logger.Config{OutputPaths: []string{"stdout"}})
	if err != nil {
		t.Fatal(err)
	}
	reported := false
	source := &containerLogSource{log: log.Named(sourceName), onError: func(error) { reported = true }}
	source.fail(context.Canceled)
	if reported {
		t.Fatal("context cancellation was reported as a source failure")
	}
}

func TestAcknowledgeRecordsDropForEveryPendingSpan(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	fingerprint := fileFingerprint{Device: 1, Inode: 2, PrefixHash: "hash", HashBytes: 4}
	file := &fileRuntime{fileID: "file", path: "/logs/0.log", observedSize: 10, fingerprint: fingerprint}
	first := sourceSpan{FileID: file.fileID, Path: file.path, StartOffset: 0, EndOffset: 5, Device: 1, Inode: 2, PrefixHash: "hash", HashBytes: 4, fingerprint: fingerprint}
	second := first
	second.StartOffset = 5
	second.EndOffset = 10
	runtime := &streamRuntime{
		files:     map[string]*fileRuntime{file.path: file},
		pending:   []*pendingSpan{{span: first, file: file, tokenID: "record"}, {span: second, file: file, tokenID: "record"}},
		revision:  1,
		persisted: state.SourceStream{StreamRef: "stream", Resource: testFrozenResourceState(), Revision: 1},
	}
	source := &containerLogSource{state: newTestCheckpointStore(t, db), streams: map[string]*streamRuntime{"stream": runtime}}
	combined := first
	combined.EndOffset = second.EndOffset
	value, err := json.Marshal(tokenValue{Spans: []sourceSpan{combined}, Revision: 1})
	if err != nil {
		t.Fatal(err)
	}
	err = source.Acknowledge(context.Background(), []api.AckResult{{
		Token:       api.AckToken{ID: "record", Source: sourceName, StreamRef: api.StreamRef{ID: "stream"}, Value: value},
		Disposition: api.AckIntentionalDrop,
		Reason:      "pipeline-policy-drop",
		Guarantee:   api.GuaranteeDurable,
	}})
	if err != nil {
		t.Fatal(err)
	}
	stream, found, err := db.GetSourceStream("stream")
	if err != nil || !found {
		t.Fatalf("GetSourceStream() found=%v err=%v", found, err)
	}
	if len(stream.Drops) != 2 || stream.Drops[0].FromOffset != 0 || stream.Drops[0].ToOffset != 5 || stream.Drops[1].FromOffset != 5 || stream.Drops[1].ToOffset != 10 {
		t.Fatalf("drops=%+v, want both physical source spans", stream.Drops)
	}
}

func TestCommitCompletedDoesNotReplaceReboundPathCheckpoint(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	path := "/logs/0.log"
	oldFingerprint := fileFingerprint{Device: 1, Inode: 1, PrefixHash: "old", HashBytes: 3}
	newFingerprint := fileFingerprint{Device: 1, Inode: 2, PrefixHash: "new", HashBytes: 3}
	oldFile := &fileRuntime{fileID: "old", path: path, observedSize: 10, fingerprint: oldFingerprint}
	newFile := &fileRuntime{fileID: "new", path: path, observedSize: 5, fingerprint: newFingerprint}
	if err := commitFileCheckpointForTest(db, checkpointFor("stream", 1, newFile, 0)); err != nil {
		t.Fatal(err)
	}
	oldSpan := sourceSpan{FileID: oldFile.fileID, Path: path, StartOffset: 0, EndOffset: 10, Device: 1, Inode: 1, PrefixHash: "old", HashBytes: 3, fingerprint: oldFingerprint}
	newSpan := sourceSpan{FileID: newFile.fileID, Path: path, StartOffset: 0, EndOffset: 5, Device: 1, Inode: 2, PrefixHash: "new", HashBytes: 3, fingerprint: newFingerprint}
	runtime := &streamRuntime{
		files:     map[string]*fileRuntime{path: newFile},
		pending:   []*pendingSpan{{span: oldSpan, file: oldFile, complete: true}, {span: newSpan, file: newFile}},
		revision:  1,
		persisted: state.SourceStream{StreamRef: "stream", Resource: testFrozenResourceState(), Revision: 1},
	}
	source := &containerLogSource{state: newTestCheckpointStore(t, db)}
	if err := source.commitCompletedLocked(runtime); err != nil {
		t.Fatal(err)
	}
	checkpoint, found, err := db.GetFileCheckpoint("stream", path)
	if err != nil || !found || checkpoint.FileID != newFile.fileID || checkpoint.Offset != 0 {
		t.Fatalf("checkpoint=%+v found=%v err=%v, want rebound file at offset zero", checkpoint, found, err)
	}
	if oldFile.committed != 10 || newFile.committed != 0 || len(runtime.pending) != 1 {
		t.Fatalf("old committed=%d new committed=%d pending=%d", oldFile.committed, newFile.committed, len(runtime.pending))
	}
	runtime.pending[0].complete = true
	if err := source.commitCompletedLocked(runtime); err != nil {
		t.Fatal(err)
	}
	checkpoint, found, err = db.GetFileCheckpoint("stream", path)
	if err != nil || !found || checkpoint.FileID != newFile.fileID || checkpoint.Offset != 5 || newFile.committed != 5 {
		t.Fatalf("checkpoint=%+v found=%v err=%v new committed=%d", checkpoint, found, err, newFile.committed)
	}
}

func TestAcknowledgeCommitsSameStreamBatchAtomically(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	path := "/logs/0.log"
	oldFingerprint := fileFingerprint{Device: 1, Inode: 1, PrefixHash: "old", HashBytes: 3}
	newFingerprint := fileFingerprint{Device: 1, Inode: 2, PrefixHash: "new", HashBytes: 3}
	oldFile := &fileRuntime{fileID: "old", path: path, observedSize: 10, fingerprint: oldFingerprint}
	newFile := &fileRuntime{fileID: "new", path: path, observedSize: 5, fingerprint: newFingerprint}
	if err := commitFileCheckpointForTest(db, checkpointFor("stream", 1, newFile, 0)); err != nil {
		t.Fatal(err)
	}
	oldSpan := sourceSpan{FileID: oldFile.fileID, Path: path, StartOffset: 0, EndOffset: 10, Device: 1, Inode: 1, PrefixHash: "old", HashBytes: 3, fingerprint: oldFingerprint}
	newSpan := sourceSpan{FileID: newFile.fileID, Path: path, StartOffset: 0, EndOffset: 5, Device: 1, Inode: 2, PrefixHash: "new", HashBytes: 3, fingerprint: newFingerprint}
	runtime := &streamRuntime{
		files:     map[string]*fileRuntime{path: newFile},
		pending:   []*pendingSpan{{span: oldSpan, file: oldFile, tokenID: "old-record"}, {span: newSpan, file: newFile, tokenID: "new-record"}},
		revision:  1,
		persisted: state.SourceStream{StreamRef: "stream", Resource: testFrozenResourceState(), Revision: 1},
	}
	checkpoints := &countingCheckpointStore{checkpointStore: newTestCheckpointStore(t, db), failCommitCall: 2}
	source := &containerLogSource{state: checkpoints, streams: map[string]*streamRuntime{"stream": runtime}}
	resultFor := func(tokenID string, span sourceSpan) api.AckResult {
		value, marshalErr := json.Marshal(tokenValue{Spans: []sourceSpan{span}, Revision: 1})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		return api.AckResult{Token: api.AckToken{ID: tokenID, Source: sourceName, StreamRef: api.StreamRef{ID: "stream"}, Value: value}, Disposition: api.AckDelivered, Guarantee: api.GuaranteeDurable}
	}
	if err := source.Acknowledge(context.Background(), []api.AckResult{resultFor("old-record", oldSpan), resultFor("new-record", newSpan)}); err != nil {
		t.Fatal(err)
	}
	if checkpoints.commitSourceCalls != 1 {
		t.Fatalf("CommitSource calls=%d, want one transaction for the stream batch", checkpoints.commitSourceCalls)
	}
	checkpoint, found, err := db.GetFileCheckpoint("stream", path)
	if err != nil || !found || checkpoint.FileID != newFile.fileID || checkpoint.Offset != 5 {
		t.Fatalf("checkpoint=%+v found=%v err=%v", checkpoint, found, err)
	}
	if len(runtime.pending) != 0 || oldFile.committed != 10 || newFile.committed != 5 {
		t.Fatalf("pending=%d old committed=%d new committed=%d", len(runtime.pending), oldFile.committed, newFile.committed)
	}
}

func TestAcknowledgeFailsClosedOnSupersededCheckpoint(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	fingerprint := fileFingerprint{Device: 1, Inode: 2, PrefixHash: "hash", HashBytes: 4}
	newer := &fileRuntime{fileID: "file", path: "/logs/0.log.1", committed: 20, observedSize: 20, fingerprint: fingerprint}
	if err := commitFileCheckpointForTest(db, checkpointFor("stream", 2, newer, newer.committed)); err != nil {
		t.Fatal(err)
	}
	stale := &fileRuntime{fileID: newer.fileID, path: "/logs/0.log", readOffset: 10, committed: 5, observedSize: 10, fingerprint: fingerprint}
	span := sourceSpan{FileID: stale.fileID, Path: stale.path, StartOffset: 5, EndOffset: 10, Device: fingerprint.Device, Inode: fingerprint.Inode, PrefixHash: fingerprint.PrefixHash, HashBytes: fingerprint.HashBytes, fingerprint: fingerprint}
	runtime := &streamRuntime{
		files:     map[string]*fileRuntime{stale.path: stale},
		pending:   []*pendingSpan{{span: span, file: stale, tokenID: "record"}},
		revision:  1,
		persisted: state.SourceStream{StreamRef: "stream", Resource: testFrozenResourceState(), Revision: 1},
	}
	source := &containerLogSource{state: newTestCheckpointStore(t, db), streams: map[string]*streamRuntime{"stream": runtime}}
	value, err := json.Marshal(tokenValue{Spans: []sourceSpan{span}, Revision: runtime.revision})
	if err != nil {
		t.Fatal(err)
	}
	err = source.Acknowledge(context.Background(), []api.AckResult{{
		Token:       api.AckToken{ID: "record", Source: sourceName, StreamRef: api.StreamRef{ID: "stream"}, Value: value},
		Disposition: api.AckDelivered,
		Guarantee:   api.GuaranteeDurable,
	}})
	if !errors.Is(err, state.ErrFileCheckpointSuperseded) || api.IsRetryableError(err) {
		t.Fatalf("Acknowledge() error=%v retryable=%v, want non-retryable superseded checkpoint", err, api.IsRetryableError(err))
	}
	if stale.committed != 5 || runtime.pending[0].complete || runtime.persisted.Guarantee != "" {
		t.Fatalf("committed=%d pending.complete=%v guarantee=%q, want in-memory state rolled back", stale.committed, runtime.pending[0].complete, runtime.persisted.Guarantee)
	}
	stored, found, err := db.GetSourceStream("stream")
	if err != nil || !found || stored.HadDrops || stored.Guarantee != "" {
		t.Fatalf("GetSourceStream()=%+v found=%v err=%v, want failed commit to preserve prior state", stored, found, err)
	}
}

func TestRepairGapResolvesOnlyAfterContiguousAckAndStableEOF(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	fingerprint := fileFingerprint{Device: 1, Inode: 2, PrefixHash: "hash", HashBytes: 4}
	gap := state.GapRecord{ID: "gap", FileID: "file", Path: "/logs/0.log", FromOffset: 0, Device: 1, Inode: 2, PrefixHash: "hash", HashBytes: 4, Reason: "file-reclaimed"}
	file := &fileRuntime{fileID: gap.FileID, path: gap.Path, readOffset: 10, observedSize: 10, fingerprint: fingerprint}
	persisted := state.SourceStream{StreamRef: "stream", Resource: testFrozenResourceState(), Revision: 2, AcknowledgedRevision: 1, HadSourceGaps: true, LossReasons: []string{gap.Reason}, Gaps: []state.GapRecord{gap}}
	if err := bindRepairGap(persisted, file); err != nil {
		t.Fatal(err)
	}
	if file.repairGapID != gap.ID {
		t.Fatalf("repair gap id=%q", file.repairGapID)
	}
	first := sourceSpan{FileID: file.fileID, Path: file.path, StartOffset: 0, EndOffset: 5, Device: 1, Inode: 2, PrefixHash: "hash", HashBytes: 4, RepairGapID: gap.ID, fingerprint: fingerprint}
	second := first
	second.StartOffset = 5
	second.EndOffset = 10
	runtime := &streamRuntime{
		files:     map[string]*fileRuntime{file.path: file},
		pending:   []*pendingSpan{{span: first, file: file, complete: true}, {span: second, file: file}},
		revision:  2,
		persisted: persisted,
	}
	source := &containerLogSource{state: newTestCheckpointStore(t, db)}
	if err := source.commitCompletedLocked(runtime); err != nil {
		t.Fatal(err)
	}
	if runtime.persisted.Gaps[0].RepairOffset == nil || *runtime.persisted.Gaps[0].RepairOffset != 5 || runtime.persisted.Gaps[0].Resolved || !runtime.persisted.HadSourceGaps {
		t.Fatalf("gap after partial repair=%+v outcome=%+v", runtime.persisted.Gaps[0], runtime.persisted)
	}
	runtime.pending[0].complete = true
	if err := source.commitCompletedLocked(runtime); err != nil {
		t.Fatal(err)
	}
	if runtime.persisted.Gaps[0].RepairOffset == nil || *runtime.persisted.Gaps[0].RepairOffset != 10 || runtime.persisted.Gaps[0].ToOffset != nil || runtime.persisted.Gaps[0].Resolved {
		t.Fatalf("gap before stable EOF=%+v", runtime.persisted.Gaps[0])
	}
	runtime.mu.Lock()
	err = source.resolveStableRepairGapsLocked(runtime)
	runtime.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	resolved := runtime.persisted.Gaps[0]
	if resolved.ToOffset == nil || *resolved.ToOffset != 10 || !resolved.Resolved || runtime.persisted.HadSourceGaps || len(runtime.persisted.LossReasons) != 0 || file.repairGapID != "" {
		t.Fatalf("resolved gap=%+v outcome=%+v repairGapID=%q", resolved, runtime.persisted, file.repairGapID)
	}
}

func TestRepairAckAfterGapBecomesCoverageStillCommits(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	fingerprint := fileFingerprint{Device: 1, Inode: 2, PrefixHash: "hash", HashBytes: 4}
	gap := state.GapRecord{ID: "gap", FileID: "file", Path: "/logs/0.log", ObservedSize: 10, Device: 1, Inode: 2, PrefixHash: "hash", HashBytes: 4, Reason: "file-reclaimed", Coverage: true}
	file := &fileRuntime{fileID: gap.FileID, path: gap.Path, readOffset: 10, observedSize: 10, fingerprint: fingerprint}
	first := sourceSpan{FileID: file.fileID, Path: file.path, StartOffset: 0, EndOffset: 5, Device: 1, Inode: 2, PrefixHash: "hash", HashBytes: 4, RepairGapID: gap.ID, fingerprint: fingerprint}
	second := first
	second.StartOffset = first.EndOffset
	second.EndOffset = 10
	runtime := &streamRuntime{
		files:     map[string]*fileRuntime{file.path: file},
		pending:   []*pendingSpan{{span: first, file: file, complete: true}, {span: second, file: file, complete: true}},
		revision:  1,
		persisted: state.SourceStream{StreamRef: "stream", Resource: testFrozenResourceState(), Revision: 1, HadSourceGaps: true, LossReasons: []string{gap.Reason}, Gaps: []state.GapRecord{gap}},
	}
	if err := (&containerLogSource{state: newTestCheckpointStore(t, db)}).commitCompletedLocked(runtime); err != nil {
		t.Fatal(err)
	}
	if file.committed != second.EndOffset || len(runtime.pending) != 0 {
		t.Fatalf("committed=%d pending=%d", file.committed, len(runtime.pending))
	}
	persistedGap := runtime.persisted.Gaps[0]
	if !persistedGap.Coverage || persistedGap.Resolved || persistedGap.RepairOffset == nil || *persistedGap.RepairOffset != second.EndOffset || !runtime.persisted.HadSourceGaps {
		t.Fatalf("gap=%+v outcome=%+v", persistedGap, runtime.persisted)
	}
}

func TestShrunkenRepairFileMakesBoundGapCoverage(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	gap := state.GapRecord{ID: "gap", FileID: "file", Path: "/logs/old.log", ObservedSize: 100, Reason: "file-reclaimed"}
	file := &fileRuntime{fileID: gap.FileID, path: "/logs/recovered.log", repairGapID: gap.ID}
	runtime := &streamRuntime{persisted: state.SourceStream{StreamRef: "stream", Resource: testFrozenResourceState(), Revision: 1, HadSourceGaps: true, LossReasons: []string{gap.Reason}, Gaps: []state.GapRecord{gap}}}
	runtime.mu.Lock()
	err = (&containerLogSource{state: newTestCheckpointStore(t, db)}).makeBoundRepairGapCoverageLocked(runtime, file)
	runtime.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if file.repairGapID != "" || !runtime.persisted.Gaps[0].Coverage || runtime.persisted.Gaps[0].Resolved || !runtime.persisted.HadSourceGaps {
		t.Fatalf("file=%+v gap=%+v outcome=%+v", file, runtime.persisted.Gaps[0], runtime.persisted)
	}
}

func TestCommitCompletedHandlesRepairAfterFileRuntimeRebind(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	fingerprint := fileFingerprint{Device: 1, Inode: 2, PrefixHash: "hash", HashBytes: 4}
	gap := state.GapRecord{ID: "gap", FileID: "file", Path: "/logs/0.log", ObservedSize: 20, Device: 1, Inode: 2, PrefixHash: "hash", HashBytes: 4, Reason: "file-reclaimed"}
	oldFile := &fileRuntime{fileID: gap.FileID, path: gap.Path, observedSize: 20, fingerprint: fingerprint}
	repairPath := "/logs/0.log.20260723"
	repairFile := &fileRuntime{fileID: gap.FileID, path: repairPath, readOffset: 10, observedSize: 10, fingerprint: fingerprint, repairGapID: gap.ID}
	oldSpan := sourceSpan{FileID: gap.FileID, Path: gap.Path, EndOffset: 20, Device: 1, Inode: 2, PrefixHash: "hash", HashBytes: 4, fingerprint: fingerprint}
	repairSpan := oldSpan
	repairSpan.Path = repairPath
	repairSpan.EndOffset = 10
	repairSpan.RepairGapID = gap.ID
	runtime := &streamRuntime{
		files:     map[string]*fileRuntime{repairPath: repairFile},
		pending:   []*pendingSpan{{span: oldSpan, file: oldFile, complete: true}, {span: repairSpan, file: repairFile, complete: true}},
		revision:  2,
		persisted: state.SourceStream{StreamRef: "stream", Resource: testFrozenResourceState(), Revision: 2, AcknowledgedRevision: 1, HadSourceGaps: true, LossReasons: []string{gap.Reason}, Gaps: []state.GapRecord{gap}},
	}
	source := &containerLogSource{state: newTestCheckpointStore(t, db)}
	if err := source.commitCompletedLocked(runtime); err != nil {
		t.Fatal(err)
	}
	checkpoint, found, err := db.GetFileCheckpoint("stream", repairPath)
	if err != nil || !found || checkpoint.FileID != gap.FileID || checkpoint.Offset != 10 {
		t.Fatalf("checkpoint=%+v found=%v err=%v", checkpoint, found, err)
	}
	if _, found, err := db.GetFileCheckpoint("stream", gap.Path); err != nil || found {
		t.Fatalf("stale checkpoint found=%v err=%v", found, err)
	}
	if oldFile.committed != 20 || repairFile.committed != 10 || len(runtime.pending) != 0 {
		t.Fatalf("old committed=%d repair committed=%d pending=%d", oldFile.committed, repairFile.committed, len(runtime.pending))
	}
	if runtime.persisted.Gaps[0].RepairOffset == nil || *runtime.persisted.Gaps[0].RepairOffset != 10 {
		t.Fatalf("gap=%+v", runtime.persisted.Gaps[0])
	}
}

func TestRepairGapRejectsNonContiguousAck(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	fingerprint := fileFingerprint{Device: 1, Inode: 2, PrefixHash: "hash", HashBytes: 4}
	file := &fileRuntime{fileID: "file", path: "/logs/0.log", fingerprint: fingerprint, repairGapID: "gap"}
	span := sourceSpan{FileID: file.fileID, Path: file.path, StartOffset: 1, EndOffset: 5, Device: 1, Inode: 2, PrefixHash: "hash", HashBytes: 4, RepairGapID: "gap", fingerprint: fingerprint}
	runtime := &streamRuntime{
		files:     map[string]*fileRuntime{file.path: file},
		pending:   []*pendingSpan{{span: span, file: file, complete: true}},
		revision:  1,
		persisted: state.SourceStream{StreamRef: "stream", Resource: testFrozenResourceState(), Revision: 1, HadSourceGaps: true, LossReasons: []string{"file-reclaimed"}, Gaps: []state.GapRecord{{ID: "gap", FileID: file.fileID, Path: file.path, FromOffset: 0, Device: 1, Inode: 2, PrefixHash: "hash", HashBytes: 4, Reason: "file-reclaimed"}}},
	}
	err = (&containerLogSource{state: newTestCheckpointStore(t, db)}).commitCompletedLocked(runtime)
	if err == nil || api.IsRetryableError(err) || !strings.Contains(err.Error(), "not contiguous") {
		t.Fatalf("commitCompletedLocked() error=%v retryable=%v", err, api.IsRetryableError(err))
	}
}

func TestBindRepairGapSkipsCoverageAndResumedGaps(t *testing.T) {
	resumeAt := int64(10)
	for _, test := range []struct {
		name string
		gap  state.GapRecord
	}{
		{name: "coverage", gap: state.GapRecord{ID: "coverage", FileID: "file", Coverage: true}},
		{name: "resumed", gap: state.GapRecord{ID: "resumed", FileID: "file", ResumeAt: &resumeAt}},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.gap.Device = 1
			test.gap.Inode = 2
			test.gap.PrefixHash = "hash"
			test.gap.HashBytes = 4
			file := &fileRuntime{fileID: "file", fingerprint: fileFingerprint{Device: 1, Inode: 2, PrefixHash: "hash", HashBytes: 4}}
			if err := bindRepairGap(state.SourceStream{Gaps: []state.GapRecord{test.gap}}, file); err != nil {
				t.Fatal(err)
			}
			if file.repairGapID != "" {
				t.Fatalf("repair gap id=%q", file.repairGapID)
			}
		})
	}
}

func TestFindRepairCheckpointMatchesRecoveredFingerprint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "0.log.1")
	if err := os.WriteFile(path, []byte("recovered bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	fingerprint, err := fingerprint(path, -1)
	if err != nil {
		t.Fatal(err)
	}
	repairOffset := int64(3)
	gap := state.GapRecord{ID: "gap", FileID: "file", Path: "/old/0.log", FromOffset: 1, RepairOffset: &repairOffset, Device: fingerprint.Device, Inode: fingerprint.Inode, PrefixHash: fingerprint.PrefixHash, HashBytes: fingerprint.HashBytes, Reason: "file-reclaimed"}
	runtime := &streamRuntime{revision: 2, persisted: state.SourceStream{StreamRef: "stream", Resource: testFrozenResourceState(), Revision: 2, AcknowledgedRevision: 1, Gaps: []state.GapRecord{gap}}}
	checkpoint, gapID, found, err := (&containerLogSource{}).findRepairCheckpoint(runtime, path)
	if err != nil || !found || gapID != gap.ID || checkpoint.FileID != gap.FileID || checkpoint.Path != path || checkpoint.Offset != repairOffset {
		t.Fatalf("checkpoint=%+v gapID=%q found=%v err=%v", checkpoint, gapID, found, err)
	}
}

func TestShortRepairCandidateDoesNotCreateSecondGap(t *testing.T) {
	raw := []byte("2026-07-23T10:00:00Z stdout F recovered\n")
	path := filepath.Join(t.TempDir(), "0.log.1")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	fingerprintValue, err := fingerprint(path, -1)
	if err != nil {
		t.Fatal(err)
	}
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	gap := state.GapRecord{ID: "gap", FileID: "file", Path: "/logs/old.log", ObservedSize: int64(len(raw) + 100), Device: fingerprintValue.Device, Inode: fingerprintValue.Inode, PrefixHash: fingerprintValue.PrefixHash, HashBytes: fingerprintValue.HashBytes, Reason: "file-reclaimed"}
	persisted := state.SourceStream{StreamRef: "stream", Resource: testFrozenResourceState(), Revision: 1, HadSourceGaps: true, LossReasons: []string{gap.Reason}, Gaps: []state.GapRecord{gap}}
	if err := db.PutSourceStream(persisted); err != nil {
		t.Fatal(err)
	}
	runtime := &streamRuntime{
		resource:  testResource(api.Resource{SandboxID: "sb", Container: "sandbox"}),
		files:     make(map[string]*fileRuntime),
		assembler: newAssembler(1024, time.Second),
		revision:  1,
		outcome:   outcomeFromState(persisted),
		persisted: persisted,
	}
	source := &containerLogSource{cfg: sourceConfig{MaxLineBytes: 1024, PartialTimeout: time.Second}, state: newTestCheckpointStore(t, db), out: make(chan api.SourceEvent, 1)}
	if _, err := source.scanFile(context.Background(), api.StreamRef{ID: persisted.StreamRef}, runtime, path); err != nil {
		t.Fatal(err)
	}
	file := runtime.files[path]
	if len(runtime.persisted.Gaps) != 1 || file == nil || file.repairGapID != gap.ID {
		t.Fatalf("gaps=%+v file=%+v", runtime.persisted.Gaps, file)
	}
}

func TestObservedSizeDoesNotBecomeGapEOF(t *testing.T) {
	stream := state.SourceStream{StreamRef: "stream"}
	checkpoint := state.FileCheckpoint{StreamRef: stream.StreamRef, FileID: "file", Path: "/logs/0.log", Offset: 5, ObservedSize: 100, Device: 1, Inode: 2, PrefixHash: "hash", HashBytes: 4}
	if !appendGapToStream(&stream, checkpoint, "file-reclaimed", false) {
		t.Fatal("gap was not added")
	}
	if len(stream.Gaps) != 1 || stream.Gaps[0].ToOffset != nil {
		t.Fatalf("gap=%+v, observed size must not be treated as EOF", stream.Gaps)
	}
	if stream.Gaps[0].ObservedSize != checkpoint.ObservedSize {
		t.Fatalf("gap observed size=%d, want %d", stream.Gaps[0].ObservedSize, checkpoint.ObservedSize)
	}
}

func TestAppendGapUpgradesDuplicateToCoverage(t *testing.T) {
	stream := state.SourceStream{StreamRef: "stream"}
	checkpoint := state.FileCheckpoint{StreamRef: stream.StreamRef, FileID: "file", Path: "/logs/0.log", Offset: 5, ObservedSize: 100, Device: 1, Inode: 2, PrefixHash: "hash", HashBytes: 4}
	if !appendGapToStream(&stream, checkpoint, "file-reclaimed", false) {
		t.Fatal("repairable gap was not added")
	}
	stream.Gaps[0].Resolved = true
	recomputeOutcome(&stream)
	if stream.HadSourceGaps {
		t.Fatal("resolved setup gap remained incomplete")
	}
	if !appendGapToStream(&stream, checkpoint, "file-reclaimed", true) {
		t.Fatal("duplicate gap was not upgraded to coverage")
	}
	gap := stream.Gaps[0]
	if !gap.Coverage || gap.Resolved || !stream.HadSourceGaps || !contains(stream.LossReasons, "file-reclaimed") {
		t.Fatalf("gap=%+v outcome=%+v", gap, stream)
	}
}

func TestStableRepairDoesNotResolveBeforeObservedExtent(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	fingerprint := fileFingerprint{Device: 1, Inode: 2, PrefixHash: "hash", HashBytes: 4}
	repairOffset := int64(50)
	gap := state.GapRecord{ID: "gap", FileID: "file", Path: "/logs/0.log", FromOffset: 5, RepairOffset: &repairOffset, ObservedSize: 100, Device: 1, Inode: 2, PrefixHash: "hash", HashBytes: 4, Reason: "file-reclaimed"}
	file := &fileRuntime{fileID: gap.FileID, path: gap.Path, readOffset: 50, committed: 50, observedSize: 50, fingerprint: fingerprint, repairGapID: gap.ID}
	runtime := &streamRuntime{
		files:     map[string]*fileRuntime{file.path: file},
		revision:  2,
		persisted: state.SourceStream{StreamRef: "stream", Resource: testFrozenResourceState(), Revision: 2, AcknowledgedRevision: 1, HadSourceGaps: true, LossReasons: []string{gap.Reason}, Gaps: []state.GapRecord{gap}},
	}
	source := &containerLogSource{state: newTestCheckpointStore(t, db)}
	runtime.mu.Lock()
	err = source.resolveStableRepairGapsLocked(runtime)
	runtime.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if runtime.persisted.Gaps[0].Resolved || runtime.persisted.Gaps[0].ToOffset != nil || !runtime.persisted.HadSourceGaps {
		t.Fatalf("short repair incorrectly resolved gap: %+v", runtime.persisted.Gaps[0])
	}
	repaired := int64(100)
	runtime.persisted.Gaps[0].RepairOffset = &repaired
	file.readOffset = repaired
	file.committed = repaired
	file.observedSize = repaired
	runtime.mu.Lock()
	err = source.resolveStableRepairGapsLocked(runtime)
	runtime.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if !runtime.persisted.Gaps[0].Resolved || runtime.persisted.Gaps[0].ToOffset == nil || *runtime.persisted.Gaps[0].ToOffset != repaired || runtime.persisted.HadSourceGaps {
		t.Fatalf("complete repair did not resolve gap: %+v", runtime.persisted.Gaps[0])
	}
}

func TestSourceDoesNotCommitDropPastUnacknowledgedDelivery(t *testing.T) {
	dir := testLogDirectory(t)
	valid := "2026-07-23T10:00:00Z stdout F hello\n"
	malformed := "not-a-cri-line\n"
	path := filepath.Join(dir, "0.log")
	if err := os.WriteFile(path, []byte(valid+malformed), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	resource := testResource(api.Resource{SandboxID: "sb", ClusterName: "cluster", Namespace: "ns", PodName: "pod", PodUID: "uid", NodeName: "node", Container: "sandbox"}, dir)
	view := &fakeView{resources: []streamResource{resource}, changes: make(chan struct{}, 1)}
	log, err := logger.New(logger.Config{OutputPaths: []string{"stdout"}})
	if err != nil {
		t.Fatal(err)
	}
	source := newTestSource(sourceConfig{ReconcileInterval: time.Hour, MaxLineBytes: 1024, PartialTimeout: time.Second, EndedStateRetention: time.Hour}, view, db, log, nil)
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan api.SourceEvent, 2)
	if err := source.Start(ctx, events); err != nil {
		t.Fatal(err)
	}
	delivery := (<-events).Delivery
	if delivery == nil {
		t.Fatal("expected delivery")
	}
	checkpoint, found, err := db.GetFileCheckpoint(delivery.StreamRef.ID, path)
	if err != nil || !found || checkpoint.Offset != 0 {
		t.Fatalf("unacknowledged cursor advanced: checkpoint=%+v found=%v err=%v", checkpoint, found, err)
	}
	if err := source.Acknowledge(context.Background(), []api.AckResult{{Token: delivery.AckToken, Disposition: api.AckDelivered, Guarantee: api.GuaranteeDurable}}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		checkpoint, found, err = db.GetFileCheckpoint(delivery.StreamRef.ID, path)
		if err != nil {
			t.Fatal(err)
		}
		stream, streamFound, streamErr := db.GetSourceStream(delivery.StreamRef.ID)
		if streamErr != nil {
			t.Fatal(streamErr)
		}
		if found && checkpoint.Offset == int64(len(valid+malformed)) && streamFound && stream.HadDrops && contains(stream.LossReasons, "malformed-cri") && len(stream.Drops) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cursor/drop outcome did not settle: checkpoint=%+v found=%v stream=%+v streamFound=%v", checkpoint, found, stream, streamFound)
		}
		time.Sleep(10 * time.Millisecond)
	}
	stopSource(t, source, cancel)
}

func TestSourceBatchesConsecutiveMalformedLines(t *testing.T) {
	dir := testLogDirectory(t)
	path := filepath.Join(dir, "0.log")
	raw := []byte(strings.Repeat("not-a-cri-line\n", 100))
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	checkpoints := &countingCheckpointStore{checkpointStore: newTestCheckpointStore(t, db)}
	resource := testResource(api.Resource{SandboxID: "sb", ClusterName: "cluster", Namespace: "ns", PodName: "pod", PodUID: "uid", NodeName: "node", Container: "sandbox"}, dir)
	view := &fakeView{resources: []streamResource{resource}, changes: make(chan struct{}, 1)}
	log, _ := logger.New(logger.Config{OutputPaths: []string{"stdout"}})
	source := newTestSource(sourceConfig{ReconcileInterval: time.Hour, MaxLineBytes: 1024, PartialTimeout: time.Second, EndedStateRetention: time.Hour}, view, checkpoints, log, nil)
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Close()
	if err := source.scanAll(context.Background(), watcher); err != nil {
		t.Fatal(err)
	}
	if checkpoints.commitSourceCalls != 2 {
		t.Fatalf("CommitSource calls=%d, want initial checkpoint plus one malformed span commit", checkpoints.commitSourceCalls)
	}
	ref := streamRef(resource)
	checkpoint, found, err := db.GetFileCheckpoint(ref.ID, path)
	if err != nil || !found || checkpoint.Offset != int64(len(raw)) {
		t.Fatalf("checkpoint=%+v found=%v err=%v", checkpoint, found, err)
	}
	stream, found, err := db.GetSourceStream(ref.ID)
	if err != nil || !found || len(stream.Drops) != 1 || stream.Drops[0].FromOffset != 0 || stream.Drops[0].ToOffset != int64(len(raw)) {
		t.Fatalf("stream=%+v found=%v err=%v", stream, found, err)
	}
}

func TestSourceRetriesMalformedCommitBeforeValidLine(t *testing.T) {
	dir := testLogDirectory(t)
	path := filepath.Join(dir, "0.log")
	malformed := "not-a-cri-line\n"
	valid := "2026-07-23T10:00:00Z stdout F hello\n"
	if err := os.WriteFile(path, []byte(malformed+valid), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	checkpoints := &countingCheckpointStore{checkpointStore: newTestCheckpointStore(t, db), failCommitCall: 2}
	resource := testResource(api.Resource{SandboxID: "sb", ClusterName: "cluster", Namespace: "ns", PodName: "pod", PodUID: "uid", NodeName: "node", Container: "sandbox"}, dir)
	view := &fakeView{resources: []streamResource{resource}, changes: make(chan struct{}, 1)}
	log, _ := logger.New(logger.Config{OutputPaths: []string{"stdout"}})
	source := newTestSource(sourceConfig{ReconcileInterval: time.Hour, MaxLineBytes: 1024, PartialTimeout: time.Second, EndedStateRetention: time.Hour}, view, checkpoints, log, nil)
	events := make(chan api.SourceEvent, 1)
	source.out = events
	ref := streamRef(resource)
	runtime, err := source.getOrCreateRuntime(ref, resource)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.scanFile(context.Background(), ref, runtime, path); err == nil {
		t.Fatal("first scan unexpectedly committed malformed prefix")
	}
	if got := runtime.files[path].readOffset; got != int64(len(malformed)) {
		t.Fatalf("read offset=%d, want retry offset=%d", got, len(malformed))
	}
	if _, err := source.scanFile(context.Background(), ref, runtime, path); err != nil {
		t.Fatal(err)
	}
	event := <-events
	if event.Delivery == nil || string(event.Delivery.Record.Body) != "hello" {
		t.Fatalf("delivery=%+v", event.Delivery)
	}
	if err := source.Acknowledge(context.Background(), []api.AckResult{{Token: event.Delivery.AckToken, Disposition: api.AckDelivered, Guarantee: api.GuaranteeDurable}}); err != nil {
		t.Fatal(err)
	}
	checkpoint, found, err := db.GetFileCheckpoint(ref.ID, path)
	if err != nil || !found || checkpoint.Offset != int64(len(malformed+valid)) {
		t.Fatalf("checkpoint=%+v found=%v err=%v", checkpoint, found, err)
	}
}

func TestSourceRetriesMalformedCommitAtEOF(t *testing.T) {
	dir := testLogDirectory(t)
	path := filepath.Join(dir, "0.log")
	raw := []byte(strings.Repeat("not-a-cri-line\n", 10))
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	checkpoints := &countingCheckpointStore{checkpointStore: newTestCheckpointStore(t, db), failCommitCall: 2}
	resource := testResource(api.Resource{SandboxID: "sb", ClusterName: "cluster", Namespace: "ns", PodName: "pod", PodUID: "uid", NodeName: "node", Container: "sandbox"}, dir)
	view := &fakeView{resources: []streamResource{resource}, changes: make(chan struct{}, 1)}
	log, _ := logger.New(logger.Config{OutputPaths: []string{"stdout"}})
	source := newTestSource(sourceConfig{ReconcileInterval: time.Hour, MaxLineBytes: 1024, PartialTimeout: time.Second, EndedStateRetention: time.Hour}, view, checkpoints, log, nil)
	ref := streamRef(resource)
	runtime, err := source.getOrCreateRuntime(ref, resource)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.scanFile(context.Background(), ref, runtime, path); err == nil {
		t.Fatal("first scan unexpectedly committed malformed EOF span")
	}
	if _, err := source.scanFile(context.Background(), ref, runtime, path); err != nil {
		t.Fatal(err)
	}
	checkpoint, found, err := db.GetFileCheckpoint(ref.ID, path)
	if err != nil || !found || checkpoint.Offset != int64(len(raw)) {
		t.Fatalf("checkpoint=%+v found=%v err=%v", checkpoint, found, err)
	}
}

func TestCompletedDropsDoNotMergeAcrossFileNames(t *testing.T) {
	file := &fileRuntime{fingerprint: fileFingerprint{Device: 1, Inode: 2, PrefixHash: "hash", HashBytes: 4}}
	runtime := &streamRuntime{}
	first := sourceSpan{FileID: "file", Path: "/logs/0.log", EndOffset: 10, fingerprint: file.fingerprint}
	appendCompletedDrop(runtime, file, first, "malformed-cri")
	second := sourceSpan{FileID: "file", Path: "/logs/0.log.20260723", StartOffset: 10, EndOffset: 20, fingerprint: file.fingerprint}
	appendCompletedDrop(runtime, file, second, "malformed-cri")
	if len(runtime.pending) != 2 {
		t.Fatalf("pending drops=%d, want separate records for different paths", len(runtime.pending))
	}
}

func TestSourceCommitsPartialAcrossRotatedFiles(t *testing.T) {
	dir := testLogDirectory(t)
	rotated := filepath.Join(dir, "0.log.20260723")
	base := filepath.Join(dir, "0.log")
	if err := os.WriteFile(rotated, []byte("2026-07-23T10:00:00Z stdout P hello \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(base, []byte("2026-07-23T10:00:01Z stdout F world\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	resource := testResource(api.Resource{SandboxID: "sb", ClusterName: "cluster", Namespace: "ns", PodName: "pod", PodUID: "uid", NodeName: "node", Container: "sandbox"}, dir)
	view := &fakeView{resources: []streamResource{resource}, changes: make(chan struct{}, 1)}
	log, _ := logger.New(logger.Config{OutputPaths: []string{"stdout"}})
	source := newTestSource(sourceConfig{ReconcileInterval: time.Hour, MaxLineBytes: 1024, PartialTimeout: time.Second, EndedStateRetention: time.Hour}, view, db, log, nil)
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan api.SourceEvent, 2)
	if err := source.Start(ctx, events); err != nil {
		t.Fatal(err)
	}
	delivery := (<-events).Delivery
	if delivery == nil || string(delivery.Record.Body) != "hello world" {
		t.Fatalf("delivery=%+v", delivery)
	}
	if err := source.Acknowledge(context.Background(), []api.AckResult{{Token: delivery.AckToken, Disposition: api.AckDelivered, Guarantee: api.GuaranteeDurable}}); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		path string
		size int64
	}{{rotated, fileSize(t, rotated)}, {base, fileSize(t, base)}} {
		checkpoint, found, err := db.GetFileCheckpoint(delivery.StreamRef.ID, item.path)
		if err != nil || !found || checkpoint.Offset != item.size {
			t.Fatalf("checkpoint %s=%+v found=%v err=%v", item.path, checkpoint, found, err)
		}
	}
	stopSource(t, source, cancel)
}

func TestSourceDoesNotAssemblePartialAcrossRestarts(t *testing.T) {
	dir := testLogDirectory(t)
	if err := os.WriteFile(filepath.Join(dir, "0.log"), []byte("2026-07-23T10:00:00Z stdout P old-\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "1.log"), []byte("2026-07-23T10:00:01Z stdout F new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	resource := testResource(api.Resource{SandboxID: "sb", ClusterName: "cluster", Namespace: "ns", PodName: "pod", PodUID: "uid", NodeName: "node", Container: "sandbox"}, dir)
	view := &fakeView{resources: []streamResource{resource}, changes: make(chan struct{}, 1)}
	log, _ := logger.New(logger.Config{OutputPaths: []string{"stdout"}})
	source := newTestSource(sourceConfig{ReconcileInterval: time.Hour, MaxLineBytes: 1024, PartialTimeout: time.Hour, EndedStateRetention: time.Hour}, view, db, log, nil)
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan api.SourceEvent, 2)
	if err := source.Start(ctx, events); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-events:
		if event.Delivery == nil || string(event.Delivery.Record.Body) != "new" {
			t.Fatalf("cross-restart partial assembly: %+v", event)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("restart-one delivery timed out")
	}
	stopSource(t, source, cancel)
}

func TestLostLatePersistsGapAndClearsPendingTogether(t *testing.T) {
	dir := testLogDirectory(t)
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	resource := testResource(api.Resource{SandboxID: "sb", ClusterName: "cluster", Namespace: "ns", PodName: "pod", PodUID: "uid", NodeName: "node", Container: "sandbox"}, dir, true)
	ref := streamRef(resource)
	persisted := state.SourceStream{StreamRef: ref.ID, Resource: freezeResource(resource), Revision: 1, AcknowledgedRevision: 1, Ended: true, LatePending: true, LossReasons: []string{}}
	if err := db.PutSourceStream(persisted); err != nil {
		t.Fatal(err)
	}
	checkpoints := &countingCheckpointStore{checkpointStore: newTestCheckpointStore(t, db)}
	view := &fakeView{resources: []streamResource{resource}, changes: make(chan struct{}, 1)}
	log, _ := logger.New(logger.Config{OutputPaths: []string{"stdout"}})
	source := newTestSource(sourceConfig{ReconcileInterval: time.Hour, MaxLineBytes: 1024, PartialTimeout: time.Hour, EndedStateRetention: time.Hour}, view, checkpoints, log, nil)
	persisted.CoverageStartedAt = time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	persisted.InitialScanComplete = true
	persisted.MonitoringEpoch = source.epochID
	if err := db.PutSourceStream(persisted); err != nil {
		t.Fatal(err)
	}
	source.streams[ref.ID] = runtimeFromState(source.cfg, resource, persisted)
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Close()
	if err := source.scanAll(context.Background(), watcher); err != nil {
		t.Fatal(err)
	}
	if checkpoints.putSourceCalls != 1 {
		t.Fatalf("lost-late transition used %d persistent updates, want 1", checkpoints.putSourceCalls)
	}
	got, found, err := db.GetSourceStream(ref.ID)
	if err != nil || !found || got.Revision != 2 || got.LatePending || len(got.Gaps) != 1 || got.Gaps[0].Reason != "late-after-finalize" {
		t.Fatalf("stream=%+v found=%v err=%v", got, found, err)
	}
}

func TestLostLateBeforeEndAcknowledgementSurvivesRestart(t *testing.T) {
	dir := testLogDirectory(t)
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	resource := testResource(api.Resource{SandboxID: "sb", ClusterName: "cluster", Namespace: "ns", PodName: "pod", PodUID: "uid", NodeName: "node", Container: "sandbox"}, dir, true)
	ref := streamRef(resource)
	coverageStartedAt := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	persisted := state.SourceStream{StreamRef: ref.ID, Resource: freezeResource(resource), CoverageStartedAt: coverageStartedAt, InitialScanComplete: true, MonitoringEpoch: "previous-agent", Revision: 1, FinalizingRevision: 1, FinalizingOutcome: &state.OutcomeSnapshot{LossReasons: []string{}}, LatePending: true, LossReasons: []string{}}
	if err := db.PutSourceStream(persisted); err != nil {
		t.Fatal(err)
	}
	if err := db.PutFinalizeIntent(state.FinalizeIntent{FinalizeID: "final", TargetID: "target", StreamRef: ref.ID, Revision: 1, CoverageStartedAt: coverageStartedAt, FinalizedAt: time.Now().UTC().Truncate(time.Second), SinkDone: true}); err != nil {
		t.Fatal(err)
	}
	checkpoints := &countingCheckpointStore{checkpointStore: newTestCheckpointStore(t, db)}
	view := &fakeView{resources: []streamResource{resource}, changes: make(chan struct{}, 1)}
	log, _ := logger.New(logger.Config{OutputPaths: []string{"stdout"}})
	source := newTestSource(sourceConfig{ReconcileInterval: time.Hour, MaxLineBytes: 1024, PartialTimeout: time.Hour, EndedStateRetention: time.Hour}, view, checkpoints, log, nil)
	events := make(chan api.SourceEvent, 1)
	source.out = events
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Close()
	if err := source.restoreStreams(context.Background(), watcher); err != nil {
		t.Fatal(err)
	}
	event := <-events
	if event.End == nil || event.End.Revision != 1 || event.End.Outcome.HadSourceGaps {
		t.Fatalf("replayed end=%+v", event.End)
	}
	beforeAck, found, err := db.GetSourceStream(ref.ID)
	if err != nil || !found || !beforeAck.LatePending || len(beforeAck.Gaps) != 1 || !contains(beforeAck.LossReasons, "monitor-interrupted") {
		t.Fatalf("before ack stream=%+v found=%v err=%v", beforeAck, found, err)
	}
	if err := source.AcknowledgeEnd(context.Background(), event.End.EndToken); err != nil {
		t.Fatal(err)
	}
	if err := source.scanAll(context.Background(), watcher); err != nil {
		t.Fatal(err)
	}
	got, found, err := db.GetSourceStream(ref.ID)
	if err != nil || !found || got.Revision != 2 || got.LatePending || len(got.Gaps) != 2 || !contains(got.LossReasons, "monitor-interrupted") || !contains(got.LossReasons, "late-after-finalize") {
		t.Fatalf("stream=%+v found=%v err=%v", got, found, err)
	}
}

func TestCompressedHistoryMakesFinalOutcomeIncomplete(t *testing.T) {
	dir := testLogDirectory(t)
	if err := os.WriteFile(filepath.Join(dir, "0.log.20260723.gz"), []byte("compressed"), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	resource := testResource(api.Resource{SandboxID: "sb", ClusterName: "cluster", Namespace: "ns", PodName: "pod", PodUID: "uid", NodeName: "node", Container: "sandbox"}, dir, true)
	view := &fakeView{resources: []streamResource{resource}, changes: make(chan struct{}, 1)}
	log, _ := logger.New(logger.Config{OutputPaths: []string{"stdout"}})
	source := newTestSource(sourceConfig{MaxLineBytes: 1024, PartialTimeout: time.Second, ReconcileInterval: 10 * time.Millisecond, EndedStateRetention: time.Hour}, view, db, log, nil)
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan api.SourceEvent, 2)
	if err := source.Start(ctx, events); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-events:
		if event.End == nil || !event.End.Outcome.HadSourceGaps || !contains(event.End.Outcome.LossReasons, "preexisting-compressed-rotation") {
			t.Fatalf("end=%+v", event.End)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream end timed out")
	}
	stopSource(t, source, cancel)
}

func TestUnterminatedTailIsCoveredBeforeFinalization(t *testing.T) {
	dir := testLogDirectory(t)
	path := filepath.Join(dir, "0.log")
	tail := "2026-07-23T10:00:00Z stdout F unterminated"
	if err := os.WriteFile(path, []byte(tail), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	resource := testResource(api.Resource{SandboxID: "sb", ClusterName: "cluster", Namespace: "ns", PodName: "pod", PodUID: "uid", NodeName: "node", Container: "sandbox"}, dir, true)
	view := &fakeView{resources: []streamResource{resource}, changes: make(chan struct{}, 1)}
	log, _ := logger.New(logger.Config{OutputPaths: []string{"stdout"}})
	source := newTestSource(sourceConfig{MaxLineBytes: 1024, PartialTimeout: time.Second, ReconcileInterval: 10 * time.Millisecond, EndedStateRetention: time.Hour}, view, db, log, nil)
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan api.SourceEvent, 2)
	if err := source.Start(ctx, events); err != nil {
		t.Fatal(err)
	}
	end := waitEnd(t, events)
	if !end.Outcome.HadSourceGaps || !contains(end.Outcome.LossReasons, "unterminated-cri-tail") {
		t.Fatalf("end outcome=%+v", end.Outcome)
	}
	if err := source.AcknowledgeEnd(context.Background(), end.EndToken); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-events:
		t.Fatalf("unchanged unterminated tail reopened stream: %+v", event)
	case <-time.After(150 * time.Millisecond):
	}
	checkpoint, found, err := db.GetFileCheckpoint(end.StreamRef.ID, path)
	if err != nil || !found || checkpoint.Offset != int64(len(tail)) {
		t.Fatalf("checkpoint=%+v found=%v err=%v", checkpoint, found, err)
	}
	stream, found, err := db.GetSourceStream(end.StreamRef.ID)
	if err != nil || !found || stream.Revision != 1 || !stream.Ended || len(stream.Gaps) != 1 || stream.Gaps[0].ResumeAt == nil || *stream.Gaps[0].ResumeAt != int64(len(tail)) {
		t.Fatalf("stream=%+v found=%v err=%v", stream, found, err)
	}
	stopSource(t, source, cancel)
}

func TestUnterminatedTailDoesNotCrossUnacknowledgedPartial(t *testing.T) {
	dir := testLogDirectory(t)
	path := filepath.Join(dir, "0.log")
	partial := "2026-07-23T10:00:00Z stdout P partial\n"
	tail := "unterminated"
	if err := os.WriteFile(path, []byte(partial+tail), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	resource := testResource(api.Resource{SandboxID: "sb", ClusterName: "cluster", Namespace: "ns", PodName: "pod", PodUID: "uid", NodeName: "node", Container: "sandbox"}, dir, true)
	view := &fakeView{resources: []streamResource{resource}, changes: make(chan struct{}, 1)}
	log, _ := logger.New(logger.Config{OutputPaths: []string{"stdout"}})
	source := newTestSource(sourceConfig{MaxLineBytes: 1024, PartialTimeout: time.Hour, ReconcileInterval: 10 * time.Millisecond, EndedStateRetention: time.Hour}, view, db, log, nil)
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan api.SourceEvent, 2)
	if err := source.Start(ctx, events); err != nil {
		t.Fatal(err)
	}
	delivery := (<-events).Delivery
	if delivery == nil || !strings.Contains(string(delivery.Record.Body), "partial") {
		t.Fatalf("delivery=%+v", delivery)
	}
	checkpoint, found, err := db.GetFileCheckpoint(delivery.StreamRef.ID, path)
	if err != nil || !found || checkpoint.Offset != 0 {
		t.Fatalf("tail crossed unacknowledged partial: checkpoint=%+v found=%v err=%v", checkpoint, found, err)
	}
	if err := source.Acknowledge(context.Background(), []api.AckResult{{Token: delivery.AckToken, Disposition: api.AckDelivered, Guarantee: api.GuaranteeDurable}}); err != nil {
		t.Fatal(err)
	}
	checkpoint, found, err = db.GetFileCheckpoint(delivery.StreamRef.ID, path)
	if err != nil || !found || checkpoint.Offset != int64(len(partial+tail)) {
		t.Fatalf("tail was not covered after partial ack: checkpoint=%+v found=%v err=%v", checkpoint, found, err)
	}
	end := waitEnd(t, events)
	if !end.Outcome.HadSourceGaps || !contains(end.Outcome.LossReasons, "unterminated-cri-tail") {
		t.Fatalf("end outcome=%+v", end.Outcome)
	}
	stopSource(t, source, cancel)
}

func TestSourceWaitsForDeliveryCommitBeforeFinalizing(t *testing.T) {
	dir := testLogDirectory(t)
	path := filepath.Join(dir, "0.log")
	if err := os.WriteFile(path, []byte("2026-07-23T10:00:00Z stdout F delivered\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	resource := testResource(api.Resource{SandboxID: "sb", ClusterName: "cluster", Namespace: "ns", PodName: "pod", PodUID: "uid", NodeName: "node", Container: "sandbox"}, dir, true)
	view := &fakeView{resources: []streamResource{resource}, changes: make(chan struct{}, 1)}
	log, _ := logger.New(logger.Config{OutputPaths: []string{"stdout"}})
	source := newTestSource(sourceConfig{ReconcileInterval: time.Hour, MaxLineBytes: 1024, PartialTimeout: time.Second, EndedStateRetention: time.Hour}, view, db, log, nil)
	events := make(chan api.SourceEvent, 2)
	source.out = events
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Close()
	for range 3 {
		if err := source.scanAll(context.Background(), watcher); err != nil {
			t.Fatal(err)
		}
	}
	delivery := (<-events).Delivery
	if delivery == nil {
		t.Fatal("expected delivery")
	}
	stream, found, err := db.GetSourceStream(delivery.StreamRef.ID)
	if err != nil || !found || stream.FinalizingRevision != 0 {
		t.Fatalf("uncommitted delivery finalized stream=%+v found=%v err=%v", stream, found, err)
	}
	select {
	case event := <-events:
		t.Fatalf("end preceded delivery commit: %+v", event)
	default:
	}
	if err := source.Acknowledge(context.Background(), []api.AckResult{{Token: delivery.AckToken, Disposition: api.AckDelivered, Guarantee: api.GuaranteeDurable}}); err != nil {
		t.Fatal(err)
	}
	if err := source.scanAll(context.Background(), watcher); err != nil {
		t.Fatal(err)
	}
	if end := waitEnd(t, events); end.Revision != 1 {
		t.Fatalf("end revision=%d", end.Revision)
	}
}

func TestMissingLogDirectoryDoesNotTerminateActiveResource(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ns_pod_uid", "sandbox")
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	resource := testResource(api.Resource{SandboxID: "sb", ClusterName: "cluster", Namespace: "ns", PodName: "pod", PodUID: "uid", NodeName: "node", Container: "sandbox"}, dir)
	view := &fakeView{resources: []streamResource{resource}, changes: make(chan struct{}, 1)}
	log, _ := logger.New(logger.Config{OutputPaths: []string{"stdout"}})
	source := newTestSource(sourceConfig{MaxLineBytes: 1024, PartialTimeout: time.Second, ReconcileInterval: 10 * time.Millisecond, EndedStateRetention: time.Hour}, view, db, log, nil)
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan api.SourceEvent, 1)
	if err := source.Start(ctx, events); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-events:
		t.Fatalf("active resource with a missing log directory ended: %+v", event)
	case <-time.After(100 * time.Millisecond):
	}
	stream, found, err := db.GetSourceStream(streamRef(resource).ID)
	if err != nil || !found || stream.Resource.Terminated {
		t.Fatalf("stream=%+v found=%v err=%v", stream, found, err)
	}
	stopSource(t, source, cancel)
}

func TestSourceRestartMatchesRenamedFileWithoutReplayingIt(t *testing.T) {
	dir := testLogDirectory(t)
	base := filepath.Join(dir, "0.log")
	firstLine := "2026-07-23T10:00:00Z stdout F first\n"
	secondLine := "2026-07-23T10:00:01Z stdout F second\n"
	if err := os.WriteFile(base, []byte(firstLine), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	resource := testResource(api.Resource{SandboxID: "sb", ClusterName: "cluster", Namespace: "ns", PodName: "pod", PodUID: "uid", NodeName: "node", Container: "sandbox"}, dir)
	view := &fakeView{resources: []streamResource{resource}, changes: make(chan struct{}, 1)}
	log, _ := logger.New(logger.Config{OutputPaths: []string{"stdout"}})
	first := newTestSource(sourceConfig{ReconcileInterval: time.Hour, MaxLineBytes: 1024, PartialTimeout: time.Second, EndedStateRetention: time.Hour}, view, db, log, nil)
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan api.SourceEvent, 2)
	if err := first.Start(ctx, events); err != nil {
		t.Fatal(err)
	}
	delivery := (<-events).Delivery
	if err := first.Acknowledge(context.Background(), []api.AckResult{{Token: delivery.AckToken, Disposition: api.AckDelivered, Guarantee: api.GuaranteeDurable}}); err != nil {
		t.Fatal(err)
	}
	stopSource(t, first, cancel)

	rotated := filepath.Join(dir, "0.log.20260723")
	if err := os.Rename(base, rotated); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(base, []byte(secondLine), 0o600); err != nil {
		t.Fatal(err)
	}
	second := newTestSource(sourceConfig{ReconcileInterval: time.Hour, MaxLineBytes: 1024, PartialTimeout: time.Second, EndedStateRetention: time.Hour}, view, db, log, nil)
	ctx, cancel = context.WithCancel(context.Background())
	events = make(chan api.SourceEvent, 4)
	if err := second.Start(ctx, events); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-events:
		if event.Delivery == nil || string(event.Delivery.Record.Body) != "second" {
			t.Fatalf("renamed file replayed or new base missed: %+v", event)
		}
		delivery = event.Delivery
	case <-time.After(2 * time.Second):
		t.Fatal("new base delivery timed out")
	}
	if err := second.Acknowledge(context.Background(), []api.AckResult{{Token: delivery.AckToken, Disposition: api.AckDelivered, Guarantee: api.GuaranteeDurable}}); err != nil {
		t.Fatal(err)
	}
	stream, found, err := db.GetSourceStream(delivery.StreamRef.ID)
	if err != nil || !found || !stream.HadSourceGaps || !contains(stream.LossReasons, "monitor-interrupted") || contains(stream.LossReasons, "file-reclaimed") {
		t.Fatalf("restart coverage outcome is incorrect: stream=%+v found=%v err=%v", stream, found, err)
	}
	files, err := db.ListFileCheckpoints(delivery.StreamRef.ID)
	if err != nil || len(files) != 2 {
		t.Fatalf("checkpoints=%+v err=%v", files, err)
	}
	stopSource(t, second, cancel)
}

func TestSourceRestartMarksMissingUncommittedFileAsReclaimed(t *testing.T) {
	dir := testLogDirectory(t)
	path := filepath.Join(dir, "0.log")
	line := "2026-07-23T10:00:00Z stdout F first\n"
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	resource := testResource(api.Resource{SandboxID: "sb", ClusterName: "cluster", Namespace: "ns", PodName: "pod", PodUID: "uid", NodeName: "node", Container: "sandbox"}, dir)
	view := &fakeView{resources: []streamResource{resource}, changes: make(chan struct{}, 1)}
	log, _ := logger.New(logger.Config{OutputPaths: []string{"stdout"}})
	cfg := sourceConfig{MaxLineBytes: 1024, PartialTimeout: time.Second, ReconcileInterval: 10 * time.Millisecond, EndedStateRetention: time.Hour}

	first := newTestSource(cfg, view, db, log, nil)
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan api.SourceEvent, 2)
	if err := first.Start(ctx, events); err != nil {
		t.Fatal(err)
	}
	delivery := (<-events).Delivery
	if delivery == nil {
		t.Fatal("expected delivery")
	}
	stopSource(t, first, cancel)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	view.resources[0].Terminated = true

	second := newTestSource(cfg, view, db, log, nil)
	ctx, cancel = context.WithCancel(context.Background())
	events = make(chan api.SourceEvent, 2)
	if err := second.Start(ctx, events); err != nil {
		t.Fatal(err)
	}
	end := waitEnd(t, events)
	if !end.Outcome.HadSourceGaps || !contains(end.Outcome.LossReasons, "file-reclaimed") {
		t.Fatalf("end outcome=%+v", end.Outcome)
	}
	stream, found, err := db.GetSourceStream(delivery.StreamRef.ID)
	if err != nil || !found || !stream.HadSourceGaps || !contains(stream.LossReasons, "file-reclaimed") {
		t.Fatalf("stream=%+v found=%v err=%v", stream, found, err)
	}
	stopSource(t, second, cancel)
}

func TestSourceRestartRecordsObservedFileShrink(t *testing.T) {
	dir := testLogDirectory(t)
	path := filepath.Join(dir, "0.log")
	line := "2026-07-23T10:00:00Z stdout F " + strings.Repeat("x", 64) + "\n"
	prefix := strings.Repeat(line, 64)
	raw := prefix + strings.Repeat(line, 16)
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	fingerprintValue, err := fingerprint(path, -1)
	if err != nil {
		t.Fatal(err)
	}
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	resource := testResource(api.Resource{SandboxID: "sb", ClusterName: "cluster", Namespace: "ns", PodName: "pod", PodUID: "uid", NodeName: "node", Container: "sandbox"}, dir)
	ref := streamRef(resource)
	checkpoint := state.FileCheckpoint{
		StreamRef: ref.ID, FileID: "file", Path: path, Offset: int64(len(prefix)), ObservedSize: int64(len(raw)), Revision: 1,
		Device: fingerprintValue.Device, Inode: fingerprintValue.Inode, PrefixHash: fingerprintValue.PrefixHash, HashBytes: fingerprintValue.HashBytes,
	}
	persisted := state.SourceStream{StreamRef: ref.ID, Resource: freezeResource(resource), CoverageStartedAt: time.Now().UTC().Add(-time.Minute).Truncate(time.Second), InitialScanComplete: true, MonitoringEpoch: "previous-agent", Revision: 1}
	if err := db.CommitSource([]state.FileCheckpoint{checkpoint}, persisted); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, int64(len(prefix))); err != nil {
		t.Fatal(err)
	}

	view := &fakeView{resources: []streamResource{resource}, changes: make(chan struct{}, 1)}
	log, _ := logger.New(logger.Config{OutputPaths: []string{"stdout"}})
	source := newTestSource(sourceConfig{MaxLineBytes: 1024, PartialTimeout: time.Second, ReconcileInterval: 10 * time.Millisecond, EndedStateRetention: time.Hour}, view, db, log, nil)
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan api.SourceEvent, 32)
	if err := source.Start(ctx, events); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		stream, found, err := db.GetSourceStream(ref.ID)
		if err != nil {
			t.Fatal(err)
		}
		if found {
			for _, gap := range stream.Gaps {
				if gap.Reason == "file-reclaimed" {
					if !gap.Coverage {
						t.Fatalf("shrink gap is repairable: %+v", gap)
					}
					goto shrinkRecorded
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("shrink gap was not persisted")
		}
		time.Sleep(10 * time.Millisecond)
	}

shrinkRecorded:

	appended := strings.Repeat("2026-07-23T10:00:00Z stdout F "+strings.Repeat("y", 64)+"\n", 16)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(appended); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 16; index++ {
		select {
		case event := <-events:
			if event.Delivery == nil {
				t.Fatalf("expected delivery, got %+v", event)
			}
			if err := source.Acknowledge(context.Background(), []api.AckResult{{Token: event.Delivery.AckToken, Disposition: api.AckDelivered, Guarantee: api.GuaranteeDurable}}); err != nil {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("appended delivery timed out")
		}
	}
	stopSource(t, source, cancel)
	resource.Terminated = true
	view = &fakeView{resources: []streamResource{resource}, changes: make(chan struct{}, 1)}
	source = newTestSource(sourceConfig{MaxLineBytes: 1024, PartialTimeout: time.Second, ReconcileInterval: 10 * time.Millisecond, EndedStateRetention: time.Hour}, view, db, log, nil)
	ctx, cancel = context.WithCancel(context.Background())
	events = make(chan api.SourceEvent, 2)
	if err := source.Start(ctx, events); err != nil {
		t.Fatal(err)
	}
	end := waitEnd(t, events)
	if !end.Outcome.HadSourceGaps || !contains(end.Outcome.LossReasons, "file-reclaimed") {
		t.Fatalf("end outcome=%+v", end.Outcome)
	}
	stream, found, err := db.GetSourceStream(ref.ID)
	if err != nil || !found {
		t.Fatalf("stream=%+v found=%v err=%v", stream, found, err)
	}
	shrinkGapFound := false
	for _, gap := range stream.Gaps {
		if gap.Reason == "file-reclaimed" {
			shrinkGapFound = gap.ObservedSize == int64(len(raw)) && gap.Coverage && !gap.Resolved
		}
	}
	if !shrinkGapFound {
		t.Fatalf("file-reclaimed gap missing from stream=%+v", stream)
	}
	stopSource(t, source, cancel)
}

func TestSourceRestartClassifiesMissingCommittedFile(t *testing.T) {
	for _, test := range []struct {
		name    string
		file    string
		wantGap bool
	}{{name: "rotated", file: "0.log.20260723"}, {name: "active-base", file: "0.log", wantGap: true}} {
		t.Run(test.name, func(t *testing.T) {
			dir := testLogDirectory(t)
			path := filepath.Join(dir, test.file)
			line := "2026-07-23T10:00:00Z stdout F first\n"
			if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
				t.Fatal(err)
			}
			db, err := state.Open(t.TempDir(), "target", 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			resource := testResource(api.Resource{SandboxID: "sb", ClusterName: "cluster", Namespace: "ns", PodName: "pod", PodUID: "uid", NodeName: "node", Container: "sandbox"}, dir)
			view := &fakeView{resources: []streamResource{resource}, changes: make(chan struct{}, 1)}
			log, _ := logger.New(logger.Config{OutputPaths: []string{"stdout"}})
			cfg := sourceConfig{MaxLineBytes: 1024, PartialTimeout: time.Second, ReconcileInterval: 10 * time.Millisecond, EndedStateRetention: time.Hour}

			first := newTestSource(cfg, view, db, log, nil)
			ctx, cancel := context.WithCancel(context.Background())
			events := make(chan api.SourceEvent, 2)
			if err := first.Start(ctx, events); err != nil {
				t.Fatal(err)
			}
			delivery := (<-events).Delivery
			if delivery == nil {
				t.Fatal("expected delivery")
			}
			if err := first.Acknowledge(context.Background(), []api.AckResult{{Token: delivery.AckToken, Disposition: api.AckDelivered, Guarantee: api.GuaranteeDurable}}); err != nil {
				t.Fatal(err)
			}
			stopSource(t, first, cancel)
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			view.resources[0].Terminated = true

			second := newTestSource(cfg, view, db, log, nil)
			ctx, cancel = context.WithCancel(context.Background())
			events = make(chan api.SourceEvent, 2)
			if err := second.Start(ctx, events); err != nil {
				t.Fatal(err)
			}
			end := waitEnd(t, events)
			if !end.Outcome.HadSourceGaps || !contains(end.Outcome.LossReasons, "monitor-interrupted") || contains(end.Outcome.LossReasons, "file-reclaimed") != test.wantGap {
				t.Fatalf("end outcome=%+v", end.Outcome)
			}
			stream, found, err := db.GetSourceStream(delivery.StreamRef.ID)
			if err != nil || !found || !stream.HadSourceGaps || !contains(stream.LossReasons, "monitor-interrupted") || contains(stream.LossReasons, "file-reclaimed") != test.wantGap {
				t.Fatalf("stream=%+v found=%v err=%v", stream, found, err)
			}
			stopSource(t, second, cancel)
		})
	}
}

func TestCompressedRotationWaitsForPendingDelivery(t *testing.T) {
	dir := testLogDirectory(t)
	base := filepath.Join(dir, "0.log.20260723")
	compressed := base + ".gz"
	if err := os.WriteFile(base, []byte("2026-07-23T10:00:00Z stdout F pending\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	resource := testResource(api.Resource{SandboxID: "sb", ClusterName: "cluster", Namespace: "ns", PodName: "pod", PodUID: "uid", NodeName: "node", Container: "sandbox"}, dir)
	view := &fakeView{resources: []streamResource{resource}, changes: make(chan struct{}, 1)}
	log, _ := logger.New(logger.Config{OutputPaths: []string{"stdout"}})
	source := newTestSource(sourceConfig{MaxLineBytes: 1024, PartialTimeout: time.Second, ReconcileInterval: 10 * time.Millisecond, EndedStateRetention: time.Hour}, view, db, log, nil)
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan api.SourceEvent, 2)
	if err := source.Start(ctx, events); err != nil {
		t.Fatal(err)
	}
	delivery := (<-events).Delivery
	if delivery == nil {
		t.Fatal("expected delivery")
	}
	if err := os.Rename(base, compressed); err != nil {
		t.Fatal(err)
	}
	view.changes <- struct{}{}
	time.Sleep(100 * time.Millisecond)
	assertNoSourceGap(t, db, delivery.StreamRef.ID)
	if err := source.Acknowledge(context.Background(), []api.AckResult{{Token: delivery.AckToken, Disposition: api.AckDelivered, Guarantee: api.GuaranteeDurable}}); err != nil {
		t.Fatal(err)
	}
	view.changes <- struct{}{}
	time.Sleep(100 * time.Millisecond)
	assertNoSourceGap(t, db, delivery.StreamRef.ID)
	stopSource(t, source, cancel)
}

func TestCompressedRotationAfterAcknowledgementDoesNotCreateGap(t *testing.T) {
	dir := testLogDirectory(t)
	base := filepath.Join(dir, "0.log.20260723")
	compressed := base + ".gz"
	if err := os.WriteFile(base, []byte("2026-07-23T10:00:00Z stdout F delivered\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	resource := testResource(api.Resource{SandboxID: "sb", ClusterName: "cluster", Namespace: "ns", PodName: "pod", PodUID: "uid", NodeName: "node", Container: "sandbox"}, dir)
	view := &fakeView{resources: []streamResource{resource}, changes: make(chan struct{}, 1)}
	log, _ := logger.New(logger.Config{OutputPaths: []string{"stdout"}})
	source := newTestSource(sourceConfig{MaxLineBytes: 1024, PartialTimeout: time.Second, ReconcileInterval: 10 * time.Millisecond, EndedStateRetention: time.Hour}, view, db, log, nil)
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan api.SourceEvent, 2)
	if err := source.Start(ctx, events); err != nil {
		t.Fatal(err)
	}
	delivery := (<-events).Delivery
	if delivery == nil {
		t.Fatal("expected delivery")
	}
	if err := source.Acknowledge(context.Background(), []api.AckResult{{Token: delivery.AckToken, Disposition: api.AckDelivered, Guarantee: api.GuaranteeDurable}}); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(base, compressed); err != nil {
		t.Fatal(err)
	}
	view.changes <- struct{}{}
	time.Sleep(100 * time.Millisecond)
	assertNoSourceGap(t, db, delivery.StreamRef.ID)
	stopSource(t, source, cancel)
}

func TestCompressedRotationWithoutObservedRenameCreatesGap(t *testing.T) {
	dir := testLogDirectory(t)
	base := filepath.Join(dir, "0.log")
	compressed := base + ".gz"
	if err := os.WriteFile(base, []byte("2026-07-23T10:00:00Z stdout F delivered\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	resource := testResource(api.Resource{SandboxID: "sb", ClusterName: "cluster", Namespace: "ns", PodName: "pod", PodUID: "uid", NodeName: "node", Container: "sandbox"}, dir, true)
	view := &fakeView{resources: []streamResource{resource}, changes: make(chan struct{}, 1)}
	log, _ := logger.New(logger.Config{OutputPaths: []string{"stdout"}})
	source := newTestSource(sourceConfig{MaxLineBytes: 1024, PartialTimeout: time.Second, ReconcileInterval: 10 * time.Millisecond, EndedStateRetention: time.Hour}, view, db, log, nil)
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan api.SourceEvent, 3)
	if err := source.Start(ctx, events); err != nil {
		t.Fatal(err)
	}
	defer stopSource(t, source, cancel)
	delivery := (<-events).Delivery
	if delivery == nil {
		t.Fatal("expected delivery")
	}
	if err := source.Acknowledge(context.Background(), []api.AckResult{{Token: delivery.AckToken, Disposition: api.AckDelivered, Guarantee: api.GuaranteeDurable}}); err != nil {
		t.Fatal(err)
	}
	firstEnd := waitEnd(t, events)
	if err := source.AcknowledgeEnd(context.Background(), firstEnd.EndToken); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(base, compressed); err != nil {
		t.Fatal(err)
	}
	view.changes <- struct{}{}
	secondEnd := waitEnd(t, events)
	if secondEnd.Revision != 2 || !secondEnd.Outcome.HadSourceGaps || !contains(secondEnd.Outcome.LossReasons, "compressed-rotation") {
		t.Fatalf("reopened end=%+v", secondEnd)
	}
}

func TestMissingFileFromStaleSnapshotDefersGapDecision(t *testing.T) {
	for _, test := range []struct {
		name    string
		rotate  bool
		wantGap bool
	}{{name: "covered compressed rotation", rotate: true}, {name: "reclaimed file", wantGap: true}} {
		t.Run(test.name, func(t *testing.T) {
			dir := testLogDirectory(t)
			name := "0.log"
			if test.rotate {
				name = "0.log.20260723"
			}
			path := filepath.Join(dir, name)
			compressed := path + ".gz"
			if err := os.WriteFile(path, []byte("2026-07-23T10:00:00Z stdout F delivered\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			db, err := state.Open(t.TempDir(), "target", 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			resource := testResource(api.Resource{SandboxID: "sb", ClusterName: "cluster", Namespace: "ns", PodName: "pod", PodUID: "uid", NodeName: "node", Container: "sandbox"}, dir)
			view := &fakeView{resources: []streamResource{resource}, changes: make(chan struct{}, 1)}
			log, _ := logger.New(logger.Config{OutputPaths: []string{"stdout"}})
			source := newTestSource(sourceConfig{ReconcileInterval: time.Hour, MaxLineBytes: 1024, PartialTimeout: time.Second, EndedStateRetention: time.Hour}, view, db, log, nil)
			events := make(chan api.SourceEvent, 1)
			source.out = events
			ref := streamRef(resource)
			runtime, err := source.getOrCreateRuntime(ref, resource)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := source.scanFile(context.Background(), ref, runtime, path); err != nil {
				t.Fatal(err)
			}
			delivery := (<-events).Delivery
			if delivery == nil {
				t.Fatal("expected delivery")
			}
			if err := source.Acknowledge(context.Background(), []api.AckResult{{Token: delivery.AckToken, Disposition: api.AckDelivered, Guarantee: api.GuaranteeDurable}}); err != nil {
				t.Fatal(err)
			}
			if test.rotate {
				if err := os.Rename(path, compressed); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			changed, err := source.scanFile(context.Background(), ref, runtime, path)
			if err != nil || !changed {
				t.Fatalf("stale scan changed=%v err=%v", changed, err)
			}
			assertNoSourceGap(t, db, ref.ID)
			if test.rotate {
				if err := source.recordCompressed(runtime, []string{compressed}); err != nil {
					t.Fatal(err)
				}
			}
			if err := source.reconcileMissingFiles(runtime, nil); err != nil {
				t.Fatal(err)
			}
			stream, found, err := db.GetSourceStream(ref.ID)
			if err != nil || !found || stream.HadSourceGaps != test.wantGap {
				t.Fatalf("stream=%+v found=%v err=%v", stream, found, err)
			}
		})
	}
}

func TestNewFileMissingFromStaleSnapshotMarksScanChanged(t *testing.T) {
	dir := testLogDirectory(t)
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	resource := testResource(api.Resource{SandboxID: "sb", ClusterName: "cluster", Namespace: "ns", PodName: "pod", PodUID: "uid", NodeName: "node", Container: "sandbox"}, dir, true)
	view := &fakeView{resources: []streamResource{resource}, changes: make(chan struct{}, 1)}
	log, _ := logger.New(logger.Config{OutputPaths: []string{"stdout"}})
	source := newTestSource(sourceConfig{ReconcileInterval: time.Hour, MaxLineBytes: 1024, PartialTimeout: time.Second, EndedStateRetention: time.Hour}, view, db, log, nil)
	ref := streamRef(resource)
	runtime, err := source.getOrCreateRuntime(ref, resource)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := source.scanFile(context.Background(), ref, runtime, filepath.Join(dir, "0.log"))
	if err != nil || !changed {
		t.Fatalf("stale scan changed=%v err=%v", changed, err)
	}
}

func TestCoveredCompressedRotationDoesNotReopenEndedStream(t *testing.T) {
	dir := testLogDirectory(t)
	base := filepath.Join(dir, "0.log.20260723")
	compressed := base + ".gz"
	if err := os.WriteFile(base, []byte("2026-07-23T10:00:00Z stdout F delivered\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	resource := testResource(api.Resource{SandboxID: "sb", ClusterName: "cluster", Namespace: "ns", PodName: "pod", PodUID: "uid", NodeName: "node", Container: "sandbox"}, dir, true)
	view := &fakeView{resources: []streamResource{resource}, changes: make(chan struct{}, 1)}
	log, _ := logger.New(logger.Config{OutputPaths: []string{"stdout"}})
	source := newTestSource(sourceConfig{MaxLineBytes: 1024, PartialTimeout: time.Second, ReconcileInterval: 10 * time.Millisecond, EndedStateRetention: time.Hour}, view, db, log, nil)
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan api.SourceEvent, 4)
	if err := source.Start(ctx, events); err != nil {
		t.Fatal(err)
	}
	delivery := (<-events).Delivery
	if delivery == nil {
		t.Fatal("expected delivery")
	}
	if err := source.Acknowledge(context.Background(), []api.AckResult{{Token: delivery.AckToken, Disposition: api.AckDelivered, Guarantee: api.GuaranteeDurable}}); err != nil {
		t.Fatal(err)
	}
	end := waitEnd(t, events)
	if err := source.AcknowledgeEnd(context.Background(), end.EndToken); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(base); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(compressed, []byte("compressed bytes with a different fingerprint"), 0o600); err != nil {
		t.Fatal(err)
	}
	view.changes <- struct{}{}
	select {
	case event := <-events:
		t.Fatalf("covered compressed rotation reopened stream: %+v", event)
	case <-time.After(150 * time.Millisecond):
	}
	stream, found, err := db.GetSourceStream(delivery.StreamRef.ID)
	if err != nil || !found || stream.Revision != 1 || !stream.Ended {
		t.Fatalf("stream=%+v found=%v err=%v", stream, found, err)
	}
	stopSource(t, source, cancel)
}

func TestCoveredUncompressedRotationDoesNotReopenEndedStream(t *testing.T) {
	dir := testLogDirectory(t)
	base := filepath.Join(dir, "0.log")
	rotated := filepath.Join(dir, "0.log.20260723")
	if err := os.WriteFile(base, []byte("2026-07-23T10:00:00Z stdout F delivered\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	resource := testResource(api.Resource{SandboxID: "sb", ClusterName: "cluster", Namespace: "ns", PodName: "pod", PodUID: "uid", NodeName: "node", Container: "sandbox"}, dir, true)
	view := &fakeView{resources: []streamResource{resource}, changes: make(chan struct{}, 1)}
	log, _ := logger.New(logger.Config{OutputPaths: []string{"stdout"}})
	source := newTestSource(sourceConfig{MaxLineBytes: 1024, PartialTimeout: time.Second, ReconcileInterval: 10 * time.Millisecond, EndedStateRetention: time.Hour}, view, db, log, nil)
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan api.SourceEvent, 4)
	if err := source.Start(ctx, events); err != nil {
		t.Fatal(err)
	}
	delivery := (<-events).Delivery
	if delivery == nil {
		t.Fatal("expected delivery")
	}
	if err := source.Acknowledge(context.Background(), []api.AckResult{{Token: delivery.AckToken, Disposition: api.AckDelivered, Guarantee: api.GuaranteeDurable}}); err != nil {
		t.Fatal(err)
	}
	end := waitEnd(t, events)
	if err := source.AcknowledgeEnd(context.Background(), end.EndToken); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(base, rotated); err != nil {
		t.Fatal(err)
	}
	view.changes <- struct{}{}
	select {
	case event := <-events:
		t.Fatalf("covered uncompressed rotation reopened stream: %+v", event)
	case <-time.After(150 * time.Millisecond):
	}
	stream, found, err := db.GetSourceStream(delivery.StreamRef.ID)
	if err != nil || !found || stream.Revision != 1 || !stream.Ended {
		t.Fatalf("stream=%+v found=%v err=%v", stream, found, err)
	}
	stopSource(t, source, cancel)
}

func TestFileSinkPruneDropsRuntimeButKeepsDurableState(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	deadline := time.Now().Add(-time.Minute)
	resource := testResource(api.Resource{SandboxID: "sb", ClusterName: "cluster", Namespace: "ns", PodName: "pod", PodUID: "uid", NodeName: "node", Container: "sandbox"}, filepath.Join(t.TempDir(), "removed"), true)
	streamRef := streamRef(resource)
	persisted := state.SourceStream{StreamRef: streamRef.ID, Resource: freezeResource(resource), Revision: 1, AcknowledgedRevision: 1, Ended: true, RepairDeadline: &deadline}
	if err := db.PutSourceStream(persisted); err != nil {
		t.Fatal(err)
	}
	view := &fakeView{changes: make(chan struct{}, 1)}
	log, _ := logger.New(logger.Config{OutputPaths: []string{"stdout"}})
	source := newTestSource(sourceConfig{ReconcileInterval: time.Hour, MaxLineBytes: 1024, PartialTimeout: time.Second, PruneEndedState: false}, view, db, log, nil)
	runtime := runtimeFromState(source.cfg, resource, persisted)
	source.streams[streamRef.ID] = runtime
	pruned, err := source.pruneEndedRuntime(streamRef, runtime, time.Now())
	if err != nil || !pruned {
		t.Fatalf("pruned=%v err=%v", pruned, err)
	}
	if _, exists := source.streams[streamRef.ID]; exists {
		t.Fatal("ended runtime was retained")
	}
	if _, found, err := db.GetSourceStream(streamRef.ID); err != nil || !found {
		t.Fatalf("durable state found=%v err=%v", found, err)
	}
}

func TestPruneEndedStateMode(t *testing.T) {
	for _, test := range []struct {
		name string
		cfg  config.Config
		want bool
	}{
		{name: "durable-file", cfg: config.Config{Sink: config.SinkFile, FilePath: "/var/log/opensandbox"}},
		{name: "stdout", cfg: config.Config{Sink: config.SinkFile}, want: true},
		{name: "oss", cfg: config.Config{Sink: config.SinkOSS}, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := pruneEndedState(test.cfg); got != test.want {
				t.Fatalf("pruneEndedState()=%v want=%v", got, test.want)
			}
		})
	}
}

func TestStdoutFileSinkPruneDeletesDurableState(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	deadline := time.Now().Add(-time.Minute)
	resource := testResource(api.Resource{SandboxID: "sb", ClusterName: "cluster", Namespace: "ns", PodName: "pod", PodUID: "uid", NodeName: "node", Container: "sandbox"}, filepath.Join(t.TempDir(), "removed"), true)
	streamRef := streamRef(resource)
	persisted := state.SourceStream{StreamRef: streamRef.ID, Resource: freezeResource(resource), Revision: 1, AcknowledgedRevision: 1, Ended: true, RepairDeadline: &deadline}
	if err := db.PutSourceStream(persisted); err != nil {
		t.Fatal(err)
	}
	view := &fakeView{changes: make(chan struct{}, 1)}
	log, _ := logger.New(logger.Config{OutputPaths: []string{"stdout"}})
	source := newTestSource(sourceConfig{ReconcileInterval: time.Hour, MaxLineBytes: 1024, PartialTimeout: time.Second, PruneEndedState: true}, view, db, log, nil)
	runtime := runtimeFromState(source.cfg, resource, persisted)
	source.streams[streamRef.ID] = runtime
	pruned, err := source.pruneEndedRuntime(streamRef, runtime, time.Now())
	if err != nil || !pruned {
		t.Fatalf("pruned=%v err=%v", pruned, err)
	}
	if _, exists := source.streams[streamRef.ID]; exists {
		t.Fatal("ended runtime was retained")
	}
	if _, found, err := db.GetSourceStream(streamRef.ID); err != nil || found {
		t.Fatalf("durable state found=%v err=%v", found, err)
	}
}

func TestSourceDoesNotReuseCursorAfterLiveBaseReplacement(t *testing.T) {
	dir := testLogDirectory(t)
	base := filepath.Join(dir, "0.log")
	if err := os.WriteFile(base, []byte("2026-07-23T10:00:00Z stdout F first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	resource := testResource(api.Resource{SandboxID: "sb", ClusterName: "cluster", Namespace: "ns", PodName: "pod", PodUID: "uid", NodeName: "node", Container: "sandbox"}, dir)
	view := &fakeView{resources: []streamResource{resource}, changes: make(chan struct{}, 1)}
	log, _ := logger.New(logger.Config{OutputPaths: []string{"stdout"}})
	source := newTestSource(sourceConfig{MaxLineBytes: 1024, PartialTimeout: time.Second, ReconcileInterval: 10 * time.Millisecond, EndedStateRetention: time.Hour}, view, db, log, nil)
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan api.SourceEvent, 4)
	if err := source.Start(ctx, events); err != nil {
		t.Fatal(err)
	}
	first := (<-events).Delivery
	if first == nil {
		t.Fatal("expected first delivery")
	}
	if err := source.Acknowledge(context.Background(), []api.AckResult{{Token: first.AckToken, Disposition: api.AckDelivered, Guarantee: api.GuaranteeDurable}}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(base); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(base, []byte("2026-07-23T10:00:01Z stdout F replacement\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	view.changes <- struct{}{}
	select {
	case event := <-events:
		if event.Delivery == nil || string(event.Delivery.Record.Body) != "replacement" {
			t.Fatalf("replacement delivery=%+v", event)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("replacement delivery timed out")
	}
	stream, found, err := db.GetSourceStream(first.StreamRef.ID)
	if err != nil || !found || !stream.HadSourceGaps || !contains(stream.LossReasons, "fingerprint-mismatch") {
		t.Fatalf("stream=%+v found=%v err=%v", stream, found, err)
	}
	stopSource(t, source, cancel)
}

func TestEndedRuntimeDetectsSamePathReplacement(t *testing.T) {
	for _, test := range []struct {
		name        string
		replacement []byte
	}{
		{name: "shorter", replacement: []byte("short")},
		{name: "same-size", replacement: []byte("replacement-content!")},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "0.log")
			original := []byte("original-log-content")
			if test.name == "same-size" && len(test.replacement) != len(original) {
				t.Fatalf("test replacement size=%d, want %d", len(test.replacement), len(original))
			}
			if err := os.WriteFile(path, original, 0o600); err != nil {
				t.Fatal(err)
			}
			fingerprintValue, err := fingerprint(path, -1)
			if err != nil {
				t.Fatal(err)
			}
			replacementPath := filepath.Join(dir, "replacement")
			if err := os.WriteFile(replacementPath, test.replacement, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(replacementPath, path); err != nil {
				t.Fatal(err)
			}
			file := &fileRuntime{fileID: "file", path: path, readOffset: int64(len(original)), committed: int64(len(original)), observedSize: int64(len(original)), fingerprint: fingerprintValue}
			runtime := &streamRuntime{files: map[string]*fileRuntime{path: file}, persisted: state.SourceStream{StreamRef: "stream"}}
			changed, err := (&containerLogSource{}).runtimeHasNewBytes(runtime, []string{path})
			if err != nil || !changed {
				t.Fatalf("runtimeHasNewBytes()=%v err=%v, want replacement detected", changed, err)
			}
		})
	}
}

func TestSourceReopensOnlyWhenLateBytesExist(t *testing.T) {
	dir := testLogDirectory(t)
	path := filepath.Join(dir, "0.log")
	if err := os.WriteFile(path, []byte("2026-07-23T10:00:00Z stdout F first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	resource := testResource(api.Resource{SandboxID: "sb", ClusterName: "cluster", Namespace: "ns", PodName: "pod", PodUID: "uid", NodeName: "node", Container: "sandbox"}, dir, true)
	view := &fakeView{resources: []streamResource{resource}, changes: make(chan struct{}, 1)}
	log, _ := logger.New(logger.Config{OutputPaths: []string{"stdout"}})
	cfg := sourceConfig{MaxLineBytes: 1024, PartialTimeout: time.Second, ReconcileInterval: 10 * time.Millisecond, EndedStateRetention: time.Hour}
	first := newTestSource(cfg, view, db, log, nil)
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan api.SourceEvent, 4)
	if err := first.Start(ctx, events); err != nil {
		t.Fatal(err)
	}
	delivery := (<-events).Delivery
	if err := first.Acknowledge(context.Background(), []api.AckResult{{Token: delivery.AckToken, Disposition: api.AckDelivered, Guarantee: api.GuaranteeDurable}}); err != nil {
		t.Fatal(err)
	}
	end := waitEnd(t, events)
	if end.Revision != 1 {
		t.Fatalf("revision=%d", end.Revision)
	}
	if err := first.AcknowledgeEnd(context.Background(), end.EndToken); err != nil {
		t.Fatal(err)
	}
	stopSource(t, first, cancel)

	second := newTestSource(cfg, view, db, log, nil)
	ctx, cancel = context.WithCancel(context.Background())
	events = make(chan api.SourceEvent, 4)
	if err := second.Start(ctx, events); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-events:
		t.Fatalf("unchanged ended stream reopened: %+v", event)
	case <-time.After(80 * time.Millisecond):
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("2026-07-23T10:00:01Z stdout F late\n"); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	view.changes <- struct{}{}
	select {
	case event := <-events:
		if event.Delivery == nil || string(event.Delivery.Record.Body) != "late" {
			t.Fatalf("late delivery=%+v", event)
		}
		delivery = event.Delivery
	case <-time.After(30 * time.Second):
		t.Fatal("late delivery timed out")
	}
	if err := second.Acknowledge(context.Background(), []api.AckResult{{Token: delivery.AckToken, Disposition: api.AckDelivered, Guarantee: api.GuaranteeDurable}}); err != nil {
		t.Fatal(err)
	}
	end = waitEnd(t, events)
	if end.Revision != 2 {
		t.Fatalf("late revision=%d", end.Revision)
	}
	stopSource(t, second, cancel)
}

func stopSource(t *testing.T, source *containerLogSource, cancel context.CancelFunc) {
	t.Helper()
	cancel()
	ctx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer stopCancel()
	if err := source.Stop(ctx); err != nil {
		t.Fatal(err)
	}
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

func waitEnd(t *testing.T, events <-chan api.SourceEvent) *api.StreamEnd {
	t.Helper()
	select {
	case event := <-events:
		if event.End == nil {
			t.Fatalf("expected stream end, got %+v", event)
		}
		return event.End
	case <-time.After(30 * time.Second):
		t.Fatal("stream end timed out")
		return nil
	}
}

func assertNoSourceGap(t *testing.T, db *state.DB, streamRef string) {
	t.Helper()
	stream, found, err := db.GetSourceStream(streamRef)
	if err != nil || !found || stream.HadSourceGaps {
		t.Fatalf("stream=%+v found=%v err=%v", stream, found, err)
	}
}

func contains(values []string, value string) bool {
	return strings.Contains("\x00"+strings.Join(values, "\x00")+"\x00", "\x00"+value+"\x00")
}

func TestRestoreStreamsRejectsResourceStreamRefMismatch(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	resource := testResource(api.Resource{SandboxID: "sb", ClusterName: "cluster", Namespace: "ns", PodName: "pod", PodUID: "uid", NodeName: "node", Container: "sandbox"}, testLogDirectory(t))
	expected := streamRef(resource).ID
	if err := db.PutSourceStream(state.SourceStream{StreamRef: expected + "-corrupt", Resource: freezeResource(resource), Revision: 1}); err != nil {
		t.Fatal(err)
	}
	log, err := logger.New(logger.Config{OutputPaths: []string{"stdout"}})
	if err != nil {
		t.Fatal(err)
	}
	source := newTestSource(sourceConfig{ReconcileInterval: time.Hour, MaxLineBytes: 1024, PartialTimeout: time.Second}, &fakeView{resources: []streamResource{resource}, changes: make(chan struct{}, 1)}, db, log, nil)
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Close()
	err = source.restoreStreams(context.Background(), watcher)
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("restoreStreams() error=%v", err)
	}
	if len(source.streams) != 0 {
		t.Fatalf("restoreStreams() registered %d runtimes from corrupt state", len(source.streams))
	}
}

func TestRestoreStreamsRejectsZeroFingerprintPastOffsetZero(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	resource := testResource(api.Resource{SandboxID: "sb", ClusterName: "cluster", Namespace: "ns", PodName: "pod", PodUID: "uid", NodeName: "node", Container: "sandbox"}, testLogDirectory(t))
	ref := streamRef(resource).ID
	if err := db.PutSourceStream(state.SourceStream{StreamRef: ref, Resource: freezeResource(resource), Revision: 1}); err != nil {
		t.Fatal(err)
	}
	if err := commitFileCheckpointForTest(db, state.FileCheckpoint{StreamRef: ref, FileID: "file", Path: filepath.Join(resource.LogDirectory, "0.log"), Offset: 1, ObservedSize: 1, Revision: 1}); err != nil {
		t.Fatal(err)
	}
	log, err := logger.New(logger.Config{OutputPaths: []string{"stdout"}})
	if err != nil {
		t.Fatal(err)
	}
	source := newTestSource(sourceConfig{ReconcileInterval: time.Hour, MaxLineBytes: 1024, PartialTimeout: time.Second}, &fakeView{resources: []streamResource{resource}, changes: make(chan struct{}, 1)}, db, log, nil)
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Close()
	err = source.restoreStreams(context.Background(), watcher)
	if err == nil || api.IsRetryableError(err) || !strings.Contains(err.Error(), "zero-length fingerprint") {
		t.Fatalf("restoreStreams() error=%v retryable=%v", err, api.IsRetryableError(err))
	}
}

func TestSourcePromotesEmptyFileFingerprintBeforeDelivery(t *testing.T) {
	dir := testLogDirectory(t)
	path := filepath.Join(dir, "0.log")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	resource := testResource(api.Resource{SandboxID: "sb", ClusterName: "cluster", Namespace: "ns", PodName: "pod", PodUID: "uid", NodeName: "node", Container: "sandbox"}, dir)
	view := &fakeView{resources: []streamResource{resource}, changes: make(chan struct{}, 1)}
	log, err := logger.New(logger.Config{OutputPaths: []string{"stdout"}})
	if err != nil {
		t.Fatal(err)
	}
	source := newTestSource(sourceConfig{ReconcileInterval: time.Hour, MaxLineBytes: 1024, PartialTimeout: time.Second}, view, db, log, nil)
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan api.SourceEvent, 1)
	if err := source.Start(ctx, events); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cancel()
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer stopCancel()
		if err := source.Stop(stopCtx); err != nil {
			t.Errorf("Stop() error=%v", err)
		}
	}()

	ref := streamRef(resource).ID
	deadline := time.Now().Add(2 * time.Second)
	for {
		checkpoint, found, err := db.GetFileCheckpoint(ref, path)
		if err != nil {
			t.Fatal(err)
		}
		if found {
			if checkpoint.Offset != 0 || checkpoint.HashBytes != 0 {
				t.Fatalf("initial checkpoint=%+v", checkpoint)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("empty-file checkpoint was not created")
		}
		time.Sleep(10 * time.Millisecond)
	}
	raw := []byte("2026-07-23T10:00:00Z stdout F hello\n")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case view.changes <- struct{}{}:
	default:
	}
	var delivery *api.Delivery
	select {
	case event := <-events:
		delivery = event.Delivery
	case <-time.After(2 * time.Second):
		t.Fatal("delivery timed out")
	}
	if delivery == nil {
		t.Fatal("expected delivery after empty file grew")
	}
	checkpoint, found, err := db.GetFileCheckpoint(ref, path)
	if err != nil || !found || checkpoint.Offset != 0 || checkpoint.HashBytes <= 0 {
		t.Fatalf("promoted checkpoint=%+v found=%v err=%v", checkpoint, found, err)
	}
	promotedHashBytes := checkpoint.HashBytes
	if err := source.Acknowledge(context.Background(), []api.AckResult{{Token: delivery.AckToken, Disposition: api.AckDelivered, Guarantee: api.GuaranteeDurable}}); err != nil {
		t.Fatal(err)
	}
	checkpoint, found, err = db.GetFileCheckpoint(ref, path)
	if err != nil || !found || checkpoint.Offset != int64(len(raw)) || checkpoint.HashBytes != promotedHashBytes {
		t.Fatalf("committed checkpoint=%+v found=%v err=%v", checkpoint, found, err)
	}
}

func TestRecordIDUsesStableFileIdentityNotPath(t *testing.T) {
	left := sourceSpan{FileID: "file", Path: "/logs/0.log", StartOffset: 10, EndOffset: 20, Device: 1, Inode: 2, PrefixHash: "hash", HashBytes: 4}
	right := left
	right.Path = "/logs/0.log.rotated"
	if stableRecordID("stream", []sourceSpan{left}) != stableRecordID("stream", []sourceSpan{right}) {
		t.Fatal("rename changed RecordID")
	}
}

func TestGapIDIncludesPhysicalFileIdentity(t *testing.T) {
	stream := state.SourceStream{StreamRef: "stream", Revision: 1}
	first := state.FileCheckpoint{StreamRef: stream.StreamRef, FileID: "file-a", Path: "/logs/0.log", Offset: 10, Device: 1, Inode: 2, PrefixHash: "hash-a", HashBytes: 4, ObservedSize: 20}
	if !appendGapToStream(&stream, first, "file-reclaimed", false) {
		t.Fatal("first gap was not appended")
	}
	stream.Gaps[0].Resolved = true
	recomputeOutcome(&stream)
	second := first
	second.FileID = "file-b"
	second.Device = 3
	second.Inode = 4
	second.PrefixHash = "hash-b"
	if !appendGapToStream(&stream, second, "file-reclaimed", false) {
		t.Fatal("replacement file gap collided with the resolved gap")
	}
	if len(stream.Gaps) != 2 || stream.Gaps[0].ID == stream.Gaps[1].ID || !stream.HadSourceGaps {
		t.Fatalf("gaps=%+v outcome=%+v", stream.Gaps, outcomeFromState(stream))
	}
}

func TestReadPhysicalLineForScanReturnsNonEOFError(t *testing.T) {
	want := errors.New("injected read failure")
	reader := bufio.NewReader(errorReader{err: want})
	_, _, _, done, err := readPhysicalLineForScan(reader, 1024)
	if !errors.Is(err, want) || done {
		t.Fatalf("done=%v error=%v", done, err)
	}
}

func TestRuntimeHasNewBytesReturnsStatErrors(t *testing.T) {
	runtime := &streamRuntime{files: make(map[string]*fileRuntime), persisted: state.SourceStream{StreamRef: "stream", Revision: 1}}
	if _, err := (&containerLogSource{}).runtimeHasNewBytes(runtime, []string{"\x00"}); err == nil {
		t.Fatal("runtimeHasNewBytes() swallowed a non-ENOENT stat error")
	}
}

func TestResumeMonitoringPersistsInterruptedCoverage(t *testing.T) {
	for _, test := range []struct {
		name                string
		initialScanComplete bool
		wantReason          string
	}{
		{name: "active epoch", initialScanComplete: true, wantReason: "monitor-interrupted"},
		{name: "initial scan", wantReason: "adoption-scan-interrupted"},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, err := state.Open(t.TempDir(), "target", 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			resource := testResource(api.Resource{SandboxID: "sb", ClusterName: "cluster", Namespace: "ns", PodName: "pod", PodUID: "uid", NodeName: "node", Container: "sandbox"}, t.TempDir())
			boundary := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
			persisted := state.SourceStream{StreamRef: streamRef(resource).ID, Resource: freezeResource(resource), CoverageStartedAt: boundary, InitialScanComplete: test.initialScanComplete, MonitoringEpoch: "old-epoch", Revision: 1}
			if err := db.PutSourceStream(persisted); err != nil {
				t.Fatal(err)
			}
			log, _ := logger.New(logger.Config{OutputPaths: []string{"stdout"}})
			source := newTestSource(sourceConfig{ReconcileInterval: time.Hour, MaxLineBytes: 1024, PartialTimeout: time.Second}, &fakeView{changes: make(chan struct{}, 1)}, db, log, nil)
			runtime := runtimeFromState(source.cfg, resource, persisted)
			if err := source.resumeMonitoring(runtime, true, nil); err != nil {
				t.Fatal(err)
			}
			got, found, err := db.GetSourceStream(persisted.StreamRef)
			if err != nil || !found || !got.CoverageStartedAt.Equal(boundary) || got.MonitoringEpoch != source.epochID || !got.HadSourceGaps || !contains(got.LossReasons, test.wantReason) {
				t.Fatalf("stream=%+v found=%v err=%v", got, found, err)
			}
		})
	}
}

func TestResumeMonitoringRejectsProgressWithoutCoverageBoundary(t *testing.T) {
	runtime := &streamRuntime{
		resource:  testResource(api.Resource{}, t.TempDir()),
		files:     map[string]*fileRuntime{"/logs/0.log": {fileID: "file", path: "/logs/0.log"}},
		persisted: state.SourceStream{StreamRef: "stream", Revision: 1},
	}
	err := (&containerLogSource{epochID: "epoch"}).resumeMonitoring(runtime, true, nil)
	if err == nil || api.IsRetryableError(err) || !strings.Contains(err.Error(), "progress without a coverage boundary") {
		t.Fatalf("resumeMonitoring() error=%v retryable=%v", err, api.IsRetryableError(err))
	}
}

func TestCanonicalCoverageBoundaryNeverMovesBeforeWatch(t *testing.T) {
	exact := time.Date(2026, 7, 23, 9, 58, 0, 0, time.FixedZone("offset", 8*60*60))
	if got := canonicalCoverageBoundary(exact); !got.Equal(exact) || got.Location() != time.UTC {
		t.Fatalf("exact boundary=%v", got)
	}
	fractional := exact.Add(123 * time.Millisecond)
	want := exact.UTC().Add(time.Second)
	if got := canonicalCoverageBoundary(fractional); !got.Equal(want) {
		t.Fatalf("fractional boundary=%v want=%v", got, want)
	}
}

func TestMissingPodDirectoryDefersCoverageUntilLeafWatch(t *testing.T) {
	logRoot := t.TempDir()
	logDirectory := filepath.Join(logRoot, "ns_pod_uid", "sandbox")
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	resource := testResource(api.Resource{SandboxID: "sb", ClusterName: "cluster", Namespace: "ns", PodName: "pod", PodUID: "uid", NodeName: "node", Container: "sandbox"}, logDirectory, true)
	log, _ := logger.New(logger.Config{OutputPaths: []string{"stdout"}})
	source := newTestSource(sourceConfig{ReconcileInterval: time.Hour, MaxLineBytes: 1024, PartialTimeout: time.Second}, &fakeView{resources: []streamResource{resource}, changes: make(chan struct{}, 1)}, db, log, nil)
	events := make(chan api.SourceEvent, 1)
	source.out = events
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Close()
	if err := source.scanAll(context.Background(), watcher); err != nil {
		t.Fatal(err)
	}
	if err := source.scanAll(context.Background(), watcher); err != nil {
		t.Fatal(err)
	}
	if len(source.watches) != 1 {
		t.Fatalf("tracked watches=%v", source.watches)
	}
	ref := streamRef(resource).ID
	persisted, found, err := db.GetSourceStream(ref)
	if err != nil || !found {
		t.Fatalf("GetSourceStream() found=%v err=%v", found, err)
	}
	if !persisted.CoverageStartedAt.IsZero() || persisted.InitialScanComplete || persisted.MonitoringEpoch != "" {
		t.Fatalf("monitoring started before leaf watch: %+v", persisted)
	}
	if runtime := source.streams[ref]; runtime == nil || runtime.stableEOF != 0 {
		t.Fatalf("stable EOF advanced without leaf watch: runtime=%+v", runtime)
	}
	select {
	case event := <-events:
		t.Fatalf("stream ended without leaf watch: %+v", event)
	default:
	}
	if err := os.MkdirAll(logDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := source.scanAll(context.Background(), watcher); err != nil {
		t.Fatal(err)
	}
	persisted, found, err = db.GetSourceStream(ref)
	if err != nil || !found {
		t.Fatalf("GetSourceStream() found=%v err=%v", found, err)
	}
	if persisted.CoverageStartedAt.IsZero() || !persisted.InitialScanComplete || persisted.MonitoringEpoch != source.epochID {
		t.Fatalf("monitoring did not start after leaf watch: %+v", persisted)
	}
	if _, watched := source.watches[logDirectory]; !watched {
		t.Fatalf("leaf watch not tracked: %v", source.watches)
	}
}

func TestWatchEventRelevanceIsScopedToStream(t *testing.T) {
	logDirectory := filepath.Join("root", "ns_pod_uid", "sandbox")
	for _, eventName := range []string{
		filepath.Join("root"),
		filepath.Join("root", "ns_pod_uid"),
		logDirectory,
		filepath.Join(logDirectory, "0.log"),
	} {
		if !watchEventRelevantToStream(logDirectory, eventName) {
			t.Fatalf("event %q should be relevant", eventName)
		}
	}
	for _, eventName := range []string{
		"",
		filepath.Join("root", "other_pod_uid"),
		filepath.Join("root", "other_pod_uid", "sandbox", "0.log"),
		filepath.Join("root", "ns_pod_uid", "sidecar", "0.log"),
	} {
		if watchEventRelevantToStream(logDirectory, eventName) {
			t.Fatalf("event %q should be unrelated", eventName)
		}
	}
}

func TestMissingLeafAfterCoverageRecordsDiscontinuityAndReconciles(t *testing.T) {
	logRoot := t.TempDir()
	logDirectory := filepath.Join(logRoot, "ns_pod_uid", "sandbox")
	if err := os.MkdirAll(logDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	resource := testResource(api.Resource{SandboxID: "sb", ClusterName: "cluster", Namespace: "ns", PodName: "pod", PodUID: "uid", NodeName: "node", Container: "sandbox"}, logDirectory)
	log, _ := logger.New(logger.Config{OutputPaths: []string{"stdout"}})
	source := newTestSource(sourceConfig{ReconcileInterval: time.Hour, MaxLineBytes: 1024, PartialTimeout: time.Second}, &fakeView{resources: []streamResource{resource}, changes: make(chan struct{}, 1)}, db, log, nil)
	events := make(chan api.SourceEvent, 1)
	source.out = events
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Close()
	if err := source.scanAll(context.Background(), watcher); err != nil {
		t.Fatal(err)
	}
	ref := streamRef(resource).ID
	before, found, err := db.GetSourceStream(ref)
	if err != nil || !found || before.CoverageStartedAt.IsZero() || !before.InitialScanComplete {
		t.Fatalf("initial stream=%+v found=%v err=%v", before, found, err)
	}
	if err := os.RemoveAll(logDirectory); err != nil {
		t.Fatal(err)
	}
	if err := source.scanAll(context.Background(), watcher); err != nil {
		t.Fatal(err)
	}
	after, found, err := db.GetSourceStream(ref)
	if err != nil || !found {
		t.Fatalf("GetSourceStream() found=%v err=%v", found, err)
	}
	if !after.CoverageStartedAt.Equal(before.CoverageStartedAt) || !after.InitialScanComplete || !after.HadSourceGaps || !contains(after.LossReasons, "watch-discontinuity") {
		t.Fatalf("stream after missing leaf=%+v", after)
	}
	if err := os.MkdirAll(logDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logDirectory, "0.log"), []byte("2026-07-23T10:00:00Z stdout F recovered\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := source.scanAll(context.Background(), watcher); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-events:
		if event.Delivery == nil || string(event.Delivery.Record.Body) != "recovered" {
			t.Fatalf("event after leaf recovery=%+v", event)
		}
	default:
		t.Fatal("recreated leaf was not reconciled")
	}
}

func TestSharedLogRootReplacementBroadcastsDiscontinuity(t *testing.T) {
	parent := t.TempDir()
	logRoot := filepath.Join(parent, "pods")
	resources := []streamResource{
		testResource(api.Resource{SandboxID: "sb-a", ClusterName: "cluster", Namespace: "ns", PodName: "pod-a", PodUID: "uid-a", NodeName: "node", Container: "sandbox"}, filepath.Join(logRoot, "ns_pod-a_uid-a", "sandbox")),
		testResource(api.Resource{SandboxID: "sb-b", ClusterName: "cluster", Namespace: "ns", PodName: "pod-b", PodUID: "uid-b", NodeName: "node", Container: "sandbox"}, filepath.Join(logRoot, "ns_pod-b_uid-b", "sandbox")),
	}
	for _, resource := range resources {
		if err := os.MkdirAll(resource.LogDirectory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	log, _ := logger.New(logger.Config{OutputPaths: []string{"stdout"}})
	checkpoints := &countingCheckpointStore{checkpointStore: newTestCheckpointStore(t, db)}
	source := newTestSource(sourceConfig{ReconcileInterval: time.Hour, MaxLineBytes: 1024, PartialTimeout: time.Second}, &fakeView{resources: resources, changes: make(chan struct{}, 1)}, checkpoints, log, nil)
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Close()
	if err := source.scanAll(context.Background(), watcher); err != nil {
		t.Fatal(err)
	}
	oldIdentity := source.watches[logRoot]
	if err := os.Rename(logRoot, logRoot+"-old"); err != nil {
		t.Fatal(err)
	}
	for _, resource := range resources {
		if err := os.MkdirAll(resource.LogDirectory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	info, err := os.Stat(logRoot)
	if err != nil {
		t.Fatal(err)
	}
	device, inode, err := sourceFileIdentity(info)
	if err != nil {
		t.Fatal(err)
	}
	scope := fmt.Sprintf("watch-replaced:%s:%d:%d:%d:%d", logRoot, oldIdentity.device, oldIdentity.inode, device, inode)
	checkpoints.failPutSourceCall = checkpoints.putSourceCalls + 1
	if err := source.scanAll(context.Background(), watcher); err == nil {
		t.Fatal("replacement broadcast succeeded despite injected state failure")
	}
	if len(source.pendingRootWatchReplacements[logRoot]) != 1 {
		t.Fatalf("pending replacements=%v", source.pendingRootWatchReplacements)
	}
	if err := source.scanAll(context.Background(), watcher); err != nil {
		t.Fatalf("retry replacement broadcast: %v", err)
	}
	if len(source.pendingRootWatchReplacements[logRoot]) != 0 {
		t.Fatalf("pending replacements after retry=%v", source.pendingRootWatchReplacements)
	}
	for _, resource := range resources {
		ref := streamRef(resource).ID
		persisted, found, err := db.GetSourceStream(ref)
		if err != nil || !found {
			t.Fatalf("GetSourceStream(%q) found=%v err=%v", ref, found, err)
		}
		wantID := coverageGapID(ref, source.epochID+":"+scope, "watch-discontinuity")
		matches := 0
		for _, gap := range persisted.Gaps {
			if gap.ID == wantID {
				matches++
			}
		}
		if matches != 1 {
			t.Fatalf("stream %q root replacement gap matches=%d gaps=%+v", ref, matches, persisted.Gaps)
		}
	}
}

func TestRemoveResourceWatchesRetainsOnlySharedLogRoot(t *testing.T) {
	logRoot := t.TempDir()
	logDirectory := filepath.Join(logRoot, "ns_pod_uid", "sandbox")
	if err := os.MkdirAll(logDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	log, _ := logger.New(logger.Config{OutputPaths: []string{"stdout"}})
	source := newTestSource(sourceConfig{ReconcileInterval: time.Hour}, &fakeView{changes: make(chan struct{}, 1)}, nil, log, nil)
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Close()
	if _, _, err := source.ensureResourceWatches(watcher, logDirectory); err != nil {
		t.Fatal(err)
	}
	if err := source.removeResourceWatches(watcher, logDirectory); err != nil {
		t.Fatal(err)
	}
	if len(source.watches) != 1 {
		t.Fatalf("tracked watches after removal=%v", source.watches)
	}
	if watches := watcher.WatchList(); len(watches) != 1 || watches[0] != logRoot {
		t.Fatalf("watch list=%v", watches)
	}
}

func TestUnobservedCreateDeleteEventPersistsCoverageGap(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	logRoot := t.TempDir()
	resource := testResource(api.Resource{SandboxID: "sb", ClusterName: "cluster", Namespace: "ns", PodName: "pod", PodUID: "uid", NodeName: "node", Container: "sandbox"}, filepath.Join(logRoot, "ns_pod_uid", "sandbox"))
	boundary := time.Date(2026, 7, 23, 9, 58, 0, 0, time.UTC)
	log, _ := logger.New(logger.Config{OutputPaths: []string{"stdout"}})
	source := newTestSource(sourceConfig{ReconcileInterval: time.Hour}, &fakeView{changes: make(chan struct{}, 1)}, db, log, nil)
	persisted := state.SourceStream{StreamRef: streamRef(resource).ID, Resource: freezeResource(resource), CoverageStartedAt: boundary, InitialScanComplete: true, MonitoringEpoch: source.epochID, Revision: 1}
	if err := db.PutSourceStream(persisted); err != nil {
		t.Fatal(err)
	}
	runtime := runtimeFromState(source.cfg, resource, persisted)
	source.streams[persisted.StreamRef] = runtime
	event := fsnotify.Event{Name: filepath.Join(resource.LogDirectory, "0.log"), Op: fsnotify.Create | fsnotify.Remove}
	if err := source.recordUnobservedWatchEvent(event); err != nil {
		t.Fatal(err)
	}
	if err := source.recordUnobservedWatchEvent(event); err != nil {
		t.Fatal(err)
	}
	got, found, err := db.GetSourceStream(persisted.StreamRef)
	if err != nil || !found || len(got.Gaps) != 1 || got.Gaps[0].Reason != "watch-discontinuity" {
		t.Fatalf("stream=%+v found=%v err=%v", got, found, err)
	}
}

func TestCoveredCompressedWatchRemovalUsesUncompressedCheckpoint(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	logRoot := t.TempDir()
	logDirectory := filepath.Join(logRoot, "ns_pod_uid", "sandbox")
	resource := testResource(api.Resource{SandboxID: "sb", ClusterName: "cluster", Namespace: "ns", PodName: "pod", PodUID: "uid", NodeName: "node", Container: "sandbox"}, logDirectory)
	boundary := time.Date(2026, 7, 23, 9, 58, 0, 0, time.UTC)
	ref := streamRef(resource).ID
	persisted := state.SourceStream{StreamRef: ref, Resource: freezeResource(resource), CoverageStartedAt: boundary, InitialScanComplete: true, MonitoringEpoch: "epoch", Revision: 1}
	if err := db.PutSourceStream(persisted); err != nil {
		t.Fatal(err)
	}
	uncompressed := filepath.Join(logDirectory, "0.log.20260723")
	if err := commitFileCheckpointForTest(db, state.FileCheckpoint{StreamRef: ref, FileID: "file", Path: uncompressed, Offset: 42, ObservedSize: 42, Revision: 1}); err != nil {
		t.Fatal(err)
	}
	log, _ := logger.New(logger.Config{OutputPaths: []string{"stdout"}})
	source := newTestSource(sourceConfig{ReconcileInterval: time.Hour}, &fakeView{changes: make(chan struct{}, 1)}, db, log, nil)
	source.streams[ref] = runtimeFromState(source.cfg, resource, persisted)
	compressed := uncompressed + ".gz"
	for _, op := range []fsnotify.Op{fsnotify.Remove, fsnotify.Rename} {
		if err := source.recordUnobservedWatchEvent(fsnotify.Event{Name: compressed, Op: op}); err != nil {
			t.Fatal(err)
		}
	}
	got, found, err := db.GetSourceStream(ref)
	if err != nil || !found || got.HadSourceGaps || len(got.Gaps) != 0 {
		t.Fatalf("stream=%+v found=%v err=%v", got, found, err)
	}
}

func TestFinalizingOutcomeStaysFrozenAcrossWatchDiscontinuity(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	resource := testResource(api.Resource{SandboxID: "sb", ClusterName: "cluster", Namespace: "ns", PodName: "pod", PodUID: "uid", NodeName: "node", Container: "sandbox"}, t.TempDir(), true)
	boundary := time.Date(2026, 7, 23, 9, 58, 0, 0, time.UTC)
	log, _ := logger.New(logger.Config{OutputPaths: []string{"stdout"}})
	source := newTestSource(sourceConfig{ReconcileInterval: time.Hour, EndedStateRetention: time.Hour}, &fakeView{changes: make(chan struct{}, 1)}, db, log, nil)
	persisted := state.SourceStream{StreamRef: streamRef(resource).ID, Resource: freezeResource(resource), CoverageStartedAt: boundary, InitialScanComplete: true, MonitoringEpoch: source.epochID, Revision: 1}
	if err := db.PutSourceStream(persisted); err != nil {
		t.Fatal(err)
	}
	runtime := runtimeFromState(source.cfg, resource, persisted)
	source.streams[persisted.StreamRef] = runtime
	events := make(chan api.SourceEvent, 2)
	source.out = events
	ref := api.StreamRef{ID: persisted.StreamRef}
	if err := source.emitEnd(context.Background(), ref, runtime); err != nil {
		t.Fatal(err)
	}
	first := <-events
	if first.End == nil || first.End.Outcome.HadSourceGaps {
		t.Fatalf("first end=%+v", first.End)
	}
	if err := source.recordWatchDiscontinuity(runtime, "overflow"); err != nil {
		t.Fatal(err)
	}
	if err := source.emitEnd(context.Background(), ref, runtime); err != nil {
		t.Fatal(err)
	}
	replayed := <-events
	if replayed.End == nil || replayed.End.Outcome.HadSourceGaps {
		t.Fatalf("replayed end changed frozen outcome: %+v", replayed.End)
	}
	current, found, err := db.GetSourceStream(persisted.StreamRef)
	if err != nil || !found || !current.HadSourceGaps || current.FinalizingOutcome == nil || current.FinalizingOutcome.HadSourceGaps {
		t.Fatalf("current stream=%+v found=%v err=%v", current, found, err)
	}
}

type errorReader struct {
	err error
}

func (r errorReader) Read([]byte) (int, error) {
	return 0, r.err
}

func testFrozenResourceState() state.FrozenResource {
	return state.FrozenResource{
		SandboxID:    "sb",
		ClusterName:  "cluster",
		Namespace:    "ns",
		PodName:      "pod",
		PodUID:       "uid",
		NodeName:     "node",
		Container:    "sandbox",
		LogDirectory: "/var/log/pods/ns_pod_uid/sandbox",
	}
}

func commitFileCheckpointForTest(db *state.DB, checkpoint state.FileCheckpoint) error {
	stream, found, err := db.GetSourceStream(checkpoint.StreamRef)
	if err != nil {
		return err
	}
	if !found {
		stream = state.SourceStream{StreamRef: checkpoint.StreamRef, Resource: testFrozenResourceState(), Revision: 1}
	}
	return db.CommitSource([]state.FileCheckpoint{checkpoint}, stream)
}

func (v *fakeView) List() []store.Resource {
	resources := make([]store.Resource, 0, len(v.resources))
	for _, resource := range v.resources {
		resources = append(resources, store.Resource{Resource: resource.Resource, Terminated: resource.Terminated})
	}
	return resources
}
func (v *fakeView) GetByUID(uid string) (store.Resource, bool) {
	for _, item := range v.resources {
		if item.PodUID == uid {
			return store.Resource{Resource: item.Resource, Terminated: item.Terminated}, true
		}
	}
	return store.Resource{}, false
}
func (v *fakeView) Forget(uid string) {
	for index, item := range v.resources {
		if item.PodUID == uid && item.Terminated {
			v.resources = append(v.resources[:index], v.resources[index+1:]...)
			return
		}
	}
}
func (v *fakeView) Changes() <-chan struct{} { return v.changes }

func TestSourceReadsCRIAndCommitsAck(t *testing.T) {
	dir := testLogDirectory(t)
	path := filepath.Join(dir, "0.log")
	if err := os.WriteFile(path, []byte("2026-07-23T10:00:00.123456789Z stdout F hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	resource := testResource(api.Resource{SandboxID: "sb", ClusterName: "cluster", Namespace: "ns", PodName: "pod", PodUID: "uid", NodeName: "node", Container: "sandbox"}, dir)
	view := &fakeView{resources: []streamResource{resource}, changes: make(chan struct{}, 1)}
	log, err := logger.New(logger.Config{OutputPaths: []string{"stdout"}})
	if err != nil {
		t.Fatal(err)
	}
	source := newTestSource(sourceConfig{ReconcileInterval: time.Hour, MaxLineBytes: 1024, PartialTimeout: time.Second}, view, db, log, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan api.SourceEvent, 1)
	if err := source.Start(ctx, events); err != nil {
		t.Fatal(err)
	}
	var delivery *api.Delivery
	select {
	case event := <-events:
		delivery = event.Delivery
	case <-time.After(2 * time.Second):
		t.Fatal("delivery timed out")
	}
	if delivery == nil || string(delivery.Record.Body) != "hello" || delivery.Record.Attributes["stream"] != "stdout" {
		t.Fatalf("delivery=%+v", delivery)
	}
	if err := source.Acknowledge(context.Background(), []api.AckResult{{Token: delivery.AckToken, Disposition: api.AckDelivered, Guarantee: api.GuaranteeDurable}}); err != nil {
		t.Fatal(err)
	}
	checkpoint, found, err := db.GetFileCheckpoint(delivery.StreamRef.ID, path)
	if err != nil || !found || checkpoint.Offset != int64(len("2026-07-23T10:00:00.123456789Z stdout F hello\n")) {
		t.Fatalf("checkpoint=%+v found=%v err=%v", checkpoint, found, err)
	}
	cancel()
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer stopCancel()
	if err := source.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
}
