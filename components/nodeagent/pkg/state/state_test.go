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

package state

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
	berrors "go.etcd.io/bbolt/errors"
)

func putFileCheckpointForTest(db *DB, checkpoint FileCheckpoint) error {
	if err := validateFileCheckpoint(checkpoint); err != nil {
		return err
	}
	raw, err := json.Marshal(checkpoint)
	if err != nil {
		return err
	}
	return db.db.Update(func(tx *bolt.Tx) error {
		return putFileCheckpoint(tx.Bucket(bucketSource), tx.Bucket(bucketSourceFileIndex), checkpoint, raw)
	})
}

func TestCheckpointPersistsAndTargetIsBound(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir, "target-a", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	writerID := db.WriterID()
	want := FileCheckpoint{StreamRef: "stream", FileID: "file", Path: "/logs/0.log", Offset: 42, Revision: 1}
	if err := putFileCheckpointForTest(db, want); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(dir, "target-a", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if db.WriterID() != writerID {
		t.Fatalf("writer ID changed: %q != %q", db.WriterID(), writerID)
	}
	got, found, err := db.GetFileCheckpoint("stream", "/logs/0.log")
	if err != nil || !found || got.Offset != want.Offset {
		t.Fatalf("checkpoint = %+v, found=%v, err=%v", got, found, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(dir, "target-b", 1<<20); err == nil {
		t.Fatal("expected target mismatch")
	}
}

func TestCommitSourcePersistsCursorAndOutcomeTogether(t *testing.T) {
	db, err := Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stream := SourceStream{StreamRef: "stream", Resource: validFrozenResource("uid"), Revision: 1, HadDrops: true, LossReasons: []string{"malformed-cri"}, Drops: []SourceDropRecord{{ID: "drop", FileID: "file", Path: "/logs/0.log", FromOffset: 0, ToOffset: 4, Reason: "malformed-cri"}}}
	checkpoint := FileCheckpoint{StreamRef: "stream", FileID: "file", Path: "/logs/0.log", Offset: 4, Revision: 1}
	if err := db.CommitSource([]FileCheckpoint{checkpoint}, stream); err != nil {
		t.Fatal(err)
	}
	gotCheckpoint, found, err := db.GetFileCheckpoint("stream", "/logs/0.log")
	if err != nil || !found || gotCheckpoint.Offset != 4 {
		t.Fatalf("checkpoint=%+v found=%v err=%v", gotCheckpoint, found, err)
	}
	gotStream, found, err := db.GetSourceStream("stream")
	if err != nil || !found || !gotStream.HadDrops || len(gotStream.Drops) != 1 {
		t.Fatalf("stream=%+v found=%v err=%v", gotStream, found, err)
	}
	files, err := db.ListFileCheckpoints("stream")
	if err != nil || len(files) != 1 {
		t.Fatalf("files=%+v err=%v", files, err)
	}
	streams, err := db.ListSourceStreams()
	if err != nil || len(streams) != 1 {
		t.Fatalf("streams=%+v err=%v", streams, err)
	}
}

func TestOpenRejectsCorruptCheckpointHashLength(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir, "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := FileCheckpoint{StreamRef: "stream", FileID: "file", Path: "/logs/0.log", HashBytes: maxCheckpointHashBytes + 1, Revision: 1}
	raw, err := json.Marshal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketSource).Put(stateKey(checkpoint.StreamRef, checkpoint.Path), raw)
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, "target", 1<<20); err == nil || !strings.Contains(err.Error(), "hash_bytes") {
		t.Fatalf("Open() error=%v, want corrupt hash_bytes error", err)
	}
}

func TestSourceFileIndexTracksRenames(t *testing.T) {
	db, err := Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stream := SourceStream{StreamRef: "stream", Resource: validFrozenResource("uid"), Revision: 1}
	old := FileCheckpoint{StreamRef: stream.StreamRef, FileID: "file", Path: "/logs/0.log", Offset: 10, ObservedSize: 10, Revision: 1}
	if err := db.CommitSource([]FileCheckpoint{old}, stream); err != nil {
		t.Fatal(err)
	}
	latest := old
	latest.Path = "/logs/0.log.20260724"
	latest.Offset = 30
	latest.ObservedSize = 30
	latest.Revision = 2
	stream.Revision = 2
	stream.AcknowledgedRevision = 1
	if err := db.CommitSource([]FileCheckpoint{latest}, stream); err != nil {
		t.Fatal(err)
	}
	if _, found, err := db.GetFileCheckpoint(stream.StreamRef, old.Path); err != nil || found {
		t.Fatalf("stale checkpoint found=%v err=%v", found, err)
	}
	if err := db.db.View(func(tx *bolt.Tx) error {
		index := tx.Bucket(bucketSourceFileIndex)
		got := index.Get(sourceFileIndexKey(stream.StreamRef, latest.FileID))
		if !bytes.Equal(got, stateKey(stream.StreamRef, latest.Path)) {
			t.Fatalf("index value=%q", got)
		}
		count := 0
		if err := index.ForEach(func(_, _ []byte) error {
			count++
			return nil
		}); err != nil {
			return err
		}
		if count != 1 {
			t.Fatalf("index retained %d entries, want 1", count)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteStream(stream.StreamRef); err != nil {
		t.Fatal(err)
	}
	if err := db.db.View(func(tx *bolt.Tx) error {
		if got := tx.Bucket(bucketSourceFileIndex).Get(sourceFileIndexKey(stream.StreamRef, latest.FileID)); got != nil {
			t.Fatalf("deleted stream retained index value %q", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestOpenRejectsNonCanonicalSourceCheckpointKey(t *testing.T) {
	for _, test := range []struct {
		name   string
		fileID string
		want   string
	}{
		{name: "indexed checkpoint", fileID: "file", want: "non-canonical key"},
		{name: "checkpoint without file ID", want: "file_id"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			db, err := Open(dir, "target", 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			checkpoint := FileCheckpoint{StreamRef: "stream", FileID: test.fileID, Path: "/logs/0.log", Revision: 1}
			raw, err := json.Marshal(checkpoint)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.db.Update(func(tx *bolt.Tx) error {
				return tx.Bucket(bucketSource).Put(stateKey(checkpoint.StreamRef, "/wrong/path"), raw)
			}); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(dir, "target", 1<<20); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Open() error=%v", err)
			}
		})
	}
}

func TestOpenRejectsSourceFileIndexDrift(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*bolt.Tx, FileCheckpoint) error
		want   string
	}{
		{
			name: "misdirected entry",
			mutate: func(tx *bolt.Tx, checkpoint FileCheckpoint) error {
				return tx.Bucket(bucketSourceFileIndex).Put(sourceFileIndexKey(checkpoint.StreamRef, checkpoint.FileID), []byte("missing"))
			},
			want: "does not match checkpoint",
		},
		{
			name: "orphan entry",
			mutate: func(tx *bolt.Tx, _ FileCheckpoint) error {
				return tx.Bucket(bucketSourceFileIndex).Put(stateKey("orphan", "file"), []byte("missing"))
			},
			want: "has no checkpoint",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			db, err := Open(dir, "target", 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			checkpoint := FileCheckpoint{StreamRef: "stream", FileID: "file", Path: "/logs/0.log", Revision: 1}
			stream := SourceStream{StreamRef: checkpoint.StreamRef, Resource: validFrozenResource("uid"), Revision: 1}
			if err := db.CommitSource([]FileCheckpoint{checkpoint}, stream); err != nil {
				t.Fatal(err)
			}
			if err := db.db.Update(func(tx *bolt.Tx) error { return test.mutate(tx, checkpoint) }); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(dir, "target", 1<<20); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Open() error=%v, want %q", err, test.want)
			}
		})
	}
}

func TestFileCheckpointRejectsMisdirectedSourceFileIndex(t *testing.T) {
	db, err := Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	original := FileCheckpoint{StreamRef: "stream", FileID: "file", Path: "/logs/0.log", Offset: 10, Revision: 1}
	unrelated := FileCheckpoint{StreamRef: "other-stream", FileID: "other-file", Path: "/logs/other.log", Offset: 20, Revision: 1}
	if err := putFileCheckpointForTest(db, original); err != nil {
		t.Fatal(err)
	}
	if err := putFileCheckpointForTest(db, unrelated); err != nil {
		t.Fatal(err)
	}
	if err := db.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketSourceFileIndex).Put(
			sourceFileIndexKey(original.StreamRef, original.FileID),
			stateKey(unrelated.StreamRef, unrelated.Path),
		)
	}); err != nil {
		t.Fatal(err)
	}

	renamed := original
	renamed.Path = "/logs/0.log.1"
	renamed.Offset = 30
	renamed.Revision = 2
	if err := putFileCheckpointForTest(db, renamed); err == nil || !strings.Contains(err.Error(), "source file index") {
		t.Fatalf("file checkpoint write error=%v, want invalid source file index error", err)
	}
	if got, found, err := db.GetFileCheckpoint(unrelated.StreamRef, unrelated.Path); err != nil || !found || got != unrelated {
		t.Fatalf("unrelated checkpoint=%+v found=%v err=%v, want %+v", got, found, err, unrelated)
	}
	if got, found, err := db.GetFileCheckpoint(original.StreamRef, original.Path); err != nil || !found || got != original {
		t.Fatalf("original checkpoint=%+v found=%v err=%v, want %+v", got, found, err, original)
	}
	if _, found, err := db.GetFileCheckpoint(renamed.StreamRef, renamed.Path); err != nil || found {
		t.Fatalf("renamed checkpoint found=%v err=%v, want transaction rollback", found, err)
	}
}

func TestFileCheckpointRejectsSourceFileIndexToNonCheckpoint(t *testing.T) {
	db, err := Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	original := FileCheckpoint{StreamRef: "stream", FileID: "file", Path: "/logs/0.log", Offset: 10, Revision: 1}
	unrelated := SourceStream{StreamRef: "unrelated-stream", Resource: validFrozenResource("uid"), Revision: 1}
	if err := putFileCheckpointForTest(db, original); err != nil {
		t.Fatal(err)
	}
	if err := db.PutSourceStream(unrelated); err != nil {
		t.Fatal(err)
	}
	if err := db.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketSourceFileIndex).Put(
			sourceFileIndexKey(original.StreamRef, original.FileID),
			stateKey("stream", unrelated.StreamRef),
		)
	}); err != nil {
		t.Fatal(err)
	}

	renamed := original
	renamed.Path = "/logs/0.log.1"
	renamed.Offset = 30
	renamed.Revision = 2
	if err := putFileCheckpointForTest(db, renamed); err == nil || !strings.Contains(err.Error(), "non-checkpoint") {
		t.Fatalf("file checkpoint write error=%v, want non-checkpoint index target error", err)
	}
	if got, found, err := db.GetSourceStream(unrelated.StreamRef); err != nil || !found || got.StreamRef != unrelated.StreamRef {
		t.Fatalf("unrelated stream=%+v found=%v err=%v, want it preserved", got, found, err)
	}
	if _, found, err := db.GetFileCheckpoint(renamed.StreamRef, renamed.Path); err != nil || found {
		t.Fatalf("renamed checkpoint found=%v err=%v, want transaction rollback", found, err)
	}
}

func TestCommitSourceChoosesSameFileCheckpointRegardlessOfBatchOrder(t *testing.T) {
	older := FileCheckpoint{StreamRef: "stream", FileID: "file", Path: "/logs/0.log", Offset: 10, ObservedSize: 10, Revision: 1}
	newer := FileCheckpoint{StreamRef: "stream", FileID: "file", Path: "/logs/0.log.1", Offset: 20, ObservedSize: 20, Revision: 2}
	stream := SourceStream{StreamRef: "stream", Resource: validFrozenResource("uid"), Revision: 2, AcknowledgedRevision: 1}

	commit := func(t *testing.T, checkpoints []FileCheckpoint) FileCheckpoint {
		t.Helper()
		db, err := Open(t.TempDir(), "target", 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if err := db.CommitSource(checkpoints, stream); err != nil {
			t.Fatal(err)
		}
		got, err := db.ListFileCheckpoints(stream.StreamRef)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Fatalf("ListFileCheckpoints()=%+v, want one winner", got)
		}
		return got[0]
	}

	forward := commit(t, []FileCheckpoint{older, newer})
	reverse := commit(t, []FileCheckpoint{newer, older})
	if forward != newer || reverse != newer {
		t.Fatalf("forward=%+v reverse=%+v, want %+v", forward, reverse, newer)
	}
}

func TestFileCheckpointReportsSupersededCandidate(t *testing.T) {
	db, err := Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	newer := FileCheckpoint{StreamRef: "stream", FileID: "file", Path: "/logs/0.log.1", Offset: 20, ObservedSize: 20, Revision: 2}
	if err := putFileCheckpointForTest(db, newer); err != nil {
		t.Fatal(err)
	}
	for _, older := range []FileCheckpoint{
		{StreamRef: "stream", FileID: "file", Path: "/logs/0.log", Offset: 10, ObservedSize: 10, Revision: 1},
		{StreamRef: "stream", FileID: "file", Path: newer.Path, Offset: 10, ObservedSize: 10, Revision: 1},
	} {
		if err := putFileCheckpointForTest(db, older); !errors.Is(err, ErrFileCheckpointSuperseded) {
			t.Fatalf("file checkpoint write(%+v) error=%v, want ErrFileCheckpointSuperseded", older, err)
		}
	}
	got, err := db.ListFileCheckpoints("stream")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != newer {
		t.Fatalf("ListFileCheckpoints()=%+v, want only %+v", got, newer)
	}
}

func TestFileCheckpointAllowsSameCursorMetadataRefresh(t *testing.T) {
	db, err := Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	original := FileCheckpoint{StreamRef: "stream", FileID: "file", Path: "/logs/0.log", Offset: 10, ObservedSize: 30, ModTimeUnixNano: 30, Revision: 1}
	if err := putFileCheckpointForTest(db, original); err != nil {
		t.Fatal(err)
	}
	refreshed := original
	refreshed.ObservedSize = 20
	refreshed.ModTimeUnixNano = 20
	if err := putFileCheckpointForTest(db, refreshed); err != nil {
		t.Fatal(err)
	}
	got, found, err := db.GetFileCheckpoint(refreshed.StreamRef, refreshed.Path)
	if err != nil || !found || got != refreshed {
		t.Fatalf("checkpoint=%+v found=%v err=%v, want %+v", got, found, err, refreshed)
	}
}

func TestStateRejectsEmptyStreamIdentity(t *testing.T) {
	db, err := Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.PutSourceStream(SourceStream{}); err == nil {
		t.Fatal("PutSourceStream() accepted an empty stream_ref")
	}
	if err := db.CommitSource(nil, SourceStream{}); err == nil {
		t.Fatal("CommitSource() accepted an empty stream_ref")
	}
	if err := db.PutSinkStream("file", SinkStream{}); err == nil {
		t.Fatal("PutSinkStream() accepted an empty stream_ref")
	}
}

func TestSinkStreamRejectsInvalidAppendIntent(t *testing.T) {
	validDigest := strings.Repeat("0", sha256.Size*2)
	for _, test := range []struct {
		name   string
		stream SinkStream
	}{
		{name: "position mismatch", stream: SinkStream{StreamRef: "stream", Position: 4, AppendIntent: &AppendIntent{Position: 3, Length: 1, SHA256: validDigest}}},
		{name: "zero length", stream: SinkStream{StreamRef: "stream", AppendIntent: &AppendIntent{Length: 0, SHA256: validDigest}}},
		{name: "negative length", stream: SinkStream{StreamRef: "stream", AppendIntent: &AppendIntent{Length: -1, SHA256: validDigest}}},
		{name: "range overflow", stream: SinkStream{StreamRef: "stream", Position: 1<<63 - 1, AppendIntent: &AppendIntent{Position: 1<<63 - 1, Length: 1, SHA256: validDigest}}},
		{name: "bad digest", stream: SinkStream{StreamRef: "stream", AppendIntent: &AppendIntent{Length: 1, SHA256: "not-a-digest"}}},
		{name: "uppercase digest", stream: SinkStream{StreamRef: "stream", AppendIntent: &AppendIntent{Length: 1, SHA256: strings.Repeat("A", sha256.Size*2)}}},
		{name: "closed generation", stream: SinkStream{StreamRef: "stream", CurrentClosed: true, AppendIntent: &AppendIntent{Length: 1, SHA256: validDigest}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, err := Open(t.TempDir(), "target", 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err := db.PutSinkStream("oss", test.stream); err == nil {
				t.Fatalf("PutSinkStream() accepted invalid stream %+v", test.stream)
			}
		})
	}
}

func TestGetAndOpenRejectCorruptAppendIntent(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir, "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	stream := SinkStream{SinkName: "oss", StreamRef: "stream", Position: 4, AppendIntent: &AppendIntent{Position: 3, Length: 1, SHA256: strings.Repeat("0", sha256.Size*2)}}
	raw, err := json.Marshal(stream)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketSink).Put(stateKey("oss", stream.StreamRef), raw)
	}); err != nil {
		t.Fatal(err)
	}
	if _, found, err := db.GetSinkStream("oss", stream.StreamRef); err == nil || !found {
		t.Fatalf("GetSinkStream() found=%v error=%v", found, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, "target", 1<<20); err == nil || !strings.Contains(err.Error(), "append intent") {
		t.Fatalf("Open() error=%v", err)
	}
}

func TestSourceStreamRejectsOutcomeThatCouldReportFalseComplete(t *testing.T) {
	db, err := Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stream := SourceStream{
		StreamRef: "stream",
		Resource:  validFrozenResource("uid"),
		Revision:  1,
		Gaps:      []GapRecord{{ID: "gap", Path: "/logs/0.log", Reason: "file-reclaimed"}},
	}
	if err := db.PutSourceStream(stream); err == nil || !strings.Contains(err.Error(), "had_source_gaps") {
		t.Fatalf("PutSourceStream() error=%v", err)
	}
}

func TestSourceStreamRejectsImpossibleRecoveryState(t *testing.T) {
	end := int64(10)
	for _, test := range []struct {
		name   string
		stream SourceStream
		want   string
	}{
		{name: "skipped revision", stream: SourceStream{StreamRef: "stream", Resource: validFrozenResource("uid"), Revision: 2}, want: "not contiguous"},
		{name: "missing resource", stream: SourceStream{StreamRef: "stream", Revision: 1}, want: "resource identity"},
		{name: "resolved coverage gap", stream: SourceStream{StreamRef: "stream", Resource: validFrozenResource("uid"), Revision: 1, Gaps: []GapRecord{{ID: "gap", Path: "/logs", Reason: "watch-discontinuity", Coverage: true, Resolved: true}}, LossReasons: []string{}}, want: "coverage gap"},
		{name: "resolved gap without fingerprint", stream: SourceStream{StreamRef: "stream", Resource: validFrozenResource("uid"), Revision: 1, Gaps: []GapRecord{{ID: "gap", FileID: "file", Path: "/logs/0.log", Reason: "file-reclaimed", ToOffset: &end, RepairOffset: &end, Resolved: true}}, LossReasons: []string{}}, want: "repair evidence"},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, err := Open(t.TempDir(), "target", 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err := db.PutSourceStream(test.stream); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("PutSourceStream() error=%v want substring %q", err, test.want)
			}
		})
	}
}

func TestFinalizeIntentRequiresCanonicalCoverageAndTarget(t *testing.T) {
	db, err := Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	boundary := time.Date(2026, 7, 23, 9, 58, 0, 0, time.UTC)
	valid := FinalizeIntent{FinalizeID: "finalize", TargetID: "target", StreamRef: "stream", Revision: 1, CoverageStartedAt: boundary, FinalizedAt: boundary.Add(time.Minute)}
	if err := db.PutFinalizeIntent(valid); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*FinalizeIntent){
		func(intent *FinalizeIntent) { intent.CoverageStartedAt = time.Time{} },
		func(intent *FinalizeIntent) { intent.CoverageStartedAt = intent.CoverageStartedAt.Add(time.Nanosecond) },
		func(intent *FinalizeIntent) { intent.FinalizedAt = intent.FinalizedAt.Add(time.Nanosecond) },
		func(intent *FinalizeIntent) { intent.TargetID = "other" },
	} {
		candidate := valid
		candidate.Revision++
		mutate(&candidate)
		if err := db.PutFinalizeIntent(candidate); err == nil {
			t.Fatalf("PutFinalizeIntent() accepted invalid intent %+v", candidate)
		}
	}
}

func TestOpenRejectsSinkStreamUnderNonCanonicalKey(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir, "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	stream := SinkStream{SinkName: "oss", StreamRef: "stream"}
	raw, err := json.Marshal(stream)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketSink).Put(stateKey("file", stream.StreamRef), raw)
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, "target", 1<<20); err == nil || !strings.Contains(err.Error(), "non-canonical key") {
		t.Fatalf("Open() error=%v", err)
	}
}

func TestCommitSourceRollsBackWhenCheckpointIsSuperseded(t *testing.T) {
	db, err := Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	newer := FileCheckpoint{StreamRef: "stream", FileID: "file", Path: "/logs/0.log.1", Offset: 20, ObservedSize: 20, Revision: 2}
	if err := putFileCheckpointForTest(db, newer); err != nil {
		t.Fatal(err)
	}
	older := FileCheckpoint{StreamRef: "stream", FileID: "file", Path: "/logs/0.log", Offset: 10, ObservedSize: 10, Revision: 1}
	stream := SourceStream{StreamRef: "stream", Resource: validFrozenResource("uid"), Revision: 1, HadDrops: true, LossReasons: []string{"test-drop"}, Drops: []SourceDropRecord{{ID: "drop", FileID: "file", Path: older.Path, FromOffset: 0, ToOffset: 1, Reason: "test-drop"}}}
	if err := db.CommitSource([]FileCheckpoint{older}, stream); !errors.Is(err, ErrFileCheckpointSuperseded) {
		t.Fatalf("CommitSource() error=%v, want ErrFileCheckpointSuperseded", err)
	}
	if _, found, err := db.GetSourceStream(stream.StreamRef); err != nil || found {
		t.Fatalf("GetSourceStream() found=%v err=%v, want transaction rollback", found, err)
	}
	got, err := db.ListFileCheckpoints(stream.StreamRef)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != newer {
		t.Fatalf("ListFileCheckpoints()=%+v, want only %+v", got, newer)
	}
}

func TestStateMaxSizeUsesBoltAllocator(t *testing.T) {
	dir := t.TempDir()
	const maxBytes = 1 << 20
	db, err := Open(dir, "target", maxBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	largeReason := strings.Repeat("x", 2*maxBytes)
	err = db.PutSourceStream(SourceStream{
		StreamRef:     "stream",
		Resource:      validFrozenResource("pod"),
		Revision:      1,
		HadSourceGaps: true,
		LossReasons:   []string{largeReason},
		Gaps:          []GapRecord{{ID: "gap", Path: "/logs", Reason: largeReason, Coverage: true}},
	})
	if !errors.Is(err, berrors.ErrMaxSizeReached) {
		t.Fatalf("PutSourceStream() error=%v, want ErrMaxSizeReached", err)
	}
	info, err := os.Stat(db.db.Path())
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > maxBytes {
		t.Fatalf("database size=%d exceeds limit=%d", info.Size(), maxBytes)
	}
}

func TestStateMaxSizeAllowsReuseFromOversizedExistingFile(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir, "target", 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 32; i++ {
		streamRef := fmt.Sprintf("stream-%d", i)
		largeReason := strings.Repeat("x", 32<<10)
		if err := db.PutSourceStream(SourceStream{
			StreamRef:     streamRef,
			Resource:      validFrozenResource(fmt.Sprintf("pod-%d", i)),
			Revision:      1,
			HadSourceGaps: true,
			LossReasons:   []string{largeReason},
			Gaps:          []GapRecord{{ID: "gap", Path: "/logs", Reason: largeReason, Coverage: true}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 32; i++ {
		if err := db.DeleteStream(fmt.Sprintf("stream-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	path := db.db.Path()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	limit := info.Size() - int64(os.Getpagesize())
	if limit <= 0 {
		t.Fatalf("unexpected database size %d", info.Size())
	}
	db, err = Open(dir, "target", limit)
	if err != nil {
		t.Fatalf("Open() rejected an existing file with reusable pages: %v", err)
	}
	defer db.Close()
	if err := db.PutSourceStream(SourceStream{StreamRef: "reused", Resource: validFrozenResource("pod"), Revision: 1}); err != nil {
		t.Fatalf("small write did not reuse free pages: %v", err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	largeReason := strings.Repeat("x", int(2*info.Size()))
	err = db.PutSourceStream(SourceStream{
		StreamRef:     "too-large",
		Resource:      validFrozenResource("pod"),
		Revision:      1,
		HadSourceGaps: true,
		LossReasons:   []string{largeReason},
		Gaps:          []GapRecord{{ID: "gap", Path: "/logs", Reason: largeReason, Coverage: true}},
	})
	if !errors.Is(err, berrors.ErrMaxSizeReached) {
		t.Fatalf("large write error=%v, want ErrMaxSizeReached", err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != before.Size() {
		t.Fatalf("database grew from %d to %d after ErrMaxSizeReached", before.Size(), after.Size())
	}
}

func validFrozenResource(podUID string) FrozenResource {
	return FrozenResource{
		SandboxID:    "sandbox-" + podUID,
		ClusterName:  "cluster",
		Namespace:    "namespace",
		PodName:      "pod-" + podUID,
		PodUID:       podUID,
		NodeName:     "node",
		Container:    "sandbox",
		LogDirectory: "/var/log/pods/" + podUID + "/sandbox",
	}
}
