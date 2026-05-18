// Copyright 2026 Alibaba Group Holding Ltd.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package sbuf provides bounded, per-stream FIFO buffers for SSE (or similar) events keyed by eid,
// used to serve disconnect resume (catch-up by event id).
// It is storage-only: callers assign eids and decide when to delete a stream.
package sbuf

import (
	"sync"
)

// Store holds bounded event rings keyed by caller-defined stream IDs (e.g. command execution id).
type Store struct {
	cfg     Config
	mu      sync.Mutex
	streams map[string]*streamBuf
}

type streamBuf struct {
	mu       sync.Mutex
	lastEid  int64
	ring     *ring
	maxBytes int64
}

// NewStore creates an empty store. cfg is copied after normalization.
func NewStore(cfg Config) *Store {
	cfg = cfg.normalized()
	return &Store{
		cfg:     cfg,
		streams: make(map[string]*streamBuf),
	}
}

// Append adds one event to the stream's ring. Payload is copied.
// With StrictMonotonic, returns ErrOutOfOrder if eid <= previous eid for this stream.
func (s *Store) Append(streamID string, eid int64, payload []byte) error {
	if streamID == "" {
		return ErrEmptyStreamID
	}
	sb := s.getOrCreate(streamID)
	sb.mu.Lock()
	defer sb.mu.Unlock()

	if s.cfg.StrictMonotonic {
		if eid <= sb.lastEid {
			return ErrOutOfOrder
		}
	}
	sb.lastEid = eid
	sb.ring.push(eid, payload, sb.maxBytes)
	return nil
}

func (s *Store) getOrCreate(streamID string) *streamBuf {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sb, ok := s.streams[streamID]; ok {
		return sb
	}
	sb := &streamBuf{
		ring:     newRing(s.cfg.MaxEvents),
		maxBytes: s.cfg.MaxBytes,
	}
	s.streams[streamID] = sb
	return sb
}

// EventsAfter returns a snapshot of events with EID > afterEid in order.
// If the stream does not exist, ok is false and events is nil.
func (s *Store) EventsAfter(streamID string, afterEid int64) (events []Event, ok bool) {
	s.mu.Lock()
	sb, found := s.streams[streamID]
	s.mu.Unlock()
	if !found {
		return nil, false
	}
	sb.mu.Lock()
	defer sb.mu.Unlock()
	return sb.ring.snapshotAfter(afterEid), true
}

// Delete removes a stream buffer. No-op if missing.
func (s *Store) Delete(streamID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.streams, streamID)
}

// Has reports whether a stream currently exists.
func (s *Store) Has(streamID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.streams[streamID]
	return ok
}
