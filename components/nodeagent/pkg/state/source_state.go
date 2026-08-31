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
	"errors"
	"fmt"
	"strings"

	"github.com/alibaba/opensandbox/nodeagent/pkg/api"
	bolt "go.etcd.io/bbolt"
)

var bucketSourcePrivate = []byte("source_private")

// SourceStateReader exposes one Source's private state for the duration of a
// SourceState.View or SourceState.Update callback.
type SourceStateReader interface {
	Get(key []byte) (value []byte, found bool, err error)
	ForEach(func(key, value []byte) error) error
}

// SourceStateWriter extends SourceStateReader with mutations that are committed
// atomically when the SourceState.Update callback succeeds.
type SourceStateWriter interface {
	SourceStateReader
	Put(key, value []byte) error
	Delete(key []byte) error
}

// LegacyContainerLogCheckpoint is the narrow compatibility surface used by
// the built-in container-logs Source while its version-1 checkpoint layout is
// still supported.
type LegacyContainerLogCheckpoint interface {
	GetFileCheckpoint(streamRef, path string) (FileCheckpoint, bool, error)
	ListFileCheckpoints(streamRef string) ([]FileCheckpoint, error)
	GetSourceStream(streamRef string) (SourceStream, bool, error)
	ListSourceStreams() ([]SourceStream, error)
	PutSourceStream(SourceStream) error
	CommitSource([]FileCheckpoint, SourceStream) error
	DeleteStream(string) error
}

// SourceState is a handle bound to one Source's private bbolt namespace.
// Callers cannot use it to select or inspect another Source's namespace.
type SourceState struct {
	db     *DB
	source string
}

type legacyContainerLogCheckpoint struct {
	state *SourceState
}

type sourceStateTx struct {
	bucket *bolt.Bucket
}

// SourceState returns a handle permanently scoped to source.
func (d *DB) SourceState(source string) (*SourceState, error) {
	if err := validateSourceStateName(source); err != nil {
		return nil, err
	}
	return &SourceState{db: d, source: source}, nil
}

// View runs fn in a read-only transaction. Values passed out of this package
// are copied so they remain valid for the whole callback and cannot mutate
// bbolt pages.
func (s *SourceState) View(fn func(SourceStateReader) error) error {
	if s == nil || s.db == nil {
		return errors.New("source state handle is nil")
	}
	if fn == nil {
		return errors.New("source state callback is nil")
	}
	return s.db.db.View(func(tx *bolt.Tx) error {
		root := tx.Bucket(bucketSourcePrivate)
		return fn(sourceStateTx{bucket: root.Bucket([]byte(s.source))})
	})
}

// Update runs fn in a writable transaction. All mutations are committed
// together, or rolled back if fn returns an error.
func (s *SourceState) Update(fn func(SourceStateWriter) error) error {
	if s == nil || s.db == nil {
		return errors.New("source state handle is nil")
	}
	if fn == nil {
		return errors.New("source state callback is nil")
	}
	return s.db.db.Update(func(tx *bolt.Tx) error {
		root := tx.Bucket(bucketSourcePrivate)
		bucket, err := root.CreateBucketIfNotExists([]byte(s.source))
		if err != nil {
			return fmt.Errorf("create private state for source %q: %w", s.source, err)
		}
		return fn(sourceStateTx{bucket: bucket})
	})
}

func (s sourceStateTx) Get(key []byte) ([]byte, bool, error) {
	if err := validateSourceStateKey(key); err != nil {
		return nil, false, err
	}
	if s.bucket == nil {
		return nil, false, nil
	}
	value := s.bucket.Get(key)
	if value == nil {
		return nil, false, nil
	}
	return append([]byte(nil), value...), true, nil
}

func (s sourceStateTx) ForEach(fn func(key, value []byte) error) error {
	if fn == nil {
		return errors.New("source state iterator is nil")
	}
	if s.bucket == nil {
		return nil
	}
	return s.bucket.ForEach(func(key, value []byte) error {
		if value == nil {
			return fmt.Errorf("source state key %q is a nested bucket", key)
		}
		return fn(append([]byte(nil), key...), append([]byte(nil), value...))
	})
}

func (s sourceStateTx) Put(key, value []byte) error {
	if err := validateSourceStateKey(key); err != nil {
		return err
	}
	return s.bucket.Put(key, value)
}

func (s sourceStateTx) Delete(key []byte) error {
	if err := validateSourceStateKey(key); err != nil {
		return err
	}
	return s.bucket.Delete(key)
}

func validateSourceStateName(source string) error {
	if source == "" {
		return errors.New("source state name is empty")
	}
	if strings.Contains(source, "/") {
		return errors.New("source state name contains a slash")
	}
	if len(source) > bolt.MaxKeySize {
		return fmt.Errorf("source state name is too large: %d bytes", len(source))
	}
	return nil
}

// LegacyContainerLogCheckpoint exposes only the version-1 container-log
// checkpoint operations, and only to the handle bound to that Source. The
// generic Source factory contract does not include this compatibility method.
func (s *SourceState) LegacyContainerLogCheckpoint() (LegacyContainerLogCheckpoint, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("source state handle is nil")
	}
	if s.source != api.SourceNameContainerLogs {
		return nil, fmt.Errorf("legacy container-log checkpoints are unavailable to source %q", s.source)
	}
	return legacyContainerLogCheckpoint{state: s}, nil
}

func (s legacyContainerLogCheckpoint) GetFileCheckpoint(streamRef, path string) (FileCheckpoint, bool, error) {
	return s.state.db.GetFileCheckpoint(streamRef, path)
}

func (s legacyContainerLogCheckpoint) ListFileCheckpoints(streamRef string) ([]FileCheckpoint, error) {
	return s.state.db.ListFileCheckpoints(streamRef)
}

func (s legacyContainerLogCheckpoint) GetSourceStream(streamRef string) (SourceStream, bool, error) {
	return s.state.db.GetSourceStream(streamRef)
}

func (s legacyContainerLogCheckpoint) ListSourceStreams() ([]SourceStream, error) {
	return s.state.db.ListSourceStreams()
}

func (s legacyContainerLogCheckpoint) PutSourceStream(stream SourceStream) error {
	return s.state.db.PutSourceStream(stream)
}

func (s legacyContainerLogCheckpoint) CommitSource(checkpoints []FileCheckpoint, stream SourceStream) error {
	return s.state.db.CommitSource(checkpoints, stream)
}

func (s legacyContainerLogCheckpoint) DeleteStream(streamRef string) error {
	return s.state.db.DeleteStream(streamRef)
}

func validateSourceStateKey(key []byte) error {
	if len(key) == 0 {
		return errors.New("source state key is empty")
	}
	if len(key) > bolt.MaxKeySize {
		return fmt.Errorf("source state key is too large: %d bytes", len(key))
	}
	return nil
}
