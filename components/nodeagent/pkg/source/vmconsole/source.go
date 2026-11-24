// Copyright 2026 The OpenSandbox Authors
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

// Package vmconsole collects Firecracker's host-side guest serial output.
package vmconsole

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/alibaba/opensandbox/nodeagent/internal/fileidentity"
	"github.com/alibaba/opensandbox/nodeagent/pkg/api"
	"github.com/alibaba/opensandbox/nodeagent/pkg/config"
	"github.com/alibaba/opensandbox/nodeagent/pkg/registry"
	"github.com/alibaba/opensandbox/nodeagent/pkg/state"
)

const disappearanceGrace = 30 * time.Second

type stream struct {
	PlacementAttempt int64
	NextPath         string
	MissingSince     time.Time
	Device           uint64
	Inode            uint64
	Finalizing       bool
	EndedAt          time.Time
	UID              string
	Resource         api.Resource
	Path             string
	Offset           int64
	Sequence         uint64
	Prefix           string
	PrefixBytes      int
	Started          time.Time
	Outcome          api.SourceOutcome
	Ended            bool
}

// streamRuntime owns synchronization and descriptors; stream is the wire checkpoint.
type streamRuntime struct {
	mu sync.Mutex
	stream
	file    *os.File
	pending string
	ending  bool
}

type token struct {
	UID      string
	Sequence uint64
	Offset   int64
}

type source struct {
	cfg     config.Config
	state   registry.SourceState
	onError func(error)
	mu      sync.Mutex
	streams map[string]*streamRuntime
	cancel  context.CancelFunc
	done    chan struct{}
	wake    chan struct{}
	out     chan<- api.SourceEvent
}

func init() {
	registry.RegisterSource(api.SourceNameVMConsole, func(d registry.SourceDependencies) (api.Source, error) {
		return &source{cfg: d.Config, state: d.State, onError: d.OnError, streams: make(map[string]*streamRuntime), done: make(chan struct{}), wake: make(chan struct{}, 1)}, nil
	})
}

func (s *source) Capabilities() api.Capabilities {
	return api.Capabilities{RecordKinds: []api.RecordKind{api.RecordKindVMConsole}}
}

func (s *source) Start(ctx context.Context, out chan<- api.SourceEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		return errors.New("vm-console already started")
	}
	if err := s.state.View(func(r state.SourceStateReader) error {
		return r.ForEach(func(key, value []byte) error {
			var st stream
			if err := json.Unmarshal(value, &st); err != nil {
				return err
			}
			if !safeUID(st.UID) || string(key) != st.UID || st.Offset < 0 || st.Sequence == 0 || st.Started.IsZero() || st.PrefixBytes < 0 || st.PrefixBytes > 256 {
				return errors.New("invalid VM console checkpoint")
			}
			for _, input := range []string{st.Path, st.NextPath} {
				if input == "" {
					continue
				}
				relative, err := filepath.Rel(s.cfg.VMConsoleRoot, input)
				if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
					return errors.New("VM console checkpoint belongs to a different input root")
				}
			}
			s.streams[st.UID] = &streamRuntime{stream: st}
			return nil
		})
	}); err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	s.cancel, s.out = cancel, out
	go s.run(runCtx)
	return nil
}

func (s *source) Stop(ctx context.Context) error {
	s.mu.Lock()
	cancel := s.cancel
	s.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *source) save(st *stream) error {
	value, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return s.state.Update(func(w state.SourceStateWriter) error { return w.Put([]byte(st.UID), value) })
}

func (s *source) notify() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *source) Acknowledge(_ context.Context, results []api.AckResult) error {
	for _, result := range results {
		if result.Guarantee != api.GuaranteeDurable && result.Guarantee != api.GuaranteeBestEffort {
			return api.Permanent(fmt.Errorf("unsupported delivery guarantee %q", result.Guarantee))
		}
		if result.Disposition != api.AckDelivered && result.Disposition != api.AckIntentionalDrop {
			return api.Permanent(fmt.Errorf("unsupported acknowledgement disposition %q", result.Disposition))
		}
	}
	for _, result := range results {
		if err := func() error {
			var t token
			if err := json.Unmarshal(result.Token.Value, &t); err != nil {
				return api.Permanent(err)
			}
			st := s.lookup(t.UID)
			if st == nil {
				return api.Permanent(errors.New("unknown VM console stream"))
			}
			st.mu.Lock()
			defer st.mu.Unlock()
			if s.lookup(t.UID) != st || result.Token.Source != api.SourceNameVMConsole || result.Token.StreamRef != streamRef(t.UID) || result.Token.ID != tokenID(t) || st.Sequence != t.Sequence {
				return api.Permanent(errors.New("invalid VM console acknowledgement"))
			}
			if t.Offset == st.Offset {
				return nil
			}
			if st.pending != result.Token.ID || t.Offset < st.Offset {
				return api.Permanent(errors.New("unexpected VM console offset"))
			}
			next := st.stream
			next.Offset = t.Offset
			if result.Disposition == api.AckIntentionalDrop {
				next.Outcome.HadDrops = true
				reason := result.Reason
				if reason == "" {
					reason = "pipeline-policy-drop"
				}
				next.Outcome.LossReasons = api.AddLossReason(next.Outcome.LossReasons, reason)
			}
			if err := s.save(&next); err != nil {
				return err
			}
			st.stream = next
			st.pending = ""
			return nil
		}(); err != nil {
			return err
		}
	}
	s.notify()
	return nil
}

func (s *source) AcknowledgeEnd(_ context.Context, t api.EndToken) error {
	var value token
	if err := json.Unmarshal(t.Value, &value); err != nil {
		return api.Permanent(err)
	}
	st := s.lookup(value.UID)
	if st == nil {
		return api.Permanent(errors.New("unknown VM console stream"))
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if s.lookup(value.UID) != st || t.Source != api.SourceNameVMConsole || t.StreamRef != streamRef(value.UID) || t.ID != "end/"+tokenID(value) || value.Sequence != st.Sequence || value.Offset != st.Offset || !st.Finalizing && !st.Ended {
		return api.Permanent(errors.New("invalid VM console end acknowledgement"))
	}
	if st.Ended {
		return nil
	}
	next := st.stream
	next.Ended = true
	next.EndedAt = time.Now().UTC()
	if err := s.save(&next); err != nil {
		return err
	}
	st.stream = next
	s.notify()
	return nil
}

func (s *source) lookup(uid string) *streamRuntime {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.streams[uid]
}

func (s *source) snapshot() []*streamRuntime {
	s.mu.Lock()
	defer s.mu.Unlock()
	streams := make([]*streamRuntime, 0, len(s.streams))
	for _, st := range s.streams {
		streams = append(streams, st)
	}
	return streams
}

func streamRef(uid string) api.StreamRef {
	return api.StreamRef{ID: path.Join(api.SourceNameVMConsole, uid), Kind: api.RecordKindVMConsole}
}

func tokenID(t token) string {
	return fmt.Sprintf("%s/%d/%d", t.UID, t.Sequence, t.Offset)
}
func metadata(st *streamRuntime) api.StreamMetadata {
	return api.StreamMetadata{"sandbox-uid": st.UID}
}

func (s *source) run(ctx context.Context) {
	defer close(s.done)
	defer close(s.out)
	defer func() {
		for _, st := range s.snapshot() {
			st.mu.Lock()
			if st.file != nil {
				_ = st.file.Close()
				st.file = nil
			}
			st.mu.Unlock()
		}
	}()

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if err := s.scan(ctx); err != nil && !errors.Is(err, context.Canceled) {
			if s.onError != nil {
				s.onError(err)
			}
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.wake:
		}
	}
}

// vmMetadata follows the FC driver's write-once discovery contract.
// placementAttempt chooses between stale local directories, never stream identity.
type vmMetadata struct {
	Version          int    `json:"version"`
	UID              string `json:"uid"`
	Namespace        string `json:"namespace"`
	Name             string `json:"name"`
	PlacementAttempt int64  `json:"placementAttempt"`
	path             string
}

func safeUID(uid string) bool {
	return uid != "" && uid != "." && uid != ".." && !strings.ContainsAny(uid, "/\\") && strings.IndexFunc(uid, func(r rune) bool { return r <= 32 || r == 127 }) < 0 && utf8.ValidString(uid)
}

func (s *source) discover() (map[string]vmMetadata, error) {
	found := make(map[string]vmMetadata)
	for _, mode := range []string{"jails/firecracker", "sandboxes"} {
		base := filepath.Join(s.cfg.VMConsoleRoot, filepath.FromSlash(mode))
		directories, err := os.ReadDir(base)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, directory := range directories {
			if !directory.IsDir() {
				continue
			}
			root := filepath.Join(base, directory.Name())
			if mode == "jails/firecracker" {
				root = filepath.Join(root, "root")
			}
			file, err := os.Open(filepath.Join(root, "fast-sandbox.json"))
			if err != nil {
				continue
			}
			payload, err := io.ReadAll(io.LimitReader(file, 64<<10+1))
			_ = file.Close()
			var record vmMetadata
			// Missing, partial and permanently failed publications are retried, not fatal.
			if err != nil || len(payload) > 64<<10 || json.Unmarshal(payload, &record) != nil || record.Version != 1 || !safeUID(record.UID) || !safeUID(record.Namespace) || !safeUID(record.Name) || record.PlacementAttempt < 0 {
				continue
			}
			expected := record.UID
			if mode == "jails/firecracker" && len(expected) > 32 {
				expected = expected[:32]
			}
			if directory.Name() != expected {
				continue
			}
			record.path = filepath.Join(root, "firecracker.log")
			previous, exists := found[record.UID]
			if !exists || record.PlacementAttempt > previous.PlacementAttempt {
				found[record.UID] = record
			}
		}
	}
	return found, nil
}

func (s *source) scan(ctx context.Context) error {
	found, err := s.discover()
	if err != nil {
		return err
	}
	for uid, record := range found {
		if s.lookup(uid) != nil {
			continue
		}
		checkpoint := stream{UID: uid, Path: record.path, PlacementAttempt: record.PlacementAttempt, Resource: api.Resource{SandboxID: record.Name, Namespace: record.Namespace, ClusterName: s.cfg.ClusterID, NodeName: s.cfg.NodeName}, Started: time.Now().UTC().Truncate(time.Second), Sequence: 1}
		if err := s.save(&checkpoint); err != nil {
			return err
		}
		s.mu.Lock()
		s.streams[uid] = &streamRuntime{stream: checkpoint}
		s.mu.Unlock()
	}
	for _, st := range s.snapshot() {
		st.mu.Lock()
		uid := st.UID
		st.mu.Unlock()
		record, present := found[uid]
		ended, err := s.reconcile(st, record, present)
		if err != nil {
			return err
		}
		st.mu.Lock()
		finished := st.Ended
		st.mu.Unlock()
		if finished {
			continue
		}
		event, err := s.read(st, ended)
		if err != nil {
			return err
		}
		if event != nil {
			select {
			case s.out <- *event:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return nil
}

// Only the scanner changes input paths. ACKs may advance state while Stat runs.
func (s *source) reconcile(st *streamRuntime, record vmMetadata, present bool) (bool, error) {
	st.mu.Lock()
	path, nextPath := st.Path, st.NextPath
	if present && record.Name == st.Resource.SandboxID && record.Namespace == st.Resource.Namespace && record.PlacementAttempt >= st.PlacementAttempt && record.path != path {
		nextPath = record.path
	}
	st.mu.Unlock()
	input := path
	if nextPath != "" {
		if _, err := os.Stat(nextPath); errors.Is(err, os.ErrNotExist) {
			nextPath = ""
		} else if err != nil {
			return false, err
		} else {
			input = nextPath
		}
	}
	_, err := os.Stat(input)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	missing := errors.Is(err, os.ErrNotExist)
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.Ended {
		if missing && !present && time.Since(st.EndedAt) > 24*time.Hour {
			if err := s.state.Update(func(w state.SourceStateWriter) error { return w.Delete([]byte(st.UID)) }); err != nil {
				return false, err
			}
			s.mu.Lock()
			if s.streams[st.UID] == st {
				delete(s.streams, st.UID)
			}
			s.mu.Unlock()
		}
		return false, nil
	}
	next := st.stream
	if present && record.Name == st.Resource.SandboxID && record.Namespace == st.Resource.Namespace && record.PlacementAttempt >= st.PlacementAttempt {
		next.PlacementAttempt = record.PlacementAttempt
	}
	next.NextPath = nextPath
	if missing {
		if next.MissingSince.IsZero() {
			next.MissingSince = time.Now().UTC()
		}
	} else {
		next.MissingSince = time.Time{}
	}
	if next.PlacementAttempt != st.PlacementAttempt || next.NextPath != st.NextPath || !next.MissingSince.Equal(st.MissingSince) {
		if err := s.save(&next); err != nil {
			return false, err
		}
		st.stream = next
	}
	return missing && time.Since(st.MissingSince) >= disappearanceGrace, nil
}

func (s *source) open(st *streamRuntime) error {
	candidates := []string{st.Path}
	preferOld := false
	if st.NextPath != "" {
		candidates = []string{st.NextPath, st.Path}
		if info, err := os.Stat(st.Path); err == nil {
			device, inode, err := fileidentity.Read(info)
			if err != nil {
				return err
			}
			preferOld = st.Inode != 0 && st.Device == device && st.Inode == inode
			if preferOld {
				candidates = []string{st.Path, st.NextPath}
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	for _, name := range candidates {
		file, err := os.Open(name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		info, err := file.Stat()
		if err != nil {
			_ = file.Close()
			return err
		}
		if !info.Mode().IsRegular() {
			_ = file.Close()
			return errors.New("VM console is not a regular file")
		}
		device, inode, err := fileidentity.Read(info)
		if err != nil {
			_ = file.Close()
			return err
		}
		if preferOld && name == st.Path && (st.Device != device || st.Inode != inode) {
			_ = file.Close()
			continue
		}
		if st.Inode != 0 && (st.Device != device || st.Inode != inode) {
			st.Offset, st.PrefixBytes, st.Prefix = 0, 0, ""
			st.Sequence++
			gap(st, "vm-console-file-replaced")
		}
		st.Device, st.Inode = device, inode
		if name == st.NextPath {
			st.NextPath = ""
		}
		st.file, st.Path = file, name
		return nil
	}
	return nil
}

func gap(st *streamRuntime, reason string) {
	st.Outcome.HadSourceGaps = true
	st.Outcome.LossReasons = api.AddLossReason(st.Outcome.LossReasons, reason)
}

func (s *source) read(st *streamRuntime, ended bool) (*api.SourceEvent, error) {
	st.mu.Lock()
	if st.pending != "" || st.ending || st.Ended {
		st.mu.Unlock()
		return nil, nil
	}
	if st.Finalizing {
		event := s.endEvent(st)
		st.mu.Unlock()
		return event, nil
	}
	before := st.stream
	before.Outcome = before.Outcome.Clone()
	input := &streamRuntime{stream: before, file: st.file}
	st.mu.Unlock()
	event, err := s.readInput(input, ended)
	st.mu.Lock()
	defer st.mu.Unlock()
	// Descriptor ownership stays with the scanner, including error cleanup.
	st.file = input.file
	if err != nil {
		return nil, err
	}
	if st.Sequence != before.Sequence || st.Offset != before.Offset || st.pending != "" || st.Finalizing || st.Ended {
		return nil, api.Permanent(errors.New("VM console changed during input read"))
	}
	next := st.stream
	next.Path, next.NextPath = input.Path, input.NextPath
	next.Device, next.Inode = input.Device, input.Inode
	next.Offset, next.Sequence = input.Offset, input.Sequence
	next.Prefix, next.PrefixBytes = input.Prefix, input.PrefixBytes
	next.Outcome, next.Finalizing = input.Outcome, input.Finalizing
	if next.Path != before.Path || next.NextPath != before.NextPath || next.Device != before.Device || next.Inode != before.Inode || next.Offset != before.Offset || next.Sequence != before.Sequence || next.Prefix != before.Prefix || next.PrefixBytes != before.PrefixBytes || next.Finalizing != before.Finalizing || !next.Outcome.Equal(before.Outcome) {
		if err := s.save(&next); err != nil {
			return nil, err
		}
		st.stream = next
	}
	st.pending, st.ending = input.pending, input.ending
	return event, nil
}

func (s *source) readInput(st *streamRuntime, ended bool) (*api.SourceEvent, error) {
	if st.file == nil {
		if err := s.open(st); err != nil {
			return nil, err
		}
	}
	if st.file != nil {
		info, err := st.file.Stat()
		if err != nil {
			return nil, err
		}
		// Keep the open descriptor after unlink so the final tail survives driver GC.
		target := st.Path
		if st.NextPath != "" {
			target = st.NextPath
		}
		current, statErr := os.Stat(target)
		if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return nil, statErr
		}
		replaced := current != nil && !os.SameFile(info, current)
		if current != nil && !replaced && st.NextPath != "" {
			st.Path, st.NextPath = target, ""
		}
		// Drain the old descriptor before following a replacement pathname.
		if replaced && info.Size() <= st.Offset {
			_ = st.file.Close()
			st.file = nil
			st.Offset, st.PrefixBytes, st.Prefix = 0, 0, ""
			st.Device, st.Inode = 0, 0
			st.Path, st.NextPath = target, ""
			st.Sequence++
			gap(st, "vm-console-file-replaced")
			return nil, nil
		}
		if info.Size() < st.Offset {
			st.Offset, st.PrefixBytes, st.Prefix = 0, 0, ""
			st.Sequence++
			gap(st, "vm-console-file-truncated")
		}
		if st.PrefixBytes > 0 {
			prefix := make([]byte, st.PrefixBytes)
			n, err := st.file.ReadAt(prefix, 0)
			if err != nil && !errors.Is(err, io.EOF) {
				return nil, err
			}
			hash := sha256.Sum256(prefix[:n])
			if hex.EncodeToString(hash[:]) != st.Prefix {
				st.Offset, st.PrefixBytes, st.Prefix = 0, 0, ""
				st.Sequence++
				gap(st, "vm-console-file-replaced")
			}
		}
		if st.PrefixBytes == 0 && info.Size() > 0 {
			prefix := make([]byte, min(info.Size(), 256))
			n, err := st.file.ReadAt(prefix, 0)
			if err != nil && !errors.Is(err, io.EOF) {
				return nil, err
			}
			hash := sha256.Sum256(prefix[:n])
			st.PrefixBytes, st.Prefix = n, hex.EncodeToString(hash[:])
		}

		if info.Size() > st.Offset {
			chunkSize := min(int64(64<<10), s.cfg.PerSandboxQueueBytes-1024)
			if chunkSize <= 0 {
				return nil, errors.New("VM console queue budget is too small")
			}
			body := make([]byte, chunkSize)
			n, err := st.file.ReadAt(body, st.Offset)
			if err != nil && !errors.Is(err, io.EOF) {
				return nil, err
			}
			if n > 0 {
				t := token{UID: st.UID, Sequence: st.Sequence, Offset: st.Offset + int64(n)}
				value, _ := json.Marshal(t)
				st.pending = tokenID(t)
				return &api.SourceEvent{Delivery: &api.Delivery{Record: api.Record{Kind: api.RecordKindVMConsole, Timestamp: time.Now().UTC(), Resource: st.Resource, Body: body[:n]}, StreamRef: streamRef(st.UID), Metadata: metadata(st), RecordID: tokenID(t), AckToken: api.AckToken{Source: api.SourceNameVMConsole, ID: tokenID(t), StreamRef: streamRef(st.UID), Value: value}}}, nil
			}
		}
	}
	if !ended {
		return nil, nil
	}
	if st.file == nil {
		gap(st, "vm-console-file-unavailable")
	} else {
		_ = st.file.Close()
		st.file = nil
	}
	st.Finalizing = true
	return s.endEvent(st), nil
}

func (s *source) endEvent(st *streamRuntime) *api.SourceEvent {
	t := token{UID: st.UID, Sequence: st.Sequence, Offset: st.Offset}
	value, _ := json.Marshal(t)
	st.ending = true
	return &api.SourceEvent{End: &api.StreamEnd{StreamRef: streamRef(st.UID), Resource: st.Resource, Metadata: metadata(st), Revision: 1, CoverageStartedAt: st.Started, Outcome: st.Outcome.Clone(), EndToken: api.EndToken{Source: api.SourceNameVMConsole, ID: "end/" + tokenID(t), StreamRef: streamRef(st.UID), Value: value}}}
}
