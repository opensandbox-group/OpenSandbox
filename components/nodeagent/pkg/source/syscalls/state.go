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

package syscalls

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"github.com/alibaba/opensandbox/nodeagent/pkg/api"
	"github.com/alibaba/opensandbox/nodeagent/pkg/state"
	"github.com/alibaba/opensandbox/nodeagent/pkg/store"
	"github.com/alibaba/opensandbox/nodeagent/pkg/streamformat"
)

const persistedStreamSchemaVersion = 1

var persistedStreamPrefix = []byte("stream/")

type persistedStream struct {
	SchemaVersion     int               `json:"schema_version"`
	Resource          api.Resource      `json:"resource"`
	CoverageStartedAt time.Time         `json:"coverage_started_at"`
	Outcome           api.SourceOutcome `json:"outcome"`
	EndEmitted        bool              `json:"end_emitted,omitempty"`
}

func (s *source) loadStreams() (map[string]*streamRuntime, error) {
	streams := make(map[string]*streamRuntime)
	err := s.state.View(func(reader state.SourceStateReader) error {
		return reader.ForEach(func(key, value []byte) error {
			if !bytes.HasPrefix(key, persistedStreamPrefix) {
				return fmt.Errorf("unexpected syscall state key %q", key)
			}
			var persisted persistedStream
			if err := json.Unmarshal(value, &persisted); err != nil {
				return fmt.Errorf("decode syscall stream state %q: %w", key, err)
			}
			if err := validatePersistedStream(key, persisted); err != nil {
				return err
			}
			outcome := persisted.Outcome
			if !persisted.EndEmitted && !contains(outcome.LossReasons, lossReasonRestart) {
				outcome.HadSourceGaps = true
				outcome.LossReasons = append(outcome.LossReasons, lossReasonRestart)
			}
			stream := &streamRuntime{
				resource:          store.Resource{Resource: persisted.Resource},
				coverageStartedAt: persisted.CoverageStartedAt,
				outcome:           outcome,
				endReady:          persisted.EndEmitted,
			}
			streams[persisted.Resource.PodUID] = stream
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return streams, nil
}

func (s *source) persistStream(stream *streamRuntime) error {
	persisted := persistedStream{
		SchemaVersion:     persistedStreamSchemaVersion,
		Resource:          stream.resource.Resource,
		CoverageStartedAt: stream.coverageStartedAt,
		Outcome:           stream.outcome,
		EndEmitted:        stream.endReady,
	}
	key := persistedStreamKey(streamformat.SyscallStreamID(stream.resource.PodUID, stream.resource.Container))
	raw, err := json.Marshal(persisted)
	if err != nil {
		return fmt.Errorf("encode syscall stream state: %w", err)
	}
	if err := s.state.Update(func(writer state.SourceStateWriter) error {
		return writer.Put(key, raw)
	}); err != nil {
		return fmt.Errorf("persist syscall stream %s: %w", stream.resource.PodUID, err)
	}
	return nil
}

func (s *source) deleteStream(streamRef api.StreamRef) error {
	return s.state.Update(func(writer state.SourceStateWriter) error {
		return writer.Delete(persistedStreamKey(streamRef.ID))
	})
}

func validatePersistedStream(key []byte, persisted persistedStream) error {
	if persisted.SchemaVersion != persistedStreamSchemaVersion {
		return fmt.Errorf("unsupported syscall stream state version %d", persisted.SchemaVersion)
	}
	resource := persisted.Resource
	if resource.SandboxID == "" || resource.ClusterName == "" || resource.Namespace == "" || resource.PodName == "" || resource.PodUID == "" || resource.NodeName == "" || resource.Container == "" {
		return fmt.Errorf("persisted syscall stream %q has incomplete resource identity", key)
	}
	if persisted.CoverageStartedAt.IsZero() || persisted.CoverageStartedAt.Location() != time.UTC || persisted.CoverageStartedAt.Nanosecond() != 0 {
		return fmt.Errorf("persisted syscall stream %q has invalid coverage boundary", key)
	}
	if !persisted.Outcome.HadSourceGaps {
		return fmt.Errorf("persisted syscall stream %q must retain incomplete coverage", key)
	}
	wantKey := persistedStreamKey(streamformat.SyscallStreamID(resource.PodUID, resource.Container))
	if !bytes.Equal(key, wantKey) {
		return fmt.Errorf("persisted syscall stream key %q does not match resource identity", key)
	}
	return nil
}

func persistedStreamKey(streamID string) []byte {
	return append(append([]byte(nil), persistedStreamPrefix...), streamID...)
}
