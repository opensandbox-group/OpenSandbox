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

// Package state owns Node Agent's node-local bbolt database. It stores only
// recovery metadata; log payloads remain in kubelet files and configured Sinks.
package state

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/alibaba/opensandbox/nodeagent/pkg/api"
	"github.com/google/uuid"
	bolt "go.etcd.io/bbolt"
)

const (
	SchemaVersion = 1
	// This is the on-disk checkpoint schema limit. Source fingerprint bounds
	// must not exceed it, and lowering it would reject existing state.
	maxCheckpointHashBytes = 4096
)

var (
	bucketMeta            = []byte("meta")
	bucketSource          = []byte("source")
	bucketSourceFileIndex = []byte("source_file_index")
	bucketPipeline        = []byte("pipeline")
	bucketStreamKind      = []byte("stream_kind")
	bucketSink            = []byte("sink")
	keySchema             = []byte("schema_version")
	keyWriterID           = []byte("writer_id")
	keyTargetID           = []byte("target_id")
)

// ErrFileCheckpointSuperseded means the requested checkpoint lost to the
// preferred checkpoint for the same physical file and was not persisted.
var ErrFileCheckpointSuperseded = errors.New("source file checkpoint superseded")

type DB struct {
	db       *bolt.DB
	writerID string
	targetID string
}

type FileCheckpoint struct {
	StreamRef       string `json:"stream_ref"`
	FileID          string `json:"file_id"`
	Path            string `json:"path"`
	Offset          int64  `json:"offset"`
	Device          uint64 `json:"device,omitempty"`
	Inode           uint64 `json:"inode,omitempty"`
	PrefixHash      string `json:"prefix_hash,omitempty"`
	HashBytes       int    `json:"hash_bytes,omitempty"`
	ObservedSize    int64  `json:"observed_size,omitempty"`
	ModTimeUnixNano int64  `json:"mod_time_unix_nano,omitempty"`
	Revision        uint64 `json:"revision"`
}

type SourceDropRecord struct {
	ID         string `json:"id"`
	FileID     string `json:"file_id"`
	Path       string `json:"path"`
	FromOffset int64  `json:"from_offset"`
	ToOffset   int64  `json:"to_offset"`
	Reason     string `json:"reason"`
}

type GapRecord struct {
	ID           string `json:"id"`
	FileID       string `json:"file_id,omitempty"`
	Path         string `json:"path"`
	FromOffset   int64  `json:"from_offset,omitempty"`
	ToOffset     *int64 `json:"to_offset,omitempty"`
	ResumeAt     *int64 `json:"resume_at,omitempty"`
	RepairOffset *int64 `json:"repair_offset,omitempty"`
	ObservedSize int64  `json:"observed_size,omitempty"`
	Device       uint64 `json:"device,omitempty"`
	Inode        uint64 `json:"inode,omitempty"`
	PrefixHash   string `json:"prefix_hash,omitempty"`
	HashBytes    int    `json:"hash_bytes,omitempty"`
	Reason       string `json:"reason"`
	Coverage     bool   `json:"coverage,omitempty"`
	Resolved     bool   `json:"resolved,omitempty"`
}

type FrozenResource struct {
	SandboxID    string `json:"sandbox_id"`
	ClusterName  string `json:"k8s.cluster.name"`
	Namespace    string `json:"k8s.namespace.name"`
	PodName      string `json:"k8s.pod.name"`
	PodUID       string `json:"k8s.pod.uid"`
	NodeName     string `json:"k8s.node.name"`
	Container    string `json:"k8s.container.name"`
	LogDirectory string `json:"log_directory"`
	Terminated   bool   `json:"terminated,omitempty"`
}

type SourceStream struct {
	StreamRef            string             `json:"stream_ref"`
	Resource             FrozenResource     `json:"resource"`
	CoverageStartedAt    time.Time          `json:"coverage_started_at"`
	InitialScanComplete  bool               `json:"initial_scan_complete"`
	MonitoringEpoch      string             `json:"monitoring_epoch"`
	Guarantee            string             `json:"guarantee,omitempty"`
	Revision             uint64             `json:"revision"`
	AcknowledgedRevision uint64             `json:"acknowledged_revision,omitempty"`
	FinalizingRevision   uint64             `json:"finalizing_revision,omitempty"`
	FinalizingOutcome    *OutcomeSnapshot   `json:"finalizing_outcome,omitempty"`
	HadDrops             bool               `json:"had_drops"`
	HadSourceGaps        bool               `json:"had_source_gaps"`
	LossReasons          []string           `json:"loss_reasons"`
	Drops                []SourceDropRecord `json:"drops,omitempty"`
	Gaps                 []GapRecord        `json:"gaps,omitempty"`
	LatePending          bool               `json:"late_pending,omitempty"`
	Ended                bool               `json:"ended,omitempty"`
	RepairDeadline       *time.Time         `json:"repair_deadline,omitempty"`
}

type OutcomeSnapshot struct {
	HadDrops      bool     `json:"had_drops"`
	HadSourceGaps bool     `json:"had_source_gaps"`
	LossReasons   []string `json:"loss_reasons"`
}

type SinkStream struct {
	SinkName             string                `json:"sink_name"`
	StreamRef            string                `json:"stream_ref"`
	Generation           uint64                `json:"generation"`
	Position             int64                 `json:"position"`
	Device               uint64                `json:"device,omitempty"`
	Inode                uint64                `json:"inode,omitempty"`
	CRC64State           []byte                `json:"crc64_state,omitempty"`
	ObjectKey            string                `json:"object_key"`
	AppendIntent         *AppendIntent         `json:"append_intent,omitempty"`
	GenerationTransition *GenerationTransition `json:"generation_transition,omitempty"`
	MarkerIntent         *MarkerIntent         `json:"marker_intent,omitempty"`
	ClosedObjects        []ClosedObject        `json:"closed_objects,omitempty"`
	CurrentClosed        bool                  `json:"current_closed,omitempty"`
	FinalizedRevision    uint64                `json:"finalized_revision,omitempty"`
	CleanupPhase         string                `json:"cleanup_phase,omitempty"`
	CleanupPath          string                `json:"cleanup_path,omitempty"`
}

type AppendIntent struct {
	Position int64  `json:"position"`
	Length   int64  `json:"length"`
	SHA256   string `json:"sha256"`
	Device   uint64 `json:"device,omitempty"`
	Inode    uint64 `json:"inode,omitempty"`
}

type GenerationTransition struct {
	FromGeneration uint64 `json:"from_generation"`
	ToGeneration   uint64 `json:"to_generation"`
	ObjectKey      string `json:"object_key"`
}

type MarkerIntent struct {
	Revision uint64 `json:"revision"`
	Path     string `json:"path"`
	TempPath string `json:"temp_path"`
	SHA256   string `json:"sha256"`
}

type ClosedObject struct {
	Key        string `json:"key"`
	Generation uint64 `json:"generation"`
	Size       int64  `json:"size"`
	CRC64      string `json:"crc64"`
}

type FinalizeIntent struct {
	FinalizeID        string             `json:"finalize_id"`
	TargetID          string             `json:"target_id"`
	StreamRef         string             `json:"stream_ref"`
	StreamKind        api.RecordKind     `json:"stream_kind,omitempty"`
	Revision          uint64             `json:"revision"`
	CoverageStartedAt time.Time          `json:"coverage_started_at"`
	FinalizedAt       time.Time          `json:"finalized_at"`
	Resource          *api.Resource      `json:"resource,omitempty"`
	Metadata          api.StreamMetadata `json:"metadata,omitempty"`
	Outcome           *api.SourceOutcome `json:"outcome,omitempty"`
	EndToken          *api.EndToken      `json:"end_token,omitempty"`
	SinkDone          bool               `json:"sink_done"`
	SourceDone        bool               `json:"source_done"`
}

func Open(dir, targetID string, maxBytes int64) (*DB, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	maxSize := 0
	if maxBytes > 0 {
		maxInt := uint64(^uint(0) >> 1)
		if uint64(maxBytes) > maxInt {
			return nil, fmt.Errorf("state size limit %d exceeds platform int capacity", maxBytes)
		}
		maxSize = int(maxBytes)
	}
	path := filepath.Join(dir, "checkpoint.db")
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: time.Second, MaxSize: maxSize})
	if err != nil {
		return nil, fmt.Errorf("open checkpoint database: %w", err)
	}
	s := &DB{db: db, targetID: targetID}
	if err := s.initialize(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (d *DB) initialize() error {
	return d.db.Update(func(tx *bolt.Tx) error {
		meta, err := tx.CreateBucketIfNotExists(bucketMeta)
		if err != nil {
			return err
		}
		for _, name := range [][]byte{bucketSource, bucketSourceFileIndex, bucketPipeline, bucketStreamKind, bucketSink, bucketSourcePrivate} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		if raw := meta.Get(keySchema); raw == nil {
			if err := meta.Put(keySchema, []byte(strconv.Itoa(SchemaVersion))); err != nil {
				return err
			}
		} else if string(raw) != strconv.Itoa(SchemaVersion) {
			return fmt.Errorf("unsupported state schema %q", raw)
		}
		if raw := meta.Get(keyWriterID); raw == nil {
			d.writerID = uuid.NewString()
			if err := meta.Put(keyWriterID, []byte(d.writerID)); err != nil {
				return err
			}
		} else {
			d.writerID = string(raw)
		}
		if raw := meta.Get(keyTargetID); raw == nil {
			if err := meta.Put(keyTargetID, []byte(d.targetID)); err != nil {
				return err
			}
		} else if string(raw) != d.targetID {
			return fmt.Errorf("state target mismatch: stored %q, configured %q", raw, d.targetID)
		}
		if err := validateSourceFileIndex(tx); err != nil {
			return err
		}
		return validateStoredState(tx, d.targetID)
	})
}

func (d *DB) Close() error { return d.db.Close() }

func (d *DB) WriterID() string { return d.writerID }

func (d *DB) TargetID() string { return d.targetID }

// ValidateEnabledSources prevents a configuration change from silently
// orphaning recovery state owned by a disabled or renamed Source.
func (d *DB) ValidateEnabledSources(sources []string) error {
	if len(sources) == 0 {
		return errors.New("enabled Source set is empty")
	}
	enabled := make(map[string]struct{}, len(sources))
	for _, source := range sources {
		if err := validateSourceStateName(source); err != nil {
			return err
		}
		enabled[source] = struct{}{}
	}
	return d.db.View(func(tx *bolt.Tx) error {
		if err := tx.Bucket(bucketStreamKind).ForEach(func(key, _ []byte) error {
			streamID := string(key)
			source, localID, found := strings.Cut(streamID, "/")
			if !found || source == "" || localID == "" {
				return fmt.Errorf("persisted stream %q has no Source namespace", streamID)
			}
			if _, ok := enabled[source]; !ok {
				return fmt.Errorf("Source %q is disabled while stream %q still has recovery state", source, streamID)
			}
			return nil
		}); err != nil {
			return err
		}

		root := tx.Bucket(bucketSourcePrivate)
		return root.ForEach(func(key, value []byte) error {
			if value != nil {
				return fmt.Errorf("invalid private Source state entry %q", key)
			}
			source := string(key)
			if err := validateSourceStateName(source); err != nil {
				return fmt.Errorf("invalid private Source state owner %q: %w", source, err)
			}
			bucket := root.Bucket(key)
			if bucket == nil {
				return fmt.Errorf("private Source state bucket %q is missing", source)
			}
			firstKey, _ := bucket.Cursor().First()
			if firstKey == nil {
				return nil
			}
			if _, ok := enabled[source]; !ok {
				return fmt.Errorf("Source %q is disabled while private recovery state remains", source)
			}
			return nil
		})
	})
}

// BindStreamKind atomically creates or verifies the durable RecordKind binding
// for a StreamRef.
func (d *DB) BindStreamKind(streamRef api.StreamRef) error {
	if err := validateStreamRef(streamRef); err != nil {
		return err
	}
	return d.db.Update(func(tx *bolt.Tx) error {
		return bindStreamKind(tx.Bucket(bucketStreamKind), streamRef)
	})
}

func bindStreamKind(bucket *bolt.Bucket, streamRef api.StreamRef) error {
	if err := validateStreamRef(streamRef); err != nil {
		return err
	}
	key := []byte(streamRef.ID)
	if current := bucket.Get(key); current != nil {
		if string(current) != string(streamRef.Kind) {
			return fmt.Errorf("stream %q changed record kind from %q to %q", streamRef.ID, current, streamRef.Kind)
		}
		return nil
	}
	return bucket.Put(key, []byte(streamRef.Kind))
}

func validateStreamRef(streamRef api.StreamRef) error {
	if streamRef.ID == "" || streamRef.Kind == "" {
		return errors.New("invalid stream kind binding")
	}
	if !utf8.ValidString(streamRef.ID) || !utf8.ValidString(string(streamRef.Kind)) {
		return errors.New("stream reference is not valid UTF-8")
	}
	if len(streamRef.ID) > bolt.MaxKeySize {
		return fmt.Errorf("stream reference is too large: %d bytes", len(streamRef.ID))
	}
	return nil
}

func (d *DB) GetFileCheckpoint(streamRef, path string) (FileCheckpoint, bool, error) {
	var out FileCheckpoint
	found, err := d.get(bucketSource, stateKey(streamRef, path), &out)
	if err == nil && found {
		err = validateFileCheckpoint(out)
	}
	return out, found, err
}

func (d *DB) ListFileCheckpoints(streamRef string) ([]FileCheckpoint, error) {
	var out []FileCheckpoint
	err := d.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketSource).ForEach(func(_, raw []byte) error {
			var checkpoint FileCheckpoint
			if err := json.Unmarshal(raw, &checkpoint); err != nil {
				return err
			}
			if checkpoint.StreamRef == streamRef && checkpoint.Path != "" {
				if err := validateFileCheckpoint(checkpoint); err != nil {
					return err
				}
				out = append(out, checkpoint)
			}
			return nil
		})
	})
	return out, err
}

func (d *DB) GetSourceStream(streamRef string) (SourceStream, bool, error) {
	var out SourceStream
	found, err := d.get(bucketSource, stateKey("stream", streamRef), &out)
	if err == nil && found {
		err = validateSourceStream(out)
	}
	return out, found, err
}

func (d *DB) PutSourceStream(stream SourceStream) error {
	if err := validateSourceStream(stream); err != nil {
		return err
	}
	return d.put(bucketSource, stateKey("stream", stream.StreamRef), stream)
}

func (d *DB) ListSourceStreams() ([]SourceStream, error) {
	var out []SourceStream
	err := d.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketSource).ForEach(func(key, raw []byte) error {
			var identity struct {
				Path string `json:"path"`
			}
			if err := json.Unmarshal(raw, &identity); err != nil {
				return err
			}
			if identity.Path != "" {
				return nil
			}
			var stream SourceStream
			if err := json.Unmarshal(raw, &stream); err != nil {
				return err
			}
			if err := validateSourceStream(stream); err != nil {
				return err
			}
			if !bytes.Equal(key, stateKey("stream", stream.StreamRef)) {
				return fmt.Errorf("source stream %q is stored under a non-canonical key", stream.StreamRef)
			}
			out = append(out, stream)
			return nil
		})
	})
	return out, err
}

// CommitSource atomically advances one or more physical file cursors together
// with the stream's cumulative loss outcome. This prevents a Source-internal
// drop from crossing an earlier, not-yet-acknowledged Delivery.
func (d *DB) CommitSource(checkpoints []FileCheckpoint, stream SourceStream) error {
	if err := validateSourceStream(stream); err != nil {
		return err
	}
	for _, checkpoint := range checkpoints {
		if checkpoint.StreamRef != stream.StreamRef {
			return errors.New("invalid source checkpoint")
		}
		if err := validateFileCheckpoint(checkpoint); err != nil {
			return err
		}
	}
	checkpoints = normalizeFileCheckpoints(checkpoints)
	checkpointBytes := make([][]byte, len(checkpoints))
	for i, checkpoint := range checkpoints {
		raw, err := json.Marshal(checkpoint)
		if err != nil {
			return err
		}
		checkpointBytes[i] = raw
	}
	streamBytes, err := json.Marshal(stream)
	if err != nil {
		return err
	}
	return d.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(bucketSource)
		index := tx.Bucket(bucketSourceFileIndex)
		for i, checkpoint := range checkpoints {
			if err := putFileCheckpoint(bucket, index, checkpoint, checkpointBytes[i]); err != nil {
				return err
			}
		}
		return bucket.Put(stateKey("stream", stream.StreamRef), streamBytes)
	})
}

func (d *DB) GetSinkStream(sinkName, streamRef string) (SinkStream, bool, error) {
	var out SinkStream
	found, err := d.get(bucketSink, stateKey(sinkName, streamRef), &out)
	if err == nil && found {
		err = validateSinkStream(out)
		if err == nil && (out.SinkName != sinkName || out.StreamRef != streamRef) {
			err = errors.New("sink stream identity does not match lookup key")
		}
	}
	return out, found, err
}

func (d *DB) PutSinkStream(sinkName string, stream SinkStream) error {
	if sinkName == "" {
		return errors.New("invalid sink stream identity")
	}
	if stream.SinkName != "" && stream.SinkName != sinkName {
		return errors.New("sink stream name does not match target bucket")
	}
	stream.SinkName = sinkName
	if err := validateSinkStream(stream); err != nil {
		return err
	}
	return d.put(bucketSink, stateKey(sinkName, stream.StreamRef), stream)
}

func (d *DB) GetFinalizeIntent(streamRef string, revision uint64) (FinalizeIntent, bool, error) {
	var out FinalizeIntent
	found, err := d.get(bucketPipeline, stateKey(streamRef, strconv.FormatUint(revision, 10)), &out)
	if err == nil && found {
		err = validateFinalizeIntent(out)
		if err == nil && out.TargetID != d.targetID {
			err = errors.New("finalize intent target does not match state target")
		}
	}
	return out, found, err
}

func (d *DB) PutFinalizeIntent(intent FinalizeIntent) error {
	if err := validateFinalizeIntent(intent); err != nil {
		return err
	}
	if intent.TargetID != d.targetID {
		return errors.New("finalize intent target does not match state target")
	}
	raw, err := json.Marshal(intent)
	if err != nil {
		return err
	}
	return d.db.Update(func(tx *bolt.Tx) error {
		if intent.StreamKind != "" {
			if err := bindStreamKind(tx.Bucket(bucketStreamKind), api.StreamRef{ID: intent.StreamRef, Kind: intent.StreamKind}); err != nil {
				return err
			}
		}
		return tx.Bucket(bucketPipeline).Put(stateKey(intent.StreamRef, strconv.FormatUint(intent.Revision, 10)), raw)
	})
}

func (d *DB) ListFinalizeIntents() ([]FinalizeIntent, error) {
	var out []FinalizeIntent
	err := d.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketPipeline).ForEach(func(_, raw []byte) error {
			var intent FinalizeIntent
			if err := json.Unmarshal(raw, &intent); err != nil {
				return err
			}
			if err := validateFinalizeIntent(intent); err != nil {
				return err
			}
			if intent.TargetID != d.targetID {
				return errors.New("finalize intent target does not match state target")
			}
			out = append(out, intent)
			return nil
		})
	})
	return out, err
}

// DeleteStream removes recovery metadata only after a backend-specific cleanup
// protocol has made the stream's object family unreachable.
func (d *DB) DeleteStream(streamRef string) error {
	return d.db.Update(func(tx *bolt.Tx) error {
		source := tx.Bucket(bucketSource)
		fileIndex := tx.Bucket(bucketSourceFileIndex)
		var sourceKeys [][]byte
		var indexKeys [][]byte
		if err := source.ForEach(func(key, raw []byte) error {
			var identity struct {
				StreamRef string `json:"stream_ref"`
				FileID    string `json:"file_id"`
				Path      string `json:"path"`
			}
			if err := json.Unmarshal(raw, &identity); err != nil {
				return err
			}
			if identity.StreamRef != streamRef {
				return nil
			}
			sourceKeys = append(sourceKeys, append([]byte(nil), key...))
			if identity.Path != "" {
				indexKeys = append(indexKeys, sourceFileIndexKey(streamRef, identity.FileID))
			}
			return nil
		}); err != nil {
			return err
		}
		for _, key := range sourceKeys {
			if err := source.Delete(key); err != nil {
				return err
			}
		}
		for _, key := range indexKeys {
			if err := fileIndex.Delete(key); err != nil {
				return err
			}
		}
		for _, bucketName := range [][]byte{bucketPipeline, bucketSink} {
			bucket := tx.Bucket(bucketName)
			var keys [][]byte
			if err := bucket.ForEach(func(key, raw []byte) error {
				var identity struct {
					StreamRef string `json:"stream_ref"`
				}
				if err := json.Unmarshal(raw, &identity); err != nil {
					return err
				}
				if identity.StreamRef == streamRef {
					keys = append(keys, append([]byte(nil), key...))
				}
				return nil
			}); err != nil {
				return err
			}
			for _, key := range keys {
				if err := bucket.Delete(key); err != nil {
					return err
				}
			}
		}
		return tx.Bucket(bucketStreamKind).Delete([]byte(streamRef))
	})
}

func validateSourceFileIndex(tx *bolt.Tx) error {
	source := tx.Bucket(bucketSource)
	index := tx.Bucket(bucketSourceFileIndex)
	expected := make(map[string][]byte)
	if err := source.ForEach(func(key, raw []byte) error {
		checkpoint, isCheckpoint, err := decodeFileCheckpoint(raw)
		if err != nil {
			return err
		}
		if !isCheckpoint {
			return nil
		}
		if canonicalKey := stateKey(checkpoint.StreamRef, checkpoint.Path); !bytes.Equal(key, canonicalKey) {
			return fmt.Errorf("source checkpoint for stream_ref=%q path=%q is stored under non-canonical key %s (expected %s)", checkpoint.StreamRef, checkpoint.Path, key, canonicalKey)
		}
		indexKey := sourceFileIndexKey(checkpoint.StreamRef, checkpoint.FileID)
		if previous, found := expected[string(indexKey)]; found {
			return fmt.Errorf("source file index %q has duplicate checkpoints %q and %q", indexKey, previous, key)
		}
		expected[string(indexKey)] = append([]byte(nil), key...)
		if indexedKey := index.Get(indexKey); !bytes.Equal(indexedKey, key) {
			return fmt.Errorf("source file index for stream_ref=%q file_id=%q does not match checkpoint key %q", checkpoint.StreamRef, checkpoint.FileID, key)
		}
		return nil
	}); err != nil {
		return err
	}
	return index.ForEach(func(indexKey, checkpointKey []byte) error {
		expectedKey, found := expected[string(indexKey)]
		if !found {
			return fmt.Errorf("source file index %q has no checkpoint", indexKey)
		}
		if !bytes.Equal(checkpointKey, expectedKey) {
			return fmt.Errorf("source file index %q points to %q, expected %q", indexKey, checkpointKey, expectedKey)
		}
		return nil
	})
}

func putFileCheckpoint(source, index *bolt.Bucket, checkpoint FileCheckpoint, raw []byte) error {
	checkpointKey := stateKey(checkpoint.StreamRef, checkpoint.Path)
	var existingAtPath FileCheckpoint
	hasExistingAtPath := false
	if existingRaw := source.Get(checkpointKey); existingRaw != nil {
		existing, isCheckpoint, err := decodeFileCheckpoint(existingRaw)
		if err != nil {
			return fmt.Errorf("decode source record at checkpoint path %q: %w", checkpoint.Path, err)
		}
		if !isCheckpoint || !bytes.Equal(stateKey(existing.StreamRef, existing.Path), checkpointKey) {
			return fmt.Errorf("source record at checkpoint path %q is not a valid checkpoint", checkpoint.Path)
		}
		existingAtPath = existing
		hasExistingAtPath = true
	}
	indexKey := sourceFileIndexKey(checkpoint.StreamRef, checkpoint.FileID)
	indexedKey := index.Get(indexKey)
	if indexedKey != nil {
		indexed, err := indexedFileCheckpoint(source, indexedKey, checkpoint.StreamRef, checkpoint.FileID)
		if err != nil {
			return err
		}
		if bytes.Equal(indexedKey, checkpointKey) {
			if checkpointCursorRegresses(checkpoint, indexed) {
				return checkpointSupersededError(checkpoint, indexed)
			}
		} else {
			if !preferCheckpoint(checkpoint, indexed) {
				return checkpointSupersededError(checkpoint, indexed)
			}
			if err := source.Delete(indexedKey); err != nil {
				return err
			}
		}
	}
	if err := index.Put(indexKey, checkpointKey); err != nil {
		return err
	}
	if hasExistingAtPath && existingAtPath.FileID != checkpoint.FileID {
		oldIndexKey := sourceFileIndexKey(existingAtPath.StreamRef, existingAtPath.FileID)
		if bytes.Equal(index.Get(oldIndexKey), checkpointKey) {
			if err := index.Delete(oldIndexKey); err != nil {
				return err
			}
		}
	}
	return source.Put(checkpointKey, raw)
}

func checkpointSupersededError(checkpoint, persisted FileCheckpoint) error {
	return fmt.Errorf(
		"%w: stream_ref=%q file_id=%q candidate path=%q revision=%d offset=%d, persisted path=%q revision=%d offset=%d",
		ErrFileCheckpointSuperseded,
		checkpoint.StreamRef,
		checkpoint.FileID,
		checkpoint.Path,
		checkpoint.Revision,
		checkpoint.Offset,
		persisted.Path,
		persisted.Revision,
		persisted.Offset,
	)
}

func normalizeFileCheckpoints(checkpoints []FileCheckpoint) []FileCheckpoint {
	normalized := make([]FileCheckpoint, 0, len(checkpoints))
	byFileID := make(map[string]int)
	for _, checkpoint := range checkpoints {
		key := string(sourceFileIndexKey(checkpoint.StreamRef, checkpoint.FileID))
		index, found := byFileID[key]
		if !found {
			byFileID[key] = len(normalized)
			normalized = append(normalized, checkpoint)
			continue
		}
		if preferCheckpoint(checkpoint, normalized[index]) {
			normalized[index] = checkpoint
		}
	}
	return normalized
}

func indexedFileCheckpoint(source *bolt.Bucket, checkpointKey []byte, streamRef, fileID string) (FileCheckpoint, error) {
	raw := source.Get(checkpointKey)
	if raw == nil {
		return FileCheckpoint{}, fmt.Errorf("source file index for stream_ref=%q file_id=%q points to a missing record", streamRef, fileID)
	}
	checkpoint, isCheckpoint, err := decodeFileCheckpoint(raw)
	if err != nil {
		return FileCheckpoint{}, fmt.Errorf("decode source file index target for stream_ref=%q file_id=%q: %w", streamRef, fileID, err)
	}
	if !isCheckpoint {
		return FileCheckpoint{}, fmt.Errorf("source file index for stream_ref=%q file_id=%q points to a non-checkpoint record", streamRef, fileID)
	}
	if checkpoint.StreamRef != streamRef || checkpoint.FileID != fileID {
		return FileCheckpoint{}, fmt.Errorf(
			"source file index for stream_ref=%q file_id=%q points to checkpoint stream_ref=%q file_id=%q",
			streamRef, fileID, checkpoint.StreamRef, checkpoint.FileID,
		)
	}
	if !bytes.Equal(stateKey(checkpoint.StreamRef, checkpoint.Path), checkpointKey) {
		return FileCheckpoint{}, fmt.Errorf("source file index for stream_ref=%q file_id=%q points to a checkpoint stored under the wrong key", streamRef, fileID)
	}
	return checkpoint, nil
}

func decodeFileCheckpoint(raw []byte) (FileCheckpoint, bool, error) {
	var checkpoint FileCheckpoint
	if err := json.Unmarshal(raw, &checkpoint); err != nil {
		return FileCheckpoint{}, false, err
	}
	if checkpoint.Path == "" {
		return FileCheckpoint{}, false, nil
	}
	if err := validateFileCheckpoint(checkpoint); err != nil {
		return FileCheckpoint{}, false, err
	}
	return checkpoint, true, nil
}

func validateFileCheckpoint(checkpoint FileCheckpoint) error {
	if checkpoint.StreamRef == "" || checkpoint.FileID == "" || checkpoint.Path == "" || checkpoint.Offset < 0 {
		return fmt.Errorf("invalid source checkpoint stream_ref=%q file_id=%q path=%q offset=%d", checkpoint.StreamRef, checkpoint.FileID, checkpoint.Path, checkpoint.Offset)
	}
	if checkpoint.HashBytes < 0 || checkpoint.HashBytes > maxCheckpointHashBytes {
		return fmt.Errorf("invalid source checkpoint hash_bytes %d for stream_ref=%q path=%q", checkpoint.HashBytes, checkpoint.StreamRef, checkpoint.Path)
	}
	return nil
}

func validateStoredState(tx *bolt.Tx, targetID string) error {
	streamKinds := tx.Bucket(bucketStreamKind)
	if err := streamKinds.ForEach(func(key, value []byte) error {
		if err := validateStreamRef(api.StreamRef{ID: string(key), Kind: api.RecordKind(value)}); err != nil {
			return fmt.Errorf("invalid persisted stream kind binding: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}
	if err := tx.Bucket(bucketSource).ForEach(func(key, raw []byte) error {
		var identity struct {
			StreamRef string `json:"stream_ref"`
			Path      string `json:"path"`
		}
		if err := json.Unmarshal(raw, &identity); err != nil {
			return fmt.Errorf("decode source state: %w", err)
		}
		if identity.Path != "" {
			return nil
		}
		var stream SourceStream
		if err := json.Unmarshal(raw, &stream); err != nil {
			return fmt.Errorf("decode source stream: %w", err)
		}
		if err := validateSourceStream(stream); err != nil {
			return err
		}
		if !bytes.Equal(key, stateKey("stream", stream.StreamRef)) {
			return fmt.Errorf("source stream %q is stored under a non-canonical key", stream.StreamRef)
		}
		return bindStreamKind(streamKinds, api.StreamRef{ID: stream.StreamRef, Kind: api.RecordKindContainerLog})
	}); err != nil {
		return err
	}
	if err := tx.Bucket(bucketSink).ForEach(func(key, raw []byte) error {
		var stream SinkStream
		if err := json.Unmarshal(raw, &stream); err != nil {
			return fmt.Errorf("decode sink stream: %w", err)
		}
		if err := validateSinkStream(stream); err != nil {
			return err
		}
		if !bytes.Equal(key, stateKey(stream.SinkName, stream.StreamRef)) {
			return fmt.Errorf("sink stream %q/%q is stored under a non-canonical key", stream.SinkName, stream.StreamRef)
		}
		if streamKinds.Get([]byte(stream.StreamRef)) == nil {
			return bindStreamKind(streamKinds, api.StreamRef{ID: stream.StreamRef, Kind: api.RecordKindContainerLog})
		}
		return nil
	}); err != nil {
		return err
	}
	if err := tx.Bucket(bucketPipeline).ForEach(func(key, raw []byte) error {
		var intent FinalizeIntent
		if err := json.Unmarshal(raw, &intent); err != nil {
			return fmt.Errorf("decode finalize intent: %w", err)
		}
		if err := validateFinalizeIntent(intent); err != nil {
			return err
		}
		if intent.TargetID != targetID {
			return errors.New("finalize intent target does not match state target")
		}
		expected := stateKey(intent.StreamRef, strconv.FormatUint(intent.Revision, 10))
		if !bytes.Equal(key, expected) {
			return fmt.Errorf("finalize intent for stream %q revision %d is stored under a non-canonical key", intent.StreamRef, intent.Revision)
		}
		kind := intent.StreamKind
		if kind == "" {
			kind = api.RecordKindContainerLog
		}
		return bindStreamKind(streamKinds, api.StreamRef{ID: intent.StreamRef, Kind: kind})
	}); err != nil {
		return err
	}
	return nil
}

func validateSourceStream(stream SourceStream) error {
	if stream.StreamRef == "" || stream.Revision == 0 {
		return fmt.Errorf("invalid source stream identity stream_ref=%q revision=%d", stream.StreamRef, stream.Revision)
	}
	resource := stream.Resource
	if resource.SandboxID == "" || resource.ClusterName == "" || resource.Namespace == "" || resource.PodName == "" || resource.PodUID == "" || resource.NodeName == "" || resource.Container == "" || resource.LogDirectory == "" || !filepath.IsAbs(resource.LogDirectory) {
		return fmt.Errorf("source stream %q has incomplete frozen resource identity", stream.StreamRef)
	}
	if stream.AcknowledgedRevision > stream.Revision {
		return fmt.Errorf("source stream %q acknowledged revision %d exceeds revision %d", stream.StreamRef, stream.AcknowledgedRevision, stream.Revision)
	}
	if stream.FinalizingRevision != 0 && stream.FinalizingRevision != stream.Revision {
		return fmt.Errorf("source stream %q finalizing revision %d does not match revision %d", stream.StreamRef, stream.FinalizingRevision, stream.Revision)
	}
	if stream.FinalizingRevision != 0 && stream.AcknowledgedRevision >= stream.FinalizingRevision {
		return fmt.Errorf("source stream %q retains an acknowledged finalizing revision", stream.StreamRef)
	}
	if (stream.FinalizingRevision != 0) != (stream.FinalizingOutcome != nil) {
		return fmt.Errorf("source stream %q has inconsistent finalizing outcome state", stream.StreamRef)
	}
	if stream.FinalizingOutcome != nil {
		if err := validateOutcomeSnapshot(*stream.FinalizingOutcome); err != nil {
			return fmt.Errorf("source stream %q has invalid finalizing outcome: %w", stream.StreamRef, err)
		}
	}
	if stream.Ended != (stream.AcknowledgedRevision == stream.Revision && stream.FinalizingRevision == 0) {
		return fmt.Errorf("source stream %q has inconsistent ended state", stream.StreamRef)
	}
	if !stream.Ended && stream.Revision != stream.AcknowledgedRevision+1 {
		return fmt.Errorf("source stream %q revision %d is not contiguous after acknowledged revision %d", stream.StreamRef, stream.Revision, stream.AcknowledgedRevision)
	}
	if stream.CoverageStartedAt.IsZero() {
		if stream.InitialScanComplete || stream.MonitoringEpoch != "" {
			return fmt.Errorf("source stream %q has monitoring state without a coverage boundary", stream.StreamRef)
		}
	} else {
		if stream.CoverageStartedAt.Location() != time.UTC || stream.CoverageStartedAt.Nanosecond() != 0 {
			return fmt.Errorf("source stream %q has a non-canonical coverage boundary", stream.StreamRef)
		}
		if stream.MonitoringEpoch == "" {
			return fmt.Errorf("source stream %q has no monitoring epoch", stream.StreamRef)
		}
	}
	if stream.Guarantee != "" && stream.Guarantee != "durable" && stream.Guarantee != "best-effort" {
		return fmt.Errorf("source stream %q has unsupported guarantee %q", stream.StreamRef, stream.Guarantee)
	}
	if err := validateSourceOutcome(stream); err != nil {
		return fmt.Errorf("source stream %q: %w", stream.StreamRef, err)
	}
	return nil
}

func validateOutcomeSnapshot(outcome OutcomeSnapshot) error {
	if (outcome.HadDrops || outcome.HadSourceGaps) != (len(outcome.LossReasons) > 0) {
		return errors.New("loss flags do not match loss reasons")
	}
	if !sort.StringsAreSorted(outcome.LossReasons) {
		return errors.New("loss reasons are not sorted")
	}
	for index, reason := range outcome.LossReasons {
		if reason == "" || index > 0 && reason == outcome.LossReasons[index-1] {
			return errors.New("loss reasons are empty or duplicated")
		}
	}
	return nil
}

func validateSourceOutcome(stream SourceStream) error {
	reasons := make(map[string]struct{})
	dropIDs := make(map[string]struct{}, len(stream.Drops))
	for _, drop := range stream.Drops {
		if drop.ID == "" || drop.FileID == "" || drop.Path == "" || drop.FromOffset < 0 || drop.ToOffset < drop.FromOffset || drop.Reason == "" {
			return errors.New("invalid source drop record")
		}
		if _, exists := dropIDs[drop.ID]; exists {
			return errors.New("duplicate source drop record")
		}
		dropIDs[drop.ID] = struct{}{}
		reasons[drop.Reason] = struct{}{}
	}
	unresolvedGap := false
	gapIDs := make(map[string]struct{}, len(stream.Gaps))
	for _, gap := range stream.Gaps {
		if gap.ID == "" || gap.Path == "" || gap.FromOffset < 0 || gap.Reason == "" {
			return errors.New("invalid source gap record")
		}
		if _, exists := gapIDs[gap.ID]; exists {
			return errors.New("duplicate source gap record")
		}
		gapIDs[gap.ID] = struct{}{}
		if gap.ToOffset != nil && *gap.ToOffset < gap.FromOffset || gap.ResumeAt != nil && *gap.ResumeAt < gap.FromOffset || gap.RepairOffset != nil && *gap.RepairOffset < gap.FromOffset {
			return errors.New("source gap offset is before its start")
		}
		if gap.ToOffset != nil && gap.RepairOffset != nil && *gap.RepairOffset > *gap.ToOffset {
			return errors.New("source gap repair offset exceeds its end")
		}
		if gap.Coverage && gap.Resolved {
			return errors.New("coverage gap cannot be resolved")
		}
		if gap.Resolved {
			repairOffset := gap.FromOffset
			if gap.RepairOffset != nil {
				repairOffset = *gap.RepairOffset
			}
			if gap.ResumeAt != nil || gap.ToOffset == nil || repairOffset < *gap.ToOffset || gap.FileID == "" || gap.PrefixHash == "" || gap.HashBytes <= 0 {
				return errors.New("resolved source gap lacks exact repair evidence")
			}
		}
		if !gap.Resolved {
			unresolvedGap = true
			reasons[gap.Reason] = struct{}{}
		}
	}
	if stream.HadDrops != (len(stream.Drops) > 0) {
		return errors.New("had_drops does not match persisted drop records")
	}
	if stream.HadSourceGaps != unresolvedGap {
		return errors.New("had_source_gaps does not match unresolved gap records")
	}
	wantReasons := make([]string, 0, len(reasons))
	for reason := range reasons {
		wantReasons = append(wantReasons, reason)
	}
	sort.Strings(wantReasons)
	if !equalStrings(stream.LossReasons, wantReasons) {
		return errors.New("loss_reasons does not match persisted loss records")
	}
	return nil
}

func validateSinkStream(stream SinkStream) error {
	if stream.SinkName == "" || stream.StreamRef == "" || stream.Position < 0 {
		return fmt.Errorf("invalid sink stream identity stream_ref=%q position=%d", stream.StreamRef, stream.Position)
	}
	if stream.AppendIntent != nil {
		intent := stream.AppendIntent
		if intent.Position != stream.Position || intent.Length <= 0 || intent.Position > (1<<63-1)-intent.Length {
			return fmt.Errorf("sink stream %q has an invalid append intent range", stream.StreamRef)
		}
		digest, err := hex.DecodeString(intent.SHA256)
		if err != nil || len(digest) != sha256.Size || hex.EncodeToString(digest) != intent.SHA256 {
			return fmt.Errorf("sink stream %q has an invalid append intent digest", stream.StreamRef)
		}
		if stream.CurrentClosed {
			return fmt.Errorf("sink stream %q has an append intent for a closed generation", stream.StreamRef)
		}
	}
	for index, object := range stream.ClosedObjects {
		if object.Generation != uint64(index) || object.Key == "" || object.Size <= 0 || !canonicalUint(object.CRC64) {
			return fmt.Errorf("sink stream %q has invalid closed object %d", stream.StreamRef, index)
		}
	}
	return nil
}

func canonicalUint(value string) bool {
	if value == "" || len(value) > 1 && value[0] == '0' {
		return false
	}
	_, err := strconv.ParseUint(value, 10, 64)
	return err == nil
}

func validateFinalizeIntent(intent FinalizeIntent) error {
	if intent.StreamRef == "" || intent.Revision == 0 || intent.FinalizeID == "" || intent.TargetID == "" || intent.CoverageStartedAt.IsZero() || intent.FinalizedAt.IsZero() {
		return errors.New("invalid finalize intent")
	}
	if intent.SourceDone && !intent.SinkDone {
		return errors.New("finalize intent completed Source before Sink")
	}
	if intent.CoverageStartedAt.Location() != time.UTC || intent.CoverageStartedAt.Nanosecond() != 0 {
		return errors.New("finalize intent has a non-canonical coverage boundary")
	}
	if intent.FinalizedAt.Location() != time.UTC || intent.FinalizedAt.Nanosecond() != 0 {
		return errors.New("finalize intent has a non-canonical finalization time")
	}
	if intent.FinalizedAt.Before(intent.CoverageStartedAt) {
		return errors.New("finalize intent precedes its coverage boundary")
	}
	hasReplayData := finalizeIntentHasReplayData(intent)
	if hasReplayData && (intent.StreamKind == "" || intent.Resource == nil || intent.Outcome == nil || intent.EndToken == nil) {
		return errors.New("finalize intent has incomplete replay data")
	}
	if hasReplayData {
		if err := intent.Metadata.Validate(); err != nil {
			return fmt.Errorf("finalize intent has invalid metadata: %w", err)
		}
		streamRef := api.StreamRef{ID: intent.StreamRef, Kind: intent.StreamKind}
		if intent.EndToken.ID == "" || intent.EndToken.Source == "" || intent.EndToken.StreamRef != streamRef {
			return errors.New("finalize intent has an invalid end token")
		}
	}
	return nil
}

func finalizeIntentHasReplayData(intent FinalizeIntent) bool {
	return intent.StreamKind != "" || intent.Resource != nil || intent.Metadata != nil || intent.Outcome != nil || intent.EndToken != nil
}

func equalStrings(left, right []string) bool {
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

func preferCheckpoint(left, right FileCheckpoint) bool {
	if left.Revision != right.Revision {
		return left.Revision > right.Revision
	}
	if left.Offset != right.Offset {
		return left.Offset > right.Offset
	}
	if left.ObservedSize != right.ObservedSize {
		return left.ObservedSize > right.ObservedSize
	}
	if left.ModTimeUnixNano != right.ModTimeUnixNano {
		return left.ModTimeUnixNano > right.ModTimeUnixNano
	}
	return left.Path > right.Path
}

func checkpointCursorRegresses(candidate, persisted FileCheckpoint) bool {
	// Metadata may move backward after recording a file-reclaimed Gap. Only
	// revision and offset define whether the durable source cursor regressed.
	return candidate.Revision < persisted.Revision ||
		(candidate.Revision == persisted.Revision && candidate.Offset < persisted.Offset)
}

func sourceFileIndexKey(streamRef, fileID string) []byte {
	return stateKey(streamRef, fileID)
}

func (d *DB) get(bucket, key []byte, out any) (bool, error) {
	found := false
	err := d.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(bucket).Get(key)
		if raw == nil {
			return nil
		}
		if err := json.Unmarshal(raw, out); err != nil {
			return err
		}
		found = true
		return nil
	})
	return found, err
}

func (d *DB) put(bucket, key []byte, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return d.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucket).Put(key, raw)
	})
}

func stateKey(parts ...string) []byte {
	h := sha256.New()
	for _, part := range parts {
		_, _ = h.Write([]byte(strconv.Itoa(len(part))))
		_, _ = h.Write([]byte{':'})
		_, _ = h.Write([]byte(part))
	}
	return []byte(hex.EncodeToString(h.Sum(nil)))
}
