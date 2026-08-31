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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alibaba/opensandbox/internal/logger"
	"github.com/alibaba/opensandbox/nodeagent/pkg/api"
	"github.com/alibaba/opensandbox/nodeagent/pkg/config"
	"github.com/alibaba/opensandbox/nodeagent/pkg/registry"
	"github.com/alibaba/opensandbox/nodeagent/pkg/state"
	"github.com/alibaba/opensandbox/nodeagent/pkg/store"
	"github.com/alibaba/opensandbox/nodeagent/pkg/streamformat"
	"github.com/fsnotify/fsnotify"
	"github.com/google/uuid"
)

const (
	sourceName = api.SourceNameContainerLogs
	// This must not exceed the state package's on-disk checkpoint limit.
	maxFingerprintHashBytes = 4096
)

var logNamePattern = regexp.MustCompile(`^(\d+)\.log(?:\.(.+))?$`)

func (s *containerLogSource) commitSource(checkpoints []state.FileCheckpoint, stream state.SourceStream) error {
	err := s.state.CommitSource(checkpoints, stream)
	if errors.Is(err, state.ErrFileCheckpointSuperseded) {
		return api.Permanent(err)
	}
	return err
}

func init() {
	registry.RegisterSource(sourceName, func(dependencies registry.SourceDependencies) (api.Source, error) {
		provider, ok := dependencies.State.(interface {
			LegacyContainerLogCheckpoint() (state.LegacyContainerLogCheckpoint, error)
		})
		if !ok {
			return nil, errors.New("container-logs Source requires its legacy checkpoint adapter")
		}
		checkpoints, err := provider.LegacyContainerLogCheckpoint()
		if err != nil {
			return nil, err
		}
		cfg := dependencies.Config
		return newSource(sourceConfig{LogRoot: cfg.LogRoot, MaxLineBytes: cfg.MaxLineBytes, PartialTimeout: cfg.PartialTimeout, ReconcileInterval: config.InternalReconcileInterval, EndedStateRetention: cfg.EndedStateRetention, PruneEndedState: pruneEndedState(cfg)}, dependencies.Store, checkpoints, dependencies.Logger, dependencies.OnError), nil
	})
}

func pruneEndedState(cfg config.Config) bool {
	return cfg.Sink != config.SinkFile || cfg.FilePath == ""
}

type checkpointStore interface {
	GetFileCheckpoint(streamRef, path string) (state.FileCheckpoint, bool, error)
	ListFileCheckpoints(streamRef string) ([]state.FileCheckpoint, error)
	GetSourceStream(streamRef string) (state.SourceStream, bool, error)
	ListSourceStreams() ([]state.SourceStream, error)
	PutSourceStream(state.SourceStream) error
	CommitSource([]state.FileCheckpoint, state.SourceStream) error
	DeleteStream(string) error
}

var _ checkpointStore = state.LegacyContainerLogCheckpoint(nil)

type sourceConfig struct {
	LogRoot             string
	MaxLineBytes        int
	PartialTimeout      time.Duration
	ReconcileInterval   time.Duration
	EndedStateRetention time.Duration
	PruneEndedState     bool
}

type containerLogSource struct {
	cfg     sourceConfig
	store   store.View
	state   checkpointStore
	log     logger.Logger
	onError func(error)

	mu                           sync.Mutex
	streams                      map[string]*streamRuntime
	out                          chan<- api.SourceEvent
	cancel                       context.CancelFunc
	done                         chan struct{}
	scanCursor                   int
	epochID                      string
	watches                      map[string]watchIdentity
	pendingRootWatchReplacements map[string]map[string]struct{}
}

type watchIdentity struct {
	device uint64
	inode  uint64
}

type streamRuntime struct {
	mu          sync.Mutex
	resource    streamResource
	files       map[string]*fileRuntime
	assembler   *assembler
	pending     []*pendingSpan
	ended       bool
	endAcked    bool
	latePending bool
	revision    uint64
	stableEOF   int
	outcome     api.SourceOutcome
	persisted   state.SourceStream
}

type streamResource struct {
	api.Resource
	Terminated   bool
	LogDirectory string
}

type fileRuntime struct {
	fileID          string
	path            string
	restart         string
	readOffset      int64
	committed       int64
	observedSize    int64
	modTimeUnixNano int64
	fingerprint     fileFingerprint
	repairGapID     string
}

type fileFingerprint struct {
	Device     uint64
	Inode      uint64
	PrefixHash string
	HashBytes  int
}

type sourceSpan struct {
	FileID      string `json:"file_id"`
	Path        string `json:"path"`
	StartOffset int64  `json:"start_offset"`
	EndOffset   int64  `json:"end_offset"`
	Device      uint64 `json:"device"`
	Inode       uint64 `json:"inode"`
	PrefixHash  string `json:"prefix_hash"`
	HashBytes   int    `json:"hash_bytes"`
	RepairGapID string `json:"repair_gap_id,omitempty"`
	fingerprint fileFingerprint
	restart     string
}

type tokenValue struct {
	Spans       []sourceSpan `json:"spans"`
	DropReasons []string     `json:"drop_reasons,omitempty"`
	Revision    uint64       `json:"revision"`
}

type pendingSpan struct {
	span        sourceSpan
	file        *fileRuntime
	tokenID     string
	complete    bool
	dropReasons []string
}

func newSource(cfg sourceConfig, view store.View, checkpoints checkpointStore, log logger.Logger, onError func(error)) *containerLogSource {
	return &containerLogSource{cfg: cfg, store: view, state: checkpoints, log: log.Named(sourceName), onError: onError, streams: make(map[string]*streamRuntime), done: make(chan struct{}), epochID: uuid.NewString(), watches: make(map[string]watchIdentity), pendingRootWatchReplacements: make(map[string]map[string]struct{})}
}

func (s *containerLogSource) Capabilities() api.Capabilities {
	return api.Capabilities{RecordKinds: []api.RecordKind{api.RecordKindContainerLog}}
}

func (s *containerLogSource) Start(ctx context.Context, out chan<- api.SourceEvent) error {
	s.mu.Lock()
	if s.cancel != nil {
		s.mu.Unlock()
		return errors.New("container-logs source already started")
	}
	runCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.out = out
	s.mu.Unlock()
	go s.run(runCtx)
	return nil
}

func (s *containerLogSource) Stop(ctx context.Context) error {
	s.mu.Lock()
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Unlock()
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *containerLogSource) Acknowledge(_ context.Context, results []api.AckResult) error {
	type decodedAck struct {
		result api.AckResult
		value  tokenValue
	}
	type ackGroup struct {
		streamRef string
		items     []decodedAck
	}
	var groups []ackGroup
	groupByStream := make(map[string]int)
	for _, result := range results {
		if result.Token.Source != sourceName {
			return api.Permanent(fmt.Errorf("ack token source %q does not match %q", result.Token.Source, sourceName))
		}
		if result.Guarantee != api.GuaranteeDurable && result.Guarantee != api.GuaranteeBestEffort {
			return api.Permanent(fmt.Errorf("unsupported delivery guarantee %q", result.Guarantee))
		}
		if result.Disposition != api.AckDelivered && result.Disposition != api.AckIntentionalDrop {
			return api.Permanent(fmt.Errorf("unsupported acknowledgement disposition %q", result.Disposition))
		}
		var value tokenValue
		if err := json.Unmarshal(result.Token.Value, &value); err != nil {
			return api.Permanent(fmt.Errorf("decode ack token: %w", err))
		}
		if len(value.Spans) == 0 {
			return api.Permanent(errors.New("ack token contains no source span"))
		}
		streamRef := result.Token.StreamRef.ID
		index, exists := groupByStream[streamRef]
		if !exists {
			index = len(groups)
			groupByStream[streamRef] = index
			groups = append(groups, ackGroup{streamRef: streamRef})
		}
		groups[index].items = append(groups[index].items, decodedAck{result: result, value: value})
	}
	for _, group := range groups {
		s.mu.Lock()
		runtime := s.streams[group.streamRef]
		s.mu.Unlock()
		if runtime == nil {
			return api.Permanent(fmt.Errorf("unknown stream %q", group.streamRef))
		}
		runtime.mu.Lock()
		oldGuarantee := runtime.persisted.Guarantee
		oldComplete := make([]bool, len(runtime.pending))
		oldReasons := make([][]string, len(runtime.pending))
		for index, pending := range runtime.pending {
			oldComplete[index] = pending.complete
			oldReasons[index] = append([]string(nil), pending.dropReasons...)
		}
		rollback := func() {
			runtime.persisted.Guarantee = oldGuarantee
			for index, pending := range runtime.pending {
				pending.complete = oldComplete[index]
				pending.dropReasons = oldReasons[index]
			}
		}
		for _, item := range group.items {
			result, value := item.result, item.value
			if value.Revision != runtime.revision {
				rollback()
				runtime.mu.Unlock()
				return api.Permanent(fmt.Errorf("ack token revision %d does not match stream revision %d", value.Revision, runtime.revision))
			}
			if runtime.persisted.Guarantee != "" && runtime.persisted.Guarantee != string(result.Guarantee) {
				rollback()
				runtime.mu.Unlock()
				return api.Permanent(fmt.Errorf("stream guarantee changed from %q to %q", runtime.persisted.Guarantee, result.Guarantee))
			}
			indices, err := runtime.pendingForToken(result.Token.ID, value.Spans)
			if err != nil {
				if runtime.allCommitted(value.Spans) {
					continue
				}
				rollback()
				runtime.mu.Unlock()
				return api.Permanent(err)
			}
			for _, index := range indices {
				runtime.pending[index].complete = true
				for _, reason := range value.DropReasons {
					runtime.pending[index].dropReasons = addReason(runtime.pending[index].dropReasons, reason)
				}
				if result.Disposition == api.AckIntentionalDrop {
					reason := result.Reason
					if reason == "" {
						reason = "pipeline-policy-drop"
					}
					runtime.pending[index].dropReasons = addReason(runtime.pending[index].dropReasons, reason)
				}
			}
			if runtime.persisted.Guarantee == "" {
				runtime.persisted.Guarantee = string(result.Guarantee)
			}
		}
		if err := s.commitCompletedLocked(runtime); err != nil {
			rollback()
			runtime.mu.Unlock()
			return fmt.Errorf("commit source checkpoint: %w", err)
		}
		runtime.mu.Unlock()
	}
	return nil
}

func (s *containerLogSource) AcknowledgeEnd(_ context.Context, token api.EndToken) error {
	revision, err := validateEndToken(token)
	if err != nil {
		return err
	}
	s.mu.Lock()
	runtime := s.streams[token.StreamRef.ID]
	if runtime == nil {
		persisted, found, err := s.state.GetSourceStream(token.StreamRef.ID)
		if err != nil {
			s.mu.Unlock()
			return err
		}
		if !found {
			s.mu.Unlock()
			return api.Permanent(fmt.Errorf("unknown stream %q", token.StreamRef.ID))
		}
		next, changed, err := s.acknowledgedEndState(persisted, revision, persisted.Resource.LogDirectory)
		if err != nil {
			s.mu.Unlock()
			return err
		}
		if changed {
			err = s.state.PutSourceStream(next)
		}
		s.mu.Unlock()
		return err
	}
	s.mu.Unlock()
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.persisted.AcknowledgedRevision < revision && len(runtime.pending) != 0 {
		return api.Permanent(errors.New("cannot acknowledge stream end with unresolved source spans"))
	}
	next, changed, err := s.acknowledgedEndState(runtime.persisted, revision, runtime.resource.LogDirectory)
	if err != nil {
		return err
	}
	if changed {
		if err := s.state.PutSourceStream(next); err != nil {
			return err
		}
		runtime.persisted = next
		runtime.endAcked = true
		runtime.outcome = outcomeFromState(next)
	}
	return nil
}

func validateEndToken(token api.EndToken) (uint64, error) {
	if token.Source != sourceName {
		return 0, api.Permanent(fmt.Errorf("end token source %q does not match", token.Source))
	}
	if token.StreamRef.ID == "" || token.StreamRef.Kind != api.RecordKindContainerLog {
		return 0, api.Permanent(errors.New("invalid end token stream reference"))
	}
	revision, err := strconv.ParseUint(string(token.Value), 10, 64)
	if err != nil || revision == 0 || strconv.FormatUint(revision, 10) != string(token.Value) {
		return 0, api.Permanent(errors.New("invalid end token revision"))
	}
	if token.ID != endTokenID(token.StreamRef.ID, token.Value) {
		return 0, api.Permanent(errors.New("invalid end token ID"))
	}
	return revision, nil
}

func (s *containerLogSource) acknowledgedEndState(current state.SourceStream, revision uint64, logDirectory string) (state.SourceStream, bool, error) {
	if current.AcknowledgedRevision >= revision {
		return current, false, nil
	}
	if revision != current.Revision || current.FinalizingRevision != revision {
		return state.SourceStream{}, false, api.Permanent(fmt.Errorf("end token revision %d is not the active finalization", revision))
	}
	next := current
	next.AcknowledgedRevision = revision
	next.FinalizingRevision = 0
	next.FinalizingOutcome = nil
	next.Ended = true
	if !next.CoverageStartedAt.IsZero() && next.MonitoringEpoch != s.epochID {
		appendCoverageGapToStream(&next, next.MonitoringEpoch, "monitor-interrupted", logDirectory)
		next.MonitoringEpoch = s.epochID
	}
	if next.RepairDeadline == nil {
		deadline := time.Now().UTC().Add(s.cfg.EndedStateRetention)
		next.RepairDeadline = &deadline
	}
	return next, true, nil
}

func (r *streamRuntime) pendingForToken(tokenID string, spans []sourceSpan) ([]int, error) {
	var indices []int
	for index, pending := range r.pending {
		if pending.tokenID == tokenID {
			indices = append(indices, index)
		}
	}
	var pendingSpans []sourceSpan
	for _, index := range indices {
		pendingSpans = appendSourceSpan(pendingSpans, r.pending[index].span)
	}
	if len(pendingSpans) != len(spans) {
		return nil, errors.New("ack token does not match pending source spans")
	}
	for i := range spans {
		if !sameSpan(pendingSpans[i], spans[i]) {
			return nil, errors.New("ack token source span identity mismatch")
		}
	}
	return indices, nil
}

func (r *streamRuntime) allCommitted(spans []sourceSpan) bool {
	for _, span := range spans {
		var matched *fileRuntime
		for _, file := range r.files {
			if file.fileID == span.FileID {
				matched = file
				break
			}
		}
		if matched == nil || matched.committed < span.EndOffset || !sameFingerprint(matched.fingerprint, span) {
			return false
		}
	}
	return true
}

func sameSpan(left, right sourceSpan) bool {
	return left.FileID == right.FileID && left.Path == right.Path && left.StartOffset == right.StartOffset && left.EndOffset == right.EndOffset && left.Device == right.Device && left.Inode == right.Inode && left.PrefixHash == right.PrefixHash && left.HashBytes == right.HashBytes && left.RepairGapID == right.RepairGapID
}

func sameFingerprint(fingerprint fileFingerprint, span sourceSpan) bool {
	return fingerprint.Device == span.Device && fingerprint.Inode == span.Inode && fingerprint.PrefixHash == span.PrefixHash && fingerprint.HashBytes == span.HashBytes
}

func (s *containerLogSource) commitCompletedLocked(runtime *streamRuntime) error {
	count := 0
	for count < len(runtime.pending) && runtime.pending[count].complete {
		count++
	}
	if count == 0 {
		return nil
	}
	next := runtime.persisted
	next.LossReasons = append([]string(nil), runtime.persisted.LossReasons...)
	next.Drops = append([]state.SourceDropRecord(nil), runtime.persisted.Drops...)
	next.Gaps = append([]state.GapRecord(nil), runtime.persisted.Gaps...)
	type checkpointCommit struct {
		file       *fileRuntime
		fileID     string
		path       string
		endOffset  int64
		lastSeq    int
		checkpoint state.FileCheckpoint
	}
	liveByFileID := make(map[string]*fileRuntime)
	for path, file := range runtime.files {
		if file == nil || file.fileID == "" || file.path != path {
			return api.Permanent(fmt.Errorf("invalid live source runtime at path %q", path))
		}
		if existing := liveByFileID[file.fileID]; existing != nil && existing != file {
			return api.Permanent(fmt.Errorf("duplicate live source runtime for file_id %q", file.fileID))
		}
		liveByFileID[file.fileID] = file
	}
	commitsByFile := make(map[*fileRuntime]*checkpointCommit)
	for sequence, pending := range runtime.pending[:count] {
		file := pending.file
		if file == nil || pending.span.FileID == "" || file.fileID != pending.span.FileID || !sameFingerprint(file.fingerprint, pending.span) {
			return api.Permanent(fmt.Errorf("pending source span for path %q has inconsistent file identity", pending.span.Path))
		}
		commit := commitsByFile[file]
		if commit == nil {
			commit = &checkpointCommit{file: file, fileID: file.fileID, path: file.path}
			commitsByFile[file] = commit
		} else if commit.path != file.path {
			return api.Permanent(fmt.Errorf("source file_id %q changed path while committing", file.fileID))
		}
		commit.endOffset = max(commit.endOffset, pending.span.EndOffset)
		commit.lastSeq = sequence
		for _, reason := range pending.dropReasons {
			next.HadDrops = true
			next.LossReasons = addReason(next.LossReasons, reason)
			next.Drops = appendDrop(next.Drops, state.SourceDropRecord{ID: spanResultID(runtime.persisted.StreamRef, pending.span, reason), FileID: pending.span.FileID, Path: pending.span.Path, FromOffset: pending.span.StartOffset, ToOffset: pending.span.EndOffset, Reason: reason})
		}
		if pending.span.RepairGapID != "" {
			foundRepairGap := false
			for index := range next.Gaps {
				gap := &next.Gaps[index]
				if gap.ID != pending.span.RepairGapID {
					continue
				}
				foundRepairGap = true
				if gap.Resolved || gap.ResumeAt != nil || gap.FileID != pending.span.FileID || gap.Device != pending.span.Device || gap.Inode != pending.span.Inode || gap.PrefixHash != pending.span.PrefixHash || gap.HashBytes != pending.span.HashBytes {
					return api.Permanent(fmt.Errorf("repair span does not match gap %q", gap.ID))
				}
				expected := gap.FromOffset
				if gap.RepairOffset != nil {
					expected = *gap.RepairOffset
				}
				if pending.span.StartOffset != expected || pending.span.EndOffset < pending.span.StartOffset {
					return api.Permanent(fmt.Errorf("repair span for gap %q is not contiguous at offset %d", gap.ID, expected))
				}
				repairOffset := pending.span.EndOffset
				gap.RepairOffset = &repairOffset
				if gap.Coverage {
					// The repair file shrank after this span was emitted. Preserve
					// ACK ordering, but never resolve identity-ambiguous coverage.
					break
				}
				if gap.ToOffset != nil && repairOffset >= *gap.ToOffset {
					gap.Resolved = true
				}
				break
			}
			if !foundRepairGap {
				return api.Permanent(fmt.Errorf("repair span references unknown gap %q", pending.span.RepairGapID))
			}
		}
	}
	recomputeOutcome(&next)
	winnerByPath := make(map[string]*checkpointCommit)
	for _, commit := range commitsByFile {
		commit.endOffset = max(commit.endOffset, commit.file.committed)
		if liveOwner := liveByFileID[commit.fileID]; liveOwner != nil && liveOwner != commit.file {
			continue
		}
		owner := runtime.files[commit.path]
		if owner != nil && owner != commit.file {
			continue
		}
		commit.checkpoint = checkpointFor(runtime.persisted.StreamRef, runtime.revision, commit.file, commit.endOffset)
		existing := winnerByPath[commit.path]
		if existing == nil || commit.lastSeq > existing.lastSeq || commit.lastSeq == existing.lastSeq && commit.fileID > existing.fileID {
			winnerByPath[commit.path] = commit
		}
	}
	commits := make([]*checkpointCommit, 0, len(winnerByPath))
	for _, commit := range winnerByPath {
		commits = append(commits, commit)
	}
	sort.Slice(commits, func(i, j int) bool {
		if commits[i].path != commits[j].path {
			return commits[i].path < commits[j].path
		}
		return commits[i].fileID < commits[j].fileID
	})
	checkpoints := make([]state.FileCheckpoint, len(commits))
	for index, commit := range commits {
		checkpoints[index] = commit.checkpoint
	}
	if err := s.commitSource(checkpoints, next); err != nil {
		return err
	}
	for _, commit := range commitsByFile {
		commit.file.committed = commit.endOffset
	}
	runtime.persisted = next
	runtime.outcome = outcomeFromState(next)
	runtime.pending = runtime.pending[count:]
	return nil
}

func (s *containerLogSource) run(ctx context.Context) {
	defer close(s.done)
	defer close(s.out)
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		s.fail(err)
		return
	}
	defer watcher.Close()
	if err := s.restoreStreams(ctx, watcher); err != nil {
		s.fail(err)
		return
	}
	if err := s.scanAll(ctx, watcher); err != nil {
		s.fail(err)
	}
	ticker := time.NewTicker(s.cfg.ReconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.store.Changes():
			if err := s.scanAll(ctx, watcher); err != nil {
				s.fail(err)
			}
		case event, ok := <-watcher.Events:
			if !ok {
				if err := s.recordWatchDiscontinuityAll("event-channel-closed"); err != nil {
					s.fail(err)
				}
				s.fail(errors.New("fsnotify event channel closed"))
				return
			}
			if event.Name != "" {
				if err := s.recordUnobservedWatchEvent(event); err != nil {
					s.fail(err)
					continue
				}
				if err := s.scanAll(ctx, watcher); err != nil {
					s.fail(err)
				}
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				if recordErr := s.recordWatchDiscontinuityAll("error-channel-closed"); recordErr != nil {
					s.fail(recordErr)
				}
				s.fail(errors.New("fsnotify error channel closed"))
				return
			}
			if err != nil {
				if recordErr := s.recordWatchDiscontinuityAll("watch-error-" + uuid.NewString()); recordErr != nil {
					s.fail(recordErr)
					return
				}
				s.fail(fmt.Errorf("fsnotify: %w", err))
				if scanErr := s.scanAll(ctx, watcher); scanErr != nil {
					s.fail(scanErr)
				}
			}
		case now := <-ticker.C:
			if err := s.scanAll(ctx, watcher); err != nil {
				s.fail(err)
			}
			s.flushExpired(ctx, now)
		}
	}
}

func (s *containerLogSource) recordUnobservedWatchEvent(event fsnotify.Event) error {
	if event.Name == "" || event.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Remove|fsnotify.Rename) == 0 {
		return nil
	}
	missing := false
	if _, err := os.Stat(event.Name); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		missing = true
	}
	if !missing && event.Op&(fsnotify.Remove|fsnotify.Rename) == 0 {
		return nil
	}
	s.mu.Lock()
	runtimes := make([]*streamRuntime, 0, len(s.streams))
	for _, runtime := range s.streams {
		runtimes = append(runtimes, runtime)
	}
	s.mu.Unlock()
	for _, runtime := range runtimes {
		runtime.mu.Lock()
		logDirectory := runtime.resource.LogDirectory
		knownFile := runtime.files[event.Name] != nil
		persistedStreamRef := runtime.persisted.StreamRef
		runtime.mu.Unlock()
		podDirectory := filepath.Dir(logDirectory)
		relevantDirectory := event.Name == logDirectory || event.Name == podDirectory
		relevantFile := filepath.Dir(event.Name) == logDirectory && isRecognizedLogArtifact(event.Name)
		if (!relevantDirectory && !relevantFile) || knownFile {
			continue
		}
		checkpointPath := strings.TrimSuffix(event.Name, ".gz")
		if relevantFile && isRotatedLog(checkpointPath) {
			checkpoint, found, err := s.state.GetFileCheckpoint(persistedStreamRef, checkpointPath)
			if err != nil {
				return err
			}
			if found && checkpoint.Offset >= checkpoint.ObservedSize {
				continue
			}
		}
		scope := fmt.Sprintf("unobserved-event:%s:%s", event.Op.String(), event.Name)
		if err := s.recordWatchDiscontinuity(runtime, scope); err != nil {
			return err
		}
	}
	return nil
}

func isRecognizedLogArtifact(path string) bool {
	name := strings.TrimSuffix(filepath.Base(path), ".gz")
	return logNamePattern.MatchString(name)
}

func (s *containerLogSource) ensureResourceWatches(watcher *fsnotify.Watcher, logDirectory string) (bool, []string, error) {
	podDirectory := filepath.Dir(logDirectory)
	logRoot := filepath.Dir(podDirectory)
	paths := []string{logRoot, podDirectory, logDirectory}
	leafWatched := false
	var discontinuities []string
	var watchErr error
	for index, path := range paths {
		present, discontinuity, err := s.ensureWatch(watcher, path)
		if discontinuity != "" {
			discontinuities = append(discontinuities, discontinuity)
		}
		if index == 0 && !present {
			if discontinuity == "" {
				discontinuities = append(discontinuities, "watch-unavailable:"+logRoot)
			}
		}
		if index == len(paths)-1 {
			leafWatched = present
		}
		if err != nil {
			watchErr = errors.Join(watchErr, err)
		}
	}
	return leafWatched, discontinuities, watchErr
}

func canonicalCoverageBoundary(now time.Time) time.Time {
	now = now.UTC()
	if now.Nanosecond() == 0 {
		return now
	}
	return now.Truncate(time.Second).Add(time.Second)
}

func (s *containerLogSource) ensureWatch(watcher *fsnotify.Watcher, path string) (bool, string, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if _, seen := s.watches[path]; seen {
				return false, "watch-unavailable:" + path, nil
			}
			return false, "", nil
		}
		return false, "watch-stat-failed:" + path, fmt.Errorf("stat watch path %q: %w", path, err)
	}
	device, inode, err := sourceFileIdentity(info)
	if err != nil {
		return false, "watch-identity-failed:" + path, err
	}
	identity := watchIdentity{device: device, inode: inode}
	previous, seen := s.watches[path]
	discontinuity := ""
	if seen && previous != identity {
		discontinuity = fmt.Sprintf("watch-replaced:%s:%d:%d:%d:%d", path, previous.device, previous.inode, identity.device, identity.inode)
	}
	if err := watcher.Add(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, "watch-add-raced:" + path, nil
		}
		return false, "watch-add-failed:" + path, fmt.Errorf("watch %q: %w", path, err)
	}
	s.watches[path] = identity
	return true, discontinuity, nil
}

func (s *containerLogSource) resumeMonitoring(runtime *streamRuntime, leafWatched bool, discontinuities []string) error {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	next := runtime.persisted
	changed := false
	hadCoverageBoundary := !next.CoverageStartedAt.IsZero()
	if next.CoverageStartedAt.IsZero() {
		if !leafWatched {
			return nil
		}
		hadPriorProgress := len(runtime.files) > 0 || next.Revision > 1 || next.AcknowledgedRevision > 0 || next.FinalizingRevision > 0 || len(next.Drops) > 0 || len(next.Gaps) > 0
		if hadPriorProgress {
			return api.Permanent(fmt.Errorf("source stream %q has progress without a coverage boundary", next.StreamRef))
		}
		next.CoverageStartedAt = canonicalCoverageBoundary(time.Now())
		next.MonitoringEpoch = s.epochID
		changed = true
	} else if next.MonitoringEpoch != s.epochID {
		reason := "monitor-interrupted"
		if !next.InitialScanComplete {
			reason = "adoption-scan-interrupted"
		}
		appendCoverageGapToStream(&next, next.MonitoringEpoch, reason, runtime.resource.LogDirectory)
		next.MonitoringEpoch = s.epochID
		changed = true
	}
	if hadCoverageBoundary {
		for _, scope := range discontinuities {
			if scope != "" && appendCoverageGapToStream(&next, s.epochID+":"+scope, "watch-discontinuity", runtime.resource.LogDirectory) {
				changed = true
			}
		}
	}
	if !changed {
		return nil
	}
	if err := s.state.PutSourceStream(next); err != nil {
		return err
	}
	runtime.persisted = next
	runtime.outcome = outcomeFromState(next)
	return nil
}

func (s *containerLogSource) removeResourceWatches(watcher *fsnotify.Watcher, logDirectory string) error {
	var removeErr error
	for _, path := range []string{logDirectory, filepath.Dir(logDirectory)} {
		if _, tracked := s.watches[path]; tracked {
			if err := watcher.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, fsnotify.ErrNonExistentWatch) {
				removeErr = errors.Join(removeErr, fmt.Errorf("remove watch %q: %w", path, err))
			}
		}
		delete(s.watches, path)
	}
	return removeErr
}

func (s *containerLogSource) completeInitialScan(runtime *streamRuntime, leafWatched bool) error {
	if !leafWatched {
		return nil
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.persisted.InitialScanComplete {
		return nil
	}
	next := runtime.persisted
	next.InitialScanComplete = true
	if err := s.state.PutSourceStream(next); err != nil {
		return err
	}
	runtime.persisted = next
	return nil
}

func (s *containerLogSource) recordWatchDiscontinuityAll(scope string) error {
	s.mu.Lock()
	runtimes := make([]*streamRuntime, 0, len(s.streams))
	for _, runtime := range s.streams {
		runtimes = append(runtimes, runtime)
	}
	s.mu.Unlock()
	for _, runtime := range runtimes {
		if err := s.recordWatchDiscontinuity(runtime, scope); err != nil {
			return err
		}
	}
	return nil
}

func (s *containerLogSource) recordSharedRootWatchReplacements(logDirectory string, discontinuities []string) error {
	logRoot := filepath.Dir(filepath.Dir(logDirectory))
	replacementPrefix := "watch-replaced:" + logRoot + ":"
	s.mu.Lock()
	for _, scope := range discontinuities {
		if strings.HasPrefix(scope, replacementPrefix) {
			pending := s.pendingRootWatchReplacements[logRoot]
			if pending == nil {
				pending = make(map[string]struct{})
				s.pendingRootWatchReplacements[logRoot] = pending
			}
			pending[scope] = struct{}{}
		}
	}
	pending := s.pendingRootWatchReplacements[logRoot]
	replacementScopes := make([]string, 0, len(pending))
	for scope := range pending {
		replacementScopes = append(replacementScopes, scope)
	}
	sort.Strings(replacementScopes)
	if len(replacementScopes) == 0 {
		s.mu.Unlock()
		return nil
	}
	runtimes := make([]*streamRuntime, 0, len(s.streams))
	for _, runtime := range s.streams {
		runtimes = append(runtimes, runtime)
	}
	s.mu.Unlock()
	var recordErr error
	for _, runtime := range runtimes {
		runtime.mu.Lock()
		sharedRoot := filepath.Dir(filepath.Dir(runtime.resource.LogDirectory)) == logRoot
		runtime.mu.Unlock()
		if !sharedRoot {
			continue
		}
		for _, scope := range replacementScopes {
			if err := s.recordWatchDiscontinuity(runtime, scope); err != nil {
				recordErr = errors.Join(recordErr, err)
			}
		}
	}
	if recordErr != nil {
		return recordErr
	}
	s.mu.Lock()
	pending = s.pendingRootWatchReplacements[logRoot]
	for _, scope := range replacementScopes {
		delete(pending, scope)
	}
	if len(pending) == 0 {
		delete(s.pendingRootWatchReplacements, logRoot)
	}
	s.mu.Unlock()
	return nil
}

func (s *containerLogSource) recordWatchDiscontinuity(runtime *streamRuntime, scope string) error {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.persisted.CoverageStartedAt.IsZero() {
		return nil
	}
	next := runtime.persisted
	if !appendCoverageGapToStream(&next, s.epochID+":"+scope, "watch-discontinuity", runtime.resource.LogDirectory) {
		return nil
	}
	if err := s.state.PutSourceStream(next); err != nil {
		return err
	}
	runtime.persisted = next
	runtime.outcome = outcomeFromState(next)
	return nil
}

func (s *containerLogSource) drainWatcherSignals(watcher *fsnotify.Watcher, logDirectory string) (bool, error) {
	hadRelevantActivity := false
	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				if err := s.recordWatchDiscontinuityAll("event-channel-closed"); err != nil {
					return true, err
				}
				return true, errors.New("fsnotify event channel closed")
			}
			if watchEventRelevantToStream(logDirectory, event.Name) {
				hadRelevantActivity = true
			}
			if err := s.recordUnobservedWatchEvent(event); err != nil {
				return true, err
			}
		case watchErr, ok := <-watcher.Errors:
			if !ok {
				if err := s.recordWatchDiscontinuityAll("error-channel-closed"); err != nil {
					return true, err
				}
				return true, errors.New("fsnotify error channel closed")
			}
			if watchErr != nil {
				if err := s.recordWatchDiscontinuityAll("watch-error-" + uuid.NewString()); err != nil {
					return true, err
				}
				return true, fmt.Errorf("fsnotify: %w", watchErr)
			}
		default:
			return hadRelevantActivity, nil
		}
	}
}

func watchEventRelevantToStream(logDirectory, eventName string) bool {
	if eventName == "" {
		return false
	}
	logDirectory = filepath.Clean(logDirectory)
	eventName = filepath.Clean(eventName)
	podDirectory := filepath.Dir(logDirectory)
	logRoot := filepath.Dir(podDirectory)
	return eventName == logRoot || eventName == podDirectory || eventName == logDirectory || strings.HasPrefix(eventName, logDirectory+string(filepath.Separator))
}

func (s *containerLogSource) scanAll(ctx context.Context, watcher *fsnotify.Watcher) error {
	resources := s.allResources()
	if len(resources) > 1 {
		start := s.scanCursor % len(resources)
		s.scanCursor++
		resources = append(resources[start:], resources[:start]...)
	}
	for _, resource := range resources {
		streamRef := streamRef(resource)
		runtime, err := s.getOrCreateRuntime(streamRef, resource)
		if err != nil {
			return err
		}
		pruned, err := s.pruneEndedRuntime(streamRef, runtime, time.Now())
		if err != nil {
			return err
		}
		if pruned {
			if err := s.removeResourceWatches(watcher, runtime.resource.LogDirectory); err != nil {
				s.log.Warnf("remove resource watches: %v", err)
			}
			continue
		}
		leafWatched, discontinuities, watchErr := s.ensureResourceWatches(watcher, resource.LogDirectory)
		if err := s.recordSharedRootWatchReplacements(resource.LogDirectory, discontinuities); err != nil {
			return err
		}
		if err := s.resumeMonitoring(runtime, leafWatched, discontinuities); err != nil {
			return err
		}
		if watchErr != nil {
			return watchErr
		}
		files, compressed, err := discoverDirectory(runtime.resource.LogDirectory)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				files = nil
				compressed = nil
			} else {
				return err
			}
		}
		runtime.mu.Lock()
		hasNewCompressed, err := s.runtimeHasNewCompressed(runtime, compressed)
		if err != nil {
			runtime.mu.Unlock()
			return err
		}
		hasNewBytes, err := s.runtimeHasNewBytes(runtime, files)
		if err != nil {
			runtime.mu.Unlock()
			return err
		}
		hasObservedLate := hasNewBytes || hasNewCompressed
		lostLate := runtime.latePending && !hasObservedLate
		hasLate := hasObservedLate || runtime.latePending
		if lostLate && !runtime.ended && runtime.persisted.FinalizingRevision == 0 {
			next := runtime.persisted
			checkpoint := state.FileCheckpoint{StreamRef: streamRef.ID, Path: runtime.resource.LogDirectory}
			appendGapToStream(&next, checkpoint, "late-after-finalize", false)
			next.LatePending = false
			if err := s.state.PutSourceStream(next); err != nil {
				runtime.mu.Unlock()
				return err
			}
			runtime.persisted = next
			runtime.latePending = false
			runtime.outcome = outcomeFromState(next)
		}
		if runtime.ended && hasLate {
			if !runtime.endAcked {
				next := runtime.persisted
				next.LatePending = true
				if !runtime.latePending {
					if err := s.state.PutSourceStream(next); err != nil {
						runtime.mu.Unlock()
						return err
					}
					runtime.persisted = next
					runtime.latePending = true
				}
				runtime.mu.Unlock()
				continue
			}
			if runtime.persisted.RepairDeadline != nil && time.Now().After(*runtime.persisted.RepairDeadline) {
				runtime.mu.Unlock()
				s.log.Warnf("ignoring bytes after repair deadline stream=%s", streamRef.ID)
				continue
			}
			next := runtime.persisted
			next.Revision++
			next.Ended = false
			next.LatePending = false
			if lostLate {
				checkpoint := state.FileCheckpoint{StreamRef: streamRef.ID, Path: runtime.resource.LogDirectory}
				appendGapToStream(&next, checkpoint, "late-after-finalize", false)
			}
			if err := s.state.PutSourceStream(next); err != nil {
				runtime.mu.Unlock()
				return err
			}
			runtime.persisted = next
			runtime.revision = next.Revision
			runtime.ended = false
			runtime.endAcked = false
			runtime.latePending = false
			runtime.stableEOF = 0
			runtime.outcome = outcomeFromState(next)
		}
		stillEnded := runtime.ended
		runtime.mu.Unlock()
		if stillEnded {
			continue
		}
		if err := s.recordCompressed(runtime, compressed); err != nil {
			return err
		}
		changed := false
		for _, path := range files {
			read, err := s.scanFile(ctx, streamRef, runtime, path)
			if err != nil {
				return err
			}
			changed = changed || read
		}
		if err := s.reconcileMissingFiles(runtime, files); err != nil {
			return err
		}
		if err := s.completeInitialScan(runtime, leafWatched); err != nil {
			return err
		}
		runtime.mu.Lock()
		monitoringEstablished := !runtime.persisted.CoverageStartedAt.IsZero()
		if !monitoringEstablished || changed {
			runtime.stableEOF = 0
		} else {
			runtime.stableEOF++
		}
		shouldEnd := monitoringEstablished && runtime.resource.Terminated && runtime.stableEOF >= 2 && !runtime.ended
		runtime.mu.Unlock()
		if shouldEnd {
			hadWatchActivity, err := s.drainWatcherSignals(watcher, runtime.resource.LogDirectory)
			if err != nil {
				return err
			}
			if hadWatchActivity {
				runtime.mu.Lock()
				runtime.stableEOF = 0
				runtime.mu.Unlock()
				continue
			}
			if err := s.finishStream(ctx, streamRef, runtime); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *containerLogSource) pruneEndedRuntime(streamRef api.StreamRef, runtime *streamRuntime, now time.Time) (bool, error) {
	runtime.mu.Lock()
	eligible := runtime.ended && runtime.endAcked && runtime.persisted.RepairDeadline != nil && !now.Before(*runtime.persisted.RepairDeadline)
	logDirectory := runtime.resource.LogDirectory
	podUID := runtime.resource.PodUID
	runtime.mu.Unlock()
	if !eligible {
		return false, nil
	}
	if _, err := os.Stat(logDirectory); err == nil {
		return false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	s.store.Forget(podUID)
	if !s.cfg.PruneEndedState {
		s.mu.Lock()
		if s.streams[streamRef.ID] == runtime {
			delete(s.streams, streamRef.ID)
		}
		s.mu.Unlock()
		return true, nil
	}
	if err := s.state.DeleteStream(streamRef.ID); err != nil {
		return false, err
	}
	s.mu.Lock()
	if s.streams[streamRef.ID] == runtime {
		delete(s.streams, streamRef.ID)
	}
	s.mu.Unlock()
	return true, nil
}

func (s *containerLogSource) scanFile(ctx context.Context, streamRef api.StreamRef, runtime *streamRuntime, path string) (bool, error) {
	runtime.mu.Lock()
	fileState := runtime.files[path]
	runtime.mu.Unlock()
	if fileState == nil {
		checkpoint, found, err := s.findCheckpoint(streamRef.ID, path)
		if err != nil {
			return false, err
		}
		repairGapID := ""
		if !found {
			checkpoint, repairGapID, found, err = s.findRepairCheckpoint(runtime, path)
			if err != nil {
				return false, err
			}
		}
		var fingerprintValue fileFingerprint
		if found {
			fingerprintValue, err = fingerprintForCheckpoint(path, checkpoint)
		} else {
			fingerprintValue, err = fingerprint(path, -1)
		}
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return true, nil
			}
			return false, err
		}
		offset := int64(0)
		fileID := uuid.NewString()
		if found {
			if !checkpointMatchesFingerprint(checkpoint, fingerprintValue) {
				runtime.mu.Lock()
				if err := s.addGapLocked(runtime, checkpoint, "fingerprint-mismatch", false); err != nil {
					runtime.mu.Unlock()
					return false, err
				}
				runtime.mu.Unlock()
				found = false
				fingerprintValue, err = fingerprint(path, -1)
				if err != nil {
					return false, err
				}
			} else {
				offset = checkpoint.Offset
				fileID = checkpoint.FileID
				if fileID == "" {
					fileID = uuid.NewString()
				}
			}
		}
		info, err := os.Stat(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return true, nil
			}
			return false, err
		}
		if info.Size() < offset {
			if repairGapID != "" {
				return false, nil
			}
			runtime.mu.Lock()
			if err := s.addGapLocked(runtime, checkpoint, "file-reclaimed", false); err != nil {
				runtime.mu.Unlock()
				return false, err
			}
			runtime.mu.Unlock()
			offset = 0
			fileID = uuid.NewString()
			found = false
			fingerprintValue, err = fingerprint(path, -1)
			if err != nil {
				return false, err
			}
		} else if found && repairGapID == "" && info.Size() < checkpoint.ObservedSize {
			runtime.mu.Lock()
			if err := s.addGapLocked(runtime, checkpoint, "file-reclaimed", true); err != nil {
				runtime.mu.Unlock()
				return false, err
			}
			runtime.mu.Unlock()
		}
		runtime.mu.Lock()
		for _, existing := range runtime.files {
			if existing.fileID == fileID {
				fileState = existing
				break
			}
		}
		reused := fileState != nil
		if fileState == nil {
			fileState = &fileRuntime{fileID: fileID}
		}
		fileState.path = path
		fileState.restart = restartIdentity(path)
		if !reused {
			fileState.readOffset = offset
			fileState.committed = offset
		}
		fileState.observedSize = info.Size()
		fileState.modTimeUnixNano = info.ModTime().UnixNano()
		fileState.fingerprint = fingerprintValue
		if err := bindRepairGap(runtime.persisted, fileState); err != nil {
			runtime.mu.Unlock()
			return false, err
		}
		for existingPath, existing := range runtime.files {
			if existingPath != path && existing.fileID == fileID {
				delete(runtime.files, existingPath)
			}
		}
		runtime.files[path] = fileState
		checkpoint = checkpointFor(streamRef.ID, runtime.revision, fileState, fileState.committed)
		next := runtime.persisted
		if err := s.commitSource([]state.FileCheckpoint{checkpoint}, next); err != nil {
			delete(runtime.files, path)
			runtime.mu.Unlock()
			return false, err
		}
		runtime.mu.Unlock()
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// The directory snapshot may have raced with a rotation. Defer the
			// loss decision to reconciliation against a fresh snapshot.
			return true, nil
		}
		return false, err
	}
	defer f.Close()
	runtime.mu.Lock()
	storedFingerprint := fileState.fingerprint
	committed := fileState.committed
	readOffsetForIdentity := fileState.readOffset
	runtime.mu.Unlock()
	requestedHashBytes := storedFingerprint.HashBytes
	promotingFingerprint := requestedHashBytes == 0
	if promotingFingerprint {
		if committed != 0 || readOffsetForIdentity != 0 {
			return false, api.Permanent(fmt.Errorf("source file %q has a zero-length fingerprint past offset zero", path))
		}
		requestedHashBytes = -1
	}
	info, err := f.Stat()
	if err != nil {
		return false, err
	}
	if promotingFingerprint && info.Size() == 0 {
		// Do not race an empty-file identity snapshot with bytes appended
		// later in this scan. A subsequent fsnotify/reconcile pass promotes
		// the prefix fingerprint before reading any data.
		return false, nil
	}
	current, err := fingerprintFile(f, requestedHashBytes)
	if err != nil {
		return false, err
	}
	if promotingFingerprint && current.Device == storedFingerprint.Device && current.Inode == storedFingerprint.Inode {
		if current.HashBytes == 0 {
			return false, nil
		}
		runtime.mu.Lock()
		if runtime.files[path] != fileState || fileState.fingerprint != storedFingerprint || fileState.committed != 0 || fileState.readOffset != 0 {
			runtime.mu.Unlock()
			return false, api.Permanent(fmt.Errorf("source file %q changed while promoting its fingerprint", path))
		}
		fileState.fingerprint = current
		checkpoint := checkpointFor(runtime.persisted.StreamRef, runtime.revision, fileState, 0)
		if err := s.commitSource([]state.FileCheckpoint{checkpoint}, runtime.persisted); err != nil {
			fileState.fingerprint = storedFingerprint
			runtime.mu.Unlock()
			return false, err
		}
		runtime.mu.Unlock()
	} else if current != storedFingerprint {
		runtime.mu.Lock()
		if runtime.files[path] == fileState {
			delete(runtime.files, path)
		}
		runtime.mu.Unlock()
		return false, s.markRuntimeFileGap(runtime, fileState, "fingerprint-mismatch", false)
	}
	runtime.mu.Lock()
	if err := bindRepairGap(runtime.persisted, fileState); err != nil {
		runtime.mu.Unlock()
		return false, err
	}
	runtime.mu.Unlock()
	info, err = f.Stat()
	if err != nil {
		return false, err
	}
	runtime.mu.Lock()
	if info.Size() < fileState.readOffset {
		delete(runtime.files, path)
		runtime.mu.Unlock()
		return false, s.markRuntimeFileGap(runtime, fileState, "file-reclaimed", true)
	}
	if info.Size() < fileState.observedSize {
		if fileState.repairGapID != "" {
			if err := s.makeBoundRepairGapCoverageLocked(runtime, fileState); err != nil {
				runtime.mu.Unlock()
				return false, err
			}
		} else {
			checkpoint := checkpointFor(runtime.persisted.StreamRef, runtime.revision, fileState, fileState.committed)
			if err := s.addGapLocked(runtime, checkpoint, "file-reclaimed", true); err != nil {
				runtime.mu.Unlock()
				return false, err
			}
		}
		if err := bindRepairGap(runtime.persisted, fileState); err != nil {
			runtime.mu.Unlock()
			return false, err
		}
	}
	fileState.observedSize = info.Size()
	fileState.modTimeUnixNano = info.ModTime().UnixNano()
	if err := s.commitCompletedLocked(runtime); err != nil {
		runtime.mu.Unlock()
		return false, err
	}
	readOffset := fileState.readOffset
	runtime.mu.Unlock()
	if _, err := f.Seek(readOffset, io.SeekStart); err != nil {
		return false, err
	}
	reader := bufio.NewReader(f)
	changed := false
	malformedLines := 0
	var malformedStart, malformedEnd int64
	for {
		start := readOffset
		raw, consumed, complete, done, err := readPhysicalLineForScan(reader, s.cfg.MaxLineBytes+512)
		if err != nil {
			return false, fmt.Errorf("read source file %q at offset %d: %w", path, start, err)
		}
		if done {
			break
		}
		if !complete {
			break
		}
		readOffset += consumed
		changed = true
		line, parseErr := parseCRILine(raw)
		span := sourceSpan{FileID: fileState.fileID, Path: path, StartOffset: start, EndOffset: readOffset, Device: fileState.fingerprint.Device, Inode: fileState.fingerprint.Inode, PrefixHash: fileState.fingerprint.PrefixHash, HashBytes: fileState.fingerprint.HashBytes, RepairGapID: fileState.repairGapID, fingerprint: fileState.fingerprint, restart: fileState.restart}
		flushedMalformedLines := 0
		runtime.mu.Lock()
		fileState.readOffset = readOffset
		if parseErr != nil {
			appendCompletedDrop(runtime, fileState, span, "malformed-cri")
			if malformedLines == 0 {
				malformedStart = start
			}
			malformedLines++
			malformedEnd = readOffset
			runtime.mu.Unlock()
			continue
		}
		if malformedLines > 0 {
			if err := s.commitCompletedLocked(runtime); err != nil {
				fileState.readOffset = start
				runtime.mu.Unlock()
				return false, err
			}
			flushedMalformedLines = malformedLines
			malformedLines = 0
		}
		runtime.pending = append(runtime.pending, &pendingSpan{span: span, file: fileState})
		record := runtime.assembler.consume(line, span, time.Now())
		runtime.mu.Unlock()
		if flushedMalformedLines > 0 {
			s.log.Warnf("dropping malformed CRI bytes path=%s start=%d end=%d lines=%d", path, malformedStart, malformedEnd, flushedMalformedLines)
		}
		if record != nil {
			if err := s.emit(ctx, streamRef, runtime, *record); err != nil {
				return false, err
			}
		}
	}
	if malformedLines > 0 {
		runtime.mu.Lock()
		if err := s.commitCompletedLocked(runtime); err != nil {
			runtime.mu.Unlock()
			return false, err
		}
		runtime.mu.Unlock()
		s.log.Warnf("dropping malformed CRI bytes path=%s start=%d end=%d lines=%d", path, malformedStart, malformedEnd, malformedLines)
	}
	return changed, nil
}

func appendCompletedDrop(runtime *streamRuntime, file *fileRuntime, span sourceSpan, reason string) {
	if len(runtime.pending) > 0 {
		last := runtime.pending[len(runtime.pending)-1]
		if last.file == file && last.tokenID == "" && last.complete && last.span.EndOffset == span.StartOffset &&
			last.span.Path == span.Path && sameFingerprint(last.span.fingerprint, span) &&
			len(last.dropReasons) == 1 && last.dropReasons[0] == reason {
			last.span.EndOffset = span.EndOffset
			return
		}
	}
	runtime.pending = append(runtime.pending, &pendingSpan{span: span, file: file, complete: true, dropReasons: []string{reason}})
}

func readPhysicalLine(reader *bufio.Reader, retainLimit int) ([]byte, int64, bool, error) {
	if retainLimit < 1 {
		retainLimit = 1
	}
	retained := make([]byte, 0, min(retainLimit, 4096))
	var consumed int64
	for {
		fragment, err := reader.ReadSlice('\n')
		consumed += int64(len(fragment))
		if remaining := retainLimit - len(retained); remaining > 0 {
			if len(fragment) > remaining {
				fragment = fragment[:remaining]
			}
			retained = append(retained, fragment...)
		}
		if err == nil {
			if len(retained) == 0 || retained[len(retained)-1] != '\n' {
				retained = append(retained, '\n')
			}
			return retained, consumed, true, nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return retained, consumed, false, err
	}
}

func readPhysicalLineForScan(reader *bufio.Reader, retainLimit int) ([]byte, int64, bool, bool, error) {
	raw, consumed, complete, err := readPhysicalLine(reader, retainLimit)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, 0, false, false, err
	}
	if consumed == 0 && errors.Is(err, io.EOF) {
		return nil, 0, false, true, nil
	}
	return raw, consumed, complete, false, nil
}

func (s *containerLogSource) flushExpired(ctx context.Context, now time.Time) {
	type expiredRecord struct {
		streamRef api.StreamRef
		runtime   *streamRuntime
		record    assembled
	}
	var expired []expiredRecord
	s.mu.Lock()
	for id, runtime := range s.streams {
		runtime.mu.Lock()
		for _, record := range runtime.assembler.expired(now) {
			expired = append(expired, expiredRecord{streamRef: api.StreamRef{ID: id, Kind: api.RecordKindContainerLog}, runtime: runtime, record: record})
		}
		runtime.mu.Unlock()
	}
	s.mu.Unlock()
	for _, item := range expired {
		if err := s.emit(ctx, item.streamRef, item.runtime, item.record); err != nil {
			s.fail(err)
		}
	}
}

func (s *containerLogSource) emit(ctx context.Context, streamRef api.StreamRef, runtime *streamRuntime, record assembled) error {
	runtime.mu.Lock()
	value, err := json.Marshal(tokenValue{Spans: record.spans, DropReasons: record.dropReasons, Revision: runtime.revision})
	if err != nil {
		runtime.mu.Unlock()
		return err
	}
	recordID := stableRecordID(streamRef.ID, record.spans)
	assigned := 0
	for _, pending := range runtime.pending {
		if pending.tokenID != "" {
			continue
		}
		for _, span := range record.spans {
			if pending.span.FileID == span.FileID && pending.span.Path == span.Path && pending.span.StartOffset >= span.StartOffset && pending.span.EndOffset <= span.EndOffset {
				pending.tokenID = recordID
				assigned++
				break
			}
		}
	}
	if assigned == 0 {
		runtime.mu.Unlock()
		return errors.New("assembled record has no pending source spans")
	}
	for _, reason := range record.dropReasons {
		runtime.outcome.HadDrops = true
		runtime.outcome.LossReasons = addReason(runtime.outcome.LossReasons, reason)
	}
	resource := runtime.resource.Resource
	metadata := streamMetadata(runtime.resource)
	runtime.mu.Unlock()
	event := api.SourceEvent{Delivery: &api.Delivery{
		Record:    api.Record{Kind: api.RecordKindContainerLog, Timestamp: record.timestamp, Body: record.body, Resource: resource, Attributes: map[string]string{"source": "container." + record.stream, "stream": record.stream, "log.file.path": record.spans[len(record.spans)-1].Path}},
		StreamRef: streamRef,
		Metadata:  metadata,
		AckToken:  api.AckToken{ID: recordID, Source: sourceName, StreamRef: streamRef, Value: value},
		RecordID:  recordID,
	}}
	select {
	case s.out <- event:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *containerLogSource) emitEnd(ctx context.Context, streamRef api.StreamRef, runtime *streamRuntime) error {
	runtime.mu.Lock()
	value := []byte(strconv.FormatUint(runtime.revision, 10))
	id := endTokenID(streamRef.ID, value)
	next := runtime.persisted
	replaying := next.FinalizingRevision == runtime.revision
	next.FinalizingRevision = runtime.revision
	next.Ended = false
	if !replaying {
		next.LatePending = false
		next.FinalizingOutcome = &state.OutcomeSnapshot{HadDrops: runtime.outcome.HadDrops, HadSourceGaps: runtime.outcome.HadSourceGaps, LossReasons: append([]string(nil), runtime.outcome.LossReasons...)}
	} else if next.FinalizingOutcome == nil {
		runtime.mu.Unlock()
		return api.Permanent(errors.New("finalizing source stream has no frozen outcome"))
	}
	if err := s.state.PutSourceStream(next); err != nil {
		runtime.mu.Unlock()
		return err
	}
	runtime.persisted = next
	if !replaying {
		runtime.latePending = false
	}
	frozenOutcome := api.SourceOutcome{HadDrops: next.FinalizingOutcome.HadDrops, HadSourceGaps: next.FinalizingOutcome.HadSourceGaps, LossReasons: append([]string(nil), next.FinalizingOutcome.LossReasons...)}
	event := api.SourceEvent{End: &api.StreamEnd{StreamRef: streamRef, EndToken: api.EndToken{ID: id, Source: sourceName, StreamRef: streamRef, Value: value}, Revision: runtime.revision, CoverageStartedAt: next.CoverageStartedAt, Resource: runtime.resource.Resource, Metadata: streamMetadata(runtime.resource), Outcome: frozenOutcome}}
	runtime.ended = true
	runtime.endAcked = false
	runtime.mu.Unlock()
	select {
	case s.out <- event:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func endTokenID(streamRef string, value []byte) string {
	hash := sha256.Sum256(append([]byte(streamRef), value...))
	return hex.EncodeToString(hash[:])
}

func (s *containerLogSource) finishStream(ctx context.Context, streamRef api.StreamRef, runtime *streamRuntime) error {
	runtime.mu.Lock()
	for _, file := range runtime.files {
		if file.observedSize > file.readOffset {
			if err := s.coverGapLocked(runtime, file, file.readOffset, file.observedSize, "unterminated-cri-tail"); err != nil {
				runtime.mu.Unlock()
				return err
			}
		}
	}
	records := runtime.assembler.finish()
	runtime.mu.Unlock()
	for _, record := range records {
		if err := s.emit(ctx, streamRef, runtime, record); err != nil {
			return err
		}
	}
	// Persist finalization only after every preceding delivery is committed.
	// This makes a persisted FinalizingRevision safe to replay before scanning
	// for bytes that arrived after the terminal boundary.
	runtime.mu.Lock()
	hasPending := len(runtime.pending) != 0
	if hasPending {
		runtime.mu.Unlock()
		return nil
	}
	if err := s.resolveStableRepairGapsLocked(runtime); err != nil {
		runtime.mu.Unlock()
		return err
	}
	runtime.mu.Unlock()
	return s.emitEnd(ctx, streamRef, runtime)
}

func (s *containerLogSource) restoreStreams(ctx context.Context, watcher *fsnotify.Watcher) error {
	streams, err := s.state.ListSourceStreams()
	if err != nil {
		return err
	}
	for _, listed := range streams {
		streamRef := api.StreamRef{ID: listed.StreamRef, Kind: api.RecordKindContainerLog}
		_, resource, runtime, err := s.restoreRuntime(listed.StreamRef)
		if err != nil {
			return err
		}
		leafWatched, discontinuities, watchErr := s.ensureResourceWatches(watcher, resource.LogDirectory)
		if err := s.recordSharedRootWatchReplacements(resource.LogDirectory, discontinuities); err != nil {
			return err
		}
		if err := s.resumeMonitoring(runtime, leafWatched, discontinuities); err != nil {
			return err
		}
		runtime.mu.Lock()
		shouldReplayEnd := runtime.persisted.FinalizingRevision == runtime.revision && runtime.persisted.AcknowledgedRevision < runtime.revision
		runtime.mu.Unlock()
		if shouldReplayEnd {
			if err := s.emitEnd(ctx, streamRef, runtime); err != nil {
				return err
			}
			if watchErr != nil {
				s.fail(watchErr)
			}
			continue
		}
		if watchErr != nil {
			return watchErr
		}
	}
	return nil
}

// restoreRuntime re-reads the stream while holding the runtime map lock. This
// orders startup recovery against AcknowledgeEnd's persistent fallback, so a
// stale ListSourceStreams snapshot cannot resurrect an unacknowledged runtime.
func (s *containerLogSource) restoreRuntime(streamID string) (state.SourceStream, streamResource, *streamRuntime, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	persisted, found, err := s.state.GetSourceStream(streamID)
	if err != nil {
		return state.SourceStream{}, streamResource{}, nil, err
	}
	if !found {
		return state.SourceStream{}, streamResource{}, nil, api.Permanent(fmt.Errorf("persisted stream %q disappeared during restore", streamID))
	}
	if existing := s.streams[persisted.StreamRef]; existing != nil {
		existing.mu.Lock()
		current := existing.persisted
		resource := existing.resource
		existing.mu.Unlock()
		return current, resource, existing, nil
	}

	if persisted.Revision == 0 {
		return state.SourceStream{}, streamResource{}, nil, api.Permanent(fmt.Errorf("persisted stream %q has revision zero", persisted.StreamRef))
	}
	resource := thawResource(persisted.Resource)
	expectedStreamRef := streamRef(resource).ID
	if persisted.StreamRef != expectedStreamRef {
		return state.SourceStream{}, streamResource{}, nil, api.Permanent(fmt.Errorf("persisted stream_ref %q does not match resource-derived stream_ref %q", persisted.StreamRef, expectedStreamRef))
	}
	if _, present := s.store.GetByUID(resource.PodUID); !present && !resource.Terminated {
		resource.Terminated = true
		persisted.Resource.Terminated = true
		if err := s.state.PutSourceStream(persisted); err != nil {
			return state.SourceStream{}, streamResource{}, nil, err
		}
	}
	runtime := runtimeFromState(s.cfg, resource, persisted)
	if err := s.hydrateFiles(runtime); err != nil {
		return state.SourceStream{}, streamResource{}, nil, err
	}
	s.streams[persisted.StreamRef] = runtime
	return persisted, resource, runtime, nil
}

func (s *containerLogSource) allResources() []streamResource {
	byStream := make(map[string]streamResource)
	for _, resource := range s.store.List() {
		resolved := streamResource{
			Resource:     resource.Resource,
			Terminated:   resource.Terminated,
			LogDirectory: filepath.Join(s.cfg.LogRoot, fmt.Sprintf("%s_%s_%s", resource.Namespace, resource.PodName, resource.PodUID), resource.Container),
		}
		byStream[streamRef(resolved).ID] = resolved
	}
	s.mu.Lock()
	for id, runtime := range s.streams {
		runtime.mu.Lock()
		if _, exists := byStream[id]; !exists {
			byStream[id] = runtime.resource
		}
		runtime.mu.Unlock()
	}
	s.mu.Unlock()
	out := make([]streamResource, 0, len(byStream))
	for _, resource := range byStream {
		out = append(out, resource)
	}
	sort.Slice(out, func(i, j int) bool { return streamRef(out[i]).ID < streamRef(out[j]).ID })
	return out
}

func (s *containerLogSource) getOrCreateRuntime(streamRef api.StreamRef, resource streamResource) (*streamRuntime, error) {
	s.mu.Lock()
	runtime := s.streams[streamRef.ID]
	s.mu.Unlock()
	if runtime != nil {
		runtime.mu.Lock()
		if runtime.resource.PodUID != resource.PodUID || runtime.resource.LogDirectory != resource.LogDirectory || runtime.resource.SandboxID != resource.SandboxID {
			runtime.mu.Unlock()
			return nil, fmt.Errorf("resource identity changed for stream %s", streamRef.ID)
		}
		if resource.Terminated && !runtime.resource.Terminated {
			next := runtime.persisted
			next.Resource.Terminated = true
			if err := s.state.PutSourceStream(next); err != nil {
				runtime.mu.Unlock()
				return nil, err
			}
			runtime.persisted = next
			runtime.resource.Terminated = true
		}
		runtime.mu.Unlock()
		return runtime, nil
	}
	persisted, found, err := s.state.GetSourceStream(streamRef.ID)
	if err != nil {
		return nil, err
	}
	if found {
		if persisted.Revision == 0 {
			return nil, api.Permanent(fmt.Errorf("persisted stream %q has revision zero", streamRef.ID))
		}
		frozen := thawResource(persisted.Resource)
		if frozen.PodUID != resource.PodUID || frozen.LogDirectory != resource.LogDirectory || frozen.SandboxID != resource.SandboxID {
			return nil, fmt.Errorf("persisted resource identity changed for stream %s", streamRef.ID)
		}
		resource = frozen
	} else {
		persisted = state.SourceStream{StreamRef: streamRef.ID, Resource: freezeResource(resource), Revision: 1, LossReasons: []string{}}
		if err := s.state.PutSourceStream(persisted); err != nil {
			return nil, err
		}
	}
	runtime = runtimeFromState(s.cfg, resource, persisted)
	if err := s.hydrateFiles(runtime); err != nil {
		return nil, err
	}
	s.mu.Lock()
	if existing := s.streams[streamRef.ID]; existing != nil {
		runtime = existing
	} else {
		s.streams[streamRef.ID] = runtime
	}
	s.mu.Unlock()
	return runtime, nil
}

func (s *containerLogSource) hydrateFiles(runtime *streamRuntime) error {
	checkpoints, err := s.state.ListFileCheckpoints(runtime.persisted.StreamRef)
	if err != nil {
		return err
	}
	for _, checkpoint := range checkpoints {
		if err := validateCheckpointFingerprint(checkpoint); err != nil {
			return err
		}
		runtime.files[checkpoint.Path] = &fileRuntime{fileID: checkpoint.FileID, path: checkpoint.Path, restart: restartIdentity(checkpoint.Path), readOffset: checkpoint.Offset, committed: checkpoint.Offset, observedSize: checkpoint.ObservedSize, modTimeUnixNano: checkpoint.ModTimeUnixNano, fingerprint: fileFingerprint{Device: checkpoint.Device, Inode: checkpoint.Inode, PrefixHash: checkpoint.PrefixHash, HashBytes: checkpoint.HashBytes}}
	}
	return nil
}

func runtimeFromState(cfg sourceConfig, resource streamResource, persisted state.SourceStream) *streamRuntime {
	return &streamRuntime{
		resource:    resource,
		files:       make(map[string]*fileRuntime),
		assembler:   newAssembler(cfg.MaxLineBytes, cfg.PartialTimeout),
		revision:    persisted.Revision,
		ended:       persisted.Ended && persisted.AcknowledgedRevision >= persisted.Revision,
		endAcked:    persisted.AcknowledgedRevision >= persisted.Revision,
		latePending: persisted.LatePending,
		outcome:     outcomeFromState(persisted),
		persisted:   persisted,
	}
}

func freezeResource(resource streamResource) state.FrozenResource {
	return state.FrozenResource{SandboxID: resource.SandboxID, ClusterName: resource.ClusterName, Namespace: resource.Namespace, PodName: resource.PodName, PodUID: resource.PodUID, NodeName: resource.NodeName, Container: resource.Container, LogDirectory: resource.LogDirectory, Terminated: resource.Terminated}
}

func thawResource(resource state.FrozenResource) streamResource {
	return streamResource{Resource: api.Resource{SandboxID: resource.SandboxID, ClusterName: resource.ClusterName, Namespace: resource.Namespace, PodName: resource.PodName, PodUID: resource.PodUID, NodeName: resource.NodeName, Container: resource.Container}, Terminated: resource.Terminated, LogDirectory: resource.LogDirectory}
}

func streamMetadata(resource streamResource) api.StreamMetadata {
	return api.StreamMetadata{streamformat.ContainerLogDirectoryMetadata: resource.LogDirectory}
}

func (s *containerLogSource) runtimeHasNewBytes(runtime *streamRuntime, files []string) (bool, error) {
	for _, path := range files {
		info, err := os.Stat(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return false, err
		}
		file := runtime.files[path]
		if file == nil {
			if info.Size() == 0 {
				continue
			}
			checkpoint, found, err := s.findCheckpoint(runtime.persisted.StreamRef, path)
			if err != nil {
				return false, err
			}
			if found {
				fingerprintValue, err := fingerprintForCheckpoint(path, checkpoint)
				if err != nil {
					if errors.Is(err, os.ErrNotExist) {
						continue
					}
					return false, err
				}
				if checkpointMatchesFingerprint(checkpoint, fingerprintValue) && checkpoint.Offset >= info.Size() {
					continue
				}
			}
			return true, nil
		}
		if info.Size() != file.readOffset {
			return true, nil
		}
		fingerprintValue, err := fingerprint(path, file.fingerprint.HashBytes)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return false, err
		}
		if fingerprintValue != file.fingerprint {
			return true, nil
		}
	}
	return false, nil
}

func (s *containerLogSource) runtimeHasNewCompressed(runtime *streamRuntime, paths []string) (bool, error) {
	for _, path := range paths {
		id := gapID(runtime.persisted.StreamRef, state.FileCheckpoint{Path: path}, "preexisting-compressed-rotation")
		known := false
		for _, gap := range runtime.persisted.Gaps {
			if gap.ID == id || gap.Path == path {
				known = true
				break
			}
		}
		if !known {
			uncompressed := strings.TrimSuffix(path, ".gz")
			checkpoint, found, err := s.state.GetFileCheckpoint(runtime.persisted.StreamRef, uncompressed)
			if err != nil {
				return false, err
			}
			if !found {
				if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
					continue
				} else if err != nil {
					return false, err
				}
			}
			if !found || !isRotatedLog(uncompressed) || checkpoint.Offset < checkpoint.ObservedSize {
				return true, nil
			}
		}
	}
	return false, nil
}

func (s *containerLogSource) findCheckpoint(streamRef, path string) (state.FileCheckpoint, bool, error) {
	if checkpoint, found, err := s.state.GetFileCheckpoint(streamRef, path); err != nil || found {
		return checkpoint, found, err
	}
	checkpoints, err := s.state.ListFileCheckpoints(streamRef)
	if err != nil {
		return state.FileCheckpoint{}, false, err
	}
	var matches []state.FileCheckpoint
	for _, checkpoint := range checkpoints {
		fingerprintValue, err := fingerprintForCheckpoint(path, checkpoint)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return state.FileCheckpoint{}, false, nil
			}
			return state.FileCheckpoint{}, false, err
		}
		if checkpointMatchesFingerprint(checkpoint, fingerprintValue) {
			matches = append(matches, checkpoint)
		}
	}
	if len(matches) > 1 {
		return state.FileCheckpoint{}, false, fmt.Errorf("ambiguous checkpoint candidates for %s", path)
	}
	if len(matches) == 1 {
		return matches[0], true, nil
	}
	return state.FileCheckpoint{}, false, nil
}

func (s *containerLogSource) findRepairCheckpoint(runtime *streamRuntime, path string) (state.FileCheckpoint, string, bool, error) {
	runtime.mu.Lock()
	streamRef := runtime.persisted.StreamRef
	revision := runtime.revision
	gaps := append([]state.GapRecord(nil), runtime.persisted.Gaps...)
	runtime.mu.Unlock()
	type candidate struct {
		checkpoint state.FileCheckpoint
		gapID      string
	}
	var matches []candidate
	for _, gap := range gaps {
		if gap.Resolved || gap.Coverage || gap.ResumeAt != nil || gap.FileID == "" || gap.HashBytes <= 0 {
			continue
		}
		offset := gap.FromOffset
		if gap.RepairOffset != nil {
			offset = *gap.RepairOffset
		}
		if offset < gap.FromOffset || gap.ToOffset != nil && (offset > *gap.ToOffset || *gap.ToOffset < gap.FromOffset) {
			return state.FileCheckpoint{}, "", false, api.Permanent(fmt.Errorf("gap %q has invalid repair bounds", gap.ID))
		}
		fingerprintValue, err := fingerprint(path, gap.HashBytes)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return state.FileCheckpoint{}, "", false, nil
			}
			return state.FileCheckpoint{}, "", false, err
		}
		if gap.Device != fingerprintValue.Device || gap.Inode != fingerprintValue.Inode || gap.PrefixHash != fingerprintValue.PrefixHash || gap.HashBytes != fingerprintValue.HashBytes {
			continue
		}
		matches = append(matches, candidate{
			gapID: gap.ID,
			checkpoint: state.FileCheckpoint{
				StreamRef: streamRef, FileID: gap.FileID, Path: path, Offset: offset,
				Device: gap.Device, Inode: gap.Inode, PrefixHash: gap.PrefixHash, HashBytes: gap.HashBytes,
				ObservedSize: gap.ObservedSize, Revision: revision,
			},
		})
	}
	if len(matches) > 1 {
		return state.FileCheckpoint{}, "", false, api.Permanent(fmt.Errorf("ambiguous repair gap candidates for %s", path))
	}
	if len(matches) == 1 {
		return matches[0].checkpoint, matches[0].gapID, true, nil
	}
	return state.FileCheckpoint{}, "", false, nil
}

func checkpointFor(streamRef string, revision uint64, file *fileRuntime, offset int64) state.FileCheckpoint {
	return state.FileCheckpoint{StreamRef: streamRef, FileID: file.fileID, Path: file.path, Offset: offset, Device: file.fingerprint.Device, Inode: file.fingerprint.Inode, PrefixHash: file.fingerprint.PrefixHash, HashBytes: file.fingerprint.HashBytes, ObservedSize: file.observedSize, ModTimeUnixNano: file.modTimeUnixNano, Revision: revision}
}

func (s *containerLogSource) recordCompressed(runtime *streamRuntime, paths []string) error {
	for _, path := range paths {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		runtime.mu.Lock()
		uncompressed := strings.TrimSuffix(path, ".gz")
		checkpoint, found, err := s.findCheckpoint(runtime.persisted.StreamRef, uncompressed)
		if err != nil {
			runtime.mu.Unlock()
			return err
		}
		trackedRotatedPath := false
		if !found {
			checkpoint = state.FileCheckpoint{StreamRef: runtime.persisted.StreamRef, Path: path}
		} else {
			trackedRotatedPath = checkpoint.Path == uncompressed && isRotatedLog(uncompressed)
			checkpoint.Path = path
		}
		if found && runtime.hasPendingFile(checkpoint.FileID) {
			runtime.mu.Unlock()
			continue
		}
		if found && checkpoint.Offset >= checkpoint.ObservedSize && trackedRotatedPath {
			for runtimePath, file := range runtime.files {
				if file.fileID == checkpoint.FileID {
					delete(runtime.files, runtimePath)
				}
			}
			runtime.mu.Unlock()
			continue
		}
		reason := "preexisting-compressed-rotation"
		coverage := true
		if found {
			reason = "compressed-rotation"
			coverage = false
		}
		if err := s.addGapLocked(runtime, checkpoint, reason, coverage); err != nil {
			runtime.mu.Unlock()
			return err
		}
		if found {
			for runtimePath, file := range runtime.files {
				if file.fileID == checkpoint.FileID {
					delete(runtime.files, runtimePath)
				}
			}
		}
		runtime.mu.Unlock()
	}
	return nil
}

func (s *containerLogSource) reconcileMissingFiles(runtime *streamRuntime, discovered []string) error {
	seen := make(map[string]struct{}, len(discovered))
	for _, path := range discovered {
		seen[path] = struct{}{}
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.ended {
		return nil
	}
	var missing []*fileRuntime
	unique := make(map[string]struct{})
	for path, file := range runtime.files {
		if _, exists := seen[path]; exists || runtime.hasPendingFile(file.fileID) {
			continue
		}
		if _, exists := unique[file.fileID]; !exists {
			missing = append(missing, file)
			unique[file.fileID] = struct{}{}
		}
	}
	for _, file := range missing {
		coveredRotation := file.committed >= file.observedSize && isRotatedLog(file.path)
		if !coveredRotation {
			checkpoint := checkpointFor(runtime.persisted.StreamRef, runtime.revision, file, file.committed)
			if err := s.addGapLocked(runtime, checkpoint, "file-reclaimed", false); err != nil {
				return err
			}
		}
		for path, candidate := range runtime.files {
			if candidate.fileID == file.fileID {
				delete(runtime.files, path)
			}
		}
	}
	return nil
}

func isRotatedLog(path string) bool {
	match := logNamePattern.FindStringSubmatch(filepath.Base(path))
	return match != nil && match[2] != ""
}

func (r *streamRuntime) hasPendingFile(fileID string) bool {
	for _, pending := range r.pending {
		if pending.span.FileID == fileID {
			return true
		}
	}
	return false
}

func (s *containerLogSource) markRuntimeFileGap(runtime *streamRuntime, file *fileRuntime, reason string, coverage bool) error {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if coverage && file.repairGapID != "" {
		return s.makeBoundRepairGapCoverageLocked(runtime, file)
	}
	checkpoint := checkpointFor(runtime.persisted.StreamRef, runtime.revision, file, file.committed)
	return s.addGapLocked(runtime, checkpoint, reason, coverage)
}

func (s *containerLogSource) makeBoundRepairGapCoverageLocked(runtime *streamRuntime, file *fileRuntime) error {
	next := runtime.persisted
	next.LossReasons = append([]string(nil), runtime.persisted.LossReasons...)
	next.Gaps = append([]state.GapRecord(nil), runtime.persisted.Gaps...)
	found := false
	for index := range next.Gaps {
		gap := &next.Gaps[index]
		if gap.ID != file.repairGapID {
			continue
		}
		found = true
		gap.Coverage = true
		gap.Resolved = false
		gap.ToOffset = nil
		break
	}
	if !found {
		return api.Permanent(fmt.Errorf("repair file references unknown gap %q", file.repairGapID))
	}
	recomputeOutcome(&next)
	if err := s.state.PutSourceStream(next); err != nil {
		return err
	}
	runtime.persisted = next
	runtime.outcome = outcomeFromState(next)
	file.repairGapID = ""
	return nil
}

func (s *containerLogSource) addGapLocked(runtime *streamRuntime, checkpoint state.FileCheckpoint, reason string, coverage bool) error {
	next := runtime.persisted
	if !appendGapToStream(&next, checkpoint, reason, coverage) {
		return nil
	}
	if err := s.state.PutSourceStream(next); err != nil {
		return err
	}
	runtime.persisted = next
	runtime.outcome = outcomeFromState(next)
	return nil
}

func appendGapToStream(stream *state.SourceStream, checkpoint state.FileCheckpoint, reason string, coverage bool) bool {
	id := gapID(stream.StreamRef, checkpoint, reason)
	observedSize := max(checkpoint.ObservedSize, checkpoint.Offset)
	stream.LossReasons = append([]string(nil), stream.LossReasons...)
	stream.Gaps = append([]state.GapRecord(nil), stream.Gaps...)
	for index := range stream.Gaps {
		if stream.Gaps[index].ID == id {
			changed := false
			if stream.Gaps[index].ObservedSize < observedSize {
				stream.Gaps[index].ObservedSize = observedSize
				changed = true
			}
			if coverage && !stream.Gaps[index].Coverage {
				stream.Gaps[index].Coverage = true
				changed = true
			}
			if changed && stream.Gaps[index].Resolved {
				stream.Gaps[index].Resolved = false
				stream.Gaps[index].ToOffset = nil
			}
			if changed {
				recomputeOutcome(stream)
			}
			return changed
		}
	}
	stream.Gaps = append(stream.Gaps, state.GapRecord{ID: id, FileID: checkpoint.FileID, Path: checkpoint.Path, FromOffset: checkpoint.Offset, ObservedSize: observedSize, Device: checkpoint.Device, Inode: checkpoint.Inode, PrefixHash: checkpoint.PrefixHash, HashBytes: checkpoint.HashBytes, Reason: reason, Coverage: coverage})
	recomputeOutcome(stream)
	return true
}

func appendCoverageGapToStream(stream *state.SourceStream, scope, reason, path string) bool {
	id := coverageGapID(stream.StreamRef, scope, reason)
	stream.LossReasons = append([]string(nil), stream.LossReasons...)
	stream.Gaps = append([]state.GapRecord(nil), stream.Gaps...)
	for _, gap := range stream.Gaps {
		if gap.ID == id {
			return false
		}
	}
	stream.Gaps = append(stream.Gaps, state.GapRecord{ID: id, Path: path, Reason: reason, Coverage: true})
	recomputeOutcome(stream)
	return true
}

func (s *containerLogSource) coverGapLocked(runtime *streamRuntime, file *fileRuntime, from, resumeAt int64, reason string) error {
	checkpoint := checkpointFor(runtime.persisted.StreamRef, runtime.revision, file, from)
	id := gapID(runtime.persisted.StreamRef, checkpoint, reason)
	next := runtime.persisted
	next.LossReasons = append([]string(nil), runtime.persisted.LossReasons...)
	next.Gaps = append([]state.GapRecord(nil), runtime.persisted.Gaps...)
	found := false
	for _, gap := range next.Gaps {
		if gap.ID == id {
			found = true
			break
		}
	}
	if !found {
		resume := resumeAt
		next.Gaps = append(next.Gaps, state.GapRecord{ID: id, FileID: file.fileID, Path: file.path, FromOffset: from, ResumeAt: &resume, Device: file.fingerprint.Device, Inode: file.fingerprint.Inode, PrefixHash: file.fingerprint.PrefixHash, HashBytes: file.fingerprint.HashBytes, Reason: reason})
	}
	recomputeOutcome(&next)
	if err := s.state.PutSourceStream(next); err != nil {
		return err
	}
	span := sourceSpan{FileID: file.fileID, Path: file.path, StartOffset: from, EndOffset: resumeAt, Device: file.fingerprint.Device, Inode: file.fingerprint.Inode, PrefixHash: file.fingerprint.PrefixHash, HashBytes: file.fingerprint.HashBytes, fingerprint: file.fingerprint}
	runtime.pending = append(runtime.pending, &pendingSpan{span: span, file: file, complete: true})
	file.readOffset = resumeAt
	runtime.persisted = next
	runtime.outcome = outcomeFromState(next)
	return s.commitCompletedLocked(runtime)
}

func outcomeFromState(stream state.SourceStream) api.SourceOutcome {
	return api.SourceOutcome{HadDrops: stream.HadDrops, HadSourceGaps: stream.HadSourceGaps, LossReasons: append([]string(nil), stream.LossReasons...)}
}

func bindRepairGap(stream state.SourceStream, file *fileRuntime) error {
	file.repairGapID = ""
	candidate := ""
	for index := range stream.Gaps {
		gap := &stream.Gaps[index]
		if gap.Resolved || gap.Coverage || gap.ResumeAt != nil || gap.FileID != file.fileID {
			continue
		}
		expected := gap.FromOffset
		if gap.RepairOffset != nil {
			expected = *gap.RepairOffset
		}
		if expected < gap.FromOffset || gap.ToOffset != nil && (expected > *gap.ToOffset || *gap.ToOffset < gap.FromOffset) {
			return api.Permanent(fmt.Errorf("gap %q has invalid repair bounds", gap.ID))
		}
		if expected != file.committed {
			continue
		}
		if gap.Device == file.fingerprint.Device && gap.Inode == file.fingerprint.Inode && gap.PrefixHash == file.fingerprint.PrefixHash && gap.HashBytes == file.fingerprint.HashBytes {
			if candidate != "" {
				return api.Permanent(fmt.Errorf("multiple repair gaps match file_id %q at offset %d", file.fileID, file.committed))
			}
			candidate = gap.ID
		}
	}
	file.repairGapID = candidate
	return nil
}

func (s *containerLogSource) resolveStableRepairGapsLocked(runtime *streamRuntime) error {
	next := runtime.persisted
	next.LossReasons = append([]string(nil), runtime.persisted.LossReasons...)
	next.Gaps = append([]state.GapRecord(nil), runtime.persisted.Gaps...)
	changed := false
	for _, file := range runtime.files {
		if file.repairGapID == "" || file.readOffset != file.observedSize || file.committed != file.observedSize {
			continue
		}
		foundGap := false
		for index := range next.Gaps {
			gap := &next.Gaps[index]
			if gap.ID != file.repairGapID {
				continue
			}
			foundGap = true
			if gap.Resolved || gap.Coverage || gap.ResumeAt != nil || gap.FileID != file.fileID || gap.Device != file.fingerprint.Device || gap.Inode != file.fingerprint.Inode || gap.PrefixHash != file.fingerprint.PrefixHash || gap.HashBytes != file.fingerprint.HashBytes {
				return api.Permanent(fmt.Errorf("stable repair file does not match gap %q", gap.ID))
			}
			if file.observedSize < gap.FromOffset {
				return api.Permanent(fmt.Errorf("stable repair file for gap %q ends before the gap starts", gap.ID))
			}
			if file.observedSize < gap.ObservedSize {
				break
			}
			toOffset := file.observedSize
			if gap.ToOffset != nil && *gap.ToOffset != toOffset {
				return api.Permanent(fmt.Errorf("stable repair EOF for gap %q changed from %d to %d", gap.ID, *gap.ToOffset, toOffset))
			}
			gap.ToOffset = &toOffset
			repairOffset := gap.FromOffset
			if gap.RepairOffset != nil {
				repairOffset = *gap.RepairOffset
			}
			if repairOffset >= toOffset {
				gap.Resolved = true
				file.repairGapID = ""
			}
			changed = true
			break
		}
		if !foundGap {
			return api.Permanent(fmt.Errorf("stable repair file references unknown gap %q", file.repairGapID))
		}
	}
	if !changed {
		return nil
	}
	recomputeOutcome(&next)
	if err := s.state.PutSourceStream(next); err != nil {
		return err
	}
	runtime.persisted = next
	runtime.outcome = outcomeFromState(next)
	return nil
}

func recomputeOutcome(stream *state.SourceStream) {
	stream.HadSourceGaps = false
	allGapReasons := make(map[string]struct{})
	reasons := make(map[string]struct{})
	for _, gap := range stream.Gaps {
		allGapReasons[gap.Reason] = struct{}{}
		if !gap.Resolved {
			stream.HadSourceGaps = true
			reasons[gap.Reason] = struct{}{}
		}
	}
	for _, drop := range stream.Drops {
		reasons[drop.Reason] = struct{}{}
	}
	for _, reason := range stream.LossReasons {
		if _, belongsToGap := allGapReasons[reason]; !belongsToGap {
			reasons[reason] = struct{}{}
		}
	}
	ordered := make([]string, 0, len(reasons))
	for reason := range reasons {
		if reason != "" {
			ordered = append(ordered, reason)
		}
	}
	sort.Strings(ordered)
	stream.LossReasons = ordered
}

func appendDrop(drops []state.SourceDropRecord, drop state.SourceDropRecord) []state.SourceDropRecord {
	for _, existing := range drops {
		if existing.ID == drop.ID {
			return drops
		}
	}
	return append(drops, drop)
}

func spanResultID(streamRef string, span sourceSpan, reason string) string {
	value := fmt.Sprintf("%s\x00%s\x00%d\x00%d\x00%s", streamRef, span.FileID, span.StartOffset, span.EndOffset, reason)
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func stableRecordID(streamRef string, spans []sourceSpan) string {
	hash := sha256.New()
	_, _ = fmt.Fprintf(hash, "%d:%s", len(streamRef), streamRef)
	for _, span := range spans {
		_, _ = fmt.Fprintf(hash, "%d:%s:%d:%d:%d:%d:%d:%s:%d:%s", len(span.FileID), span.FileID, span.StartOffset, span.EndOffset, span.Device, span.Inode, span.HashBytes, span.PrefixHash, len(span.RepairGapID), span.RepairGapID)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func gapID(streamRef string, checkpoint state.FileCheckpoint, reason string) string {
	value := fmt.Sprintf("%s\x00%s\x00%s\x00%d\x00%d\x00%d\x00%s\x00%d\x00%s", streamRef, checkpoint.FileID, checkpoint.Path, checkpoint.Offset, checkpoint.Device, checkpoint.Inode, checkpoint.PrefixHash, checkpoint.HashBytes, reason)
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func coverageGapID(streamRef, scope, reason string) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00coverage\x00%s\x00%s", streamRef, scope, reason)))
	return hex.EncodeToString(digest[:])
}

func (s *containerLogSource) fail(err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	s.log.Errorf("source error: %v", err)
	if s.onError != nil {
		s.onError(err)
	}
}

func streamRef(resource streamResource) api.StreamRef {
	return api.StreamRef{ID: streamformat.ContainerLogStreamID(resource.PodUID, resource.Container), Kind: api.RecordKindContainerLog}
}

type discoveredFile struct {
	path    string
	restart int
	rotated bool
	suffix  string
}

func discoverDirectory(dir string) ([]string, []string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}
	var files []discoveredFile
	var compressed []discoveredFile
	for _, entry := range entries {
		if entry.IsDir() || strings.HasSuffix(entry.Name(), ".tmp") {
			continue
		}
		match := logNamePattern.FindStringSubmatch(entry.Name())
		if match == nil {
			continue
		}
		restart, _ := strconv.Atoi(match[1])
		item := discoveredFile{path: filepath.Join(dir, entry.Name()), restart: restart, rotated: match[2] != "", suffix: match[2]}
		if strings.HasSuffix(entry.Name(), ".gz") {
			compressed = append(compressed, item)
		} else {
			files = append(files, item)
		}
	}
	sortDiscovered(files)
	sortDiscovered(compressed)
	out := make([]string, len(files))
	for i := range files {
		out[i] = files[i].path
	}
	gzip := make([]string, len(compressed))
	for i := range compressed {
		gzip[i] = compressed[i].path
	}
	return out, gzip, nil
}

func sortDiscovered(files []discoveredFile) {
	sort.Slice(files, func(i, j int) bool {
		if files[i].restart != files[j].restart {
			return files[i].restart < files[j].restart
		}
		if files[i].rotated != files[j].rotated {
			return files[i].rotated
		}
		return files[i].suffix < files[j].suffix
	})
}

func fingerprint(path string, requestedHashBytes int) (fileFingerprint, error) {
	f, err := os.Open(path)
	if err != nil {
		return fileFingerprint{}, err
	}
	defer f.Close()
	return fingerprintFile(f, requestedHashBytes)
}

func fingerprintForCheckpoint(path string, checkpoint state.FileCheckpoint) (fileFingerprint, error) {
	if err := validateCheckpointFingerprint(checkpoint); err != nil {
		return fileFingerprint{}, err
	}
	requestedHashBytes := checkpoint.HashBytes
	if requestedHashBytes == 0 {
		requestedHashBytes = -1
	}
	return fingerprint(path, requestedHashBytes)
}

func validateCheckpointFingerprint(checkpoint state.FileCheckpoint) error {
	if checkpoint.HashBytes == 0 && (checkpoint.Offset != 0 || checkpoint.ObservedSize != 0) {
		return api.Permanent(fmt.Errorf("source checkpoint for %q has a zero-length fingerprint past offset zero", checkpoint.Path))
	}
	return nil
}

func checkpointMatchesFingerprint(checkpoint state.FileCheckpoint, fingerprint fileFingerprint) bool {
	if checkpoint.Device != fingerprint.Device || checkpoint.Inode != fingerprint.Inode {
		return false
	}
	if checkpoint.HashBytes == 0 {
		return checkpoint.Offset == 0 && checkpoint.ObservedSize == 0
	}
	return checkpoint.PrefixHash == fingerprint.PrefixHash && checkpoint.HashBytes == fingerprint.HashBytes
}

func fingerprintFile(f *os.File, requestedHashBytes int) (fileFingerprint, error) {
	info, err := f.Stat()
	if err != nil {
		return fileFingerprint{}, err
	}
	device, inode, err := sourceFileIdentity(info)
	if err != nil {
		return fileFingerprint{}, err
	}
	hashBytes := maxFingerprintHashBytes
	if info.Size() < int64(hashBytes) {
		hashBytes = int(info.Size())
	}
	if requestedHashBytes != -1 {
		if requestedHashBytes < 0 || requestedHashBytes > maxFingerprintHashBytes {
			return fileFingerprint{}, fmt.Errorf("invalid fingerprint hash bytes %d", requestedHashBytes)
		}
		hashBytes = requestedHashBytes
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fileFingerprint{}, err
	}
	prefix := make([]byte, hashBytes)
	n, err := io.ReadFull(f, prefix)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return fileFingerprint{}, err
	}
	digest := sha256.Sum256(prefix[:n])
	return fileFingerprint{Device: device, Inode: inode, PrefixHash: hex.EncodeToString(digest[:]), HashBytes: n}, nil
}

func addReason(reasons []string, reason string) []string {
	for _, existing := range reasons {
		if existing == reason {
			return reasons
		}
	}
	return append(reasons, reason)
}
