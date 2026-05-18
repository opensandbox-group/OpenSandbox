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

package sbuf

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStore_EventsAfter_NotFound(t *testing.T) {
	s := NewStore(Config{MaxEvents: 8, StrictMonotonic: true})
	ev, ok := s.EventsAfter("missing", 0)
	require.False(t, ok)
	require.Nil(t, ev)
}

func TestStore_AppendStrictMonotonic(t *testing.T) {
	s := NewStore(Config{MaxEvents: 8, StrictMonotonic: true})
	require.NoError(t, s.Append("stream-a", 1, []byte(`{"a":1}`)))
	require.ErrorIs(t, s.Append("stream-a", 1, []byte(`dup`)), ErrOutOfOrder)
	require.NoError(t, s.Append("stream-a", 2, []byte(`{"a":2}`)))

	ev, ok := s.EventsAfter("stream-a", 0)
	require.True(t, ok)
	require.Len(t, ev, 2)
	require.Equal(t, int64(1), ev[0].EID)
	require.Equal(t, `{"a":1}`, string(ev[0].Payload))

	ev2, _ := s.EventsAfter("stream-a", 1)
	require.Len(t, ev2, 1)
	require.Equal(t, int64(2), ev2[0].EID)
}

func TestStore_MaxEventsEvictsOldest(t *testing.T) {
	s := NewStore(Config{MaxEvents: 3, StrictMonotonic: true})
	for i := int64(1); i <= 5; i++ {
		require.NoError(t, s.Append("s", i, []byte{byte(i)}))
	}
	ev, ok := s.EventsAfter("s", 0)
	require.True(t, ok)
	require.Len(t, ev, 3)
	require.Equal(t, int64(3), ev[0].EID)
	require.Equal(t, byte(3), ev[0].Payload[0])
}

func TestStore_MaxBytesEvicts(t *testing.T) {
	s := NewStore(Config{MaxEvents: 100, MaxBytes: 10, StrictMonotonic: true})
	require.NoError(t, s.Append("s", 1, []byte("1234567890")))
	require.NoError(t, s.Append("s", 2, []byte("1234567890")))
	ev, ok := s.EventsAfter("s", 0)
	require.True(t, ok)
	require.Len(t, ev, 1)
	require.Equal(t, int64(2), ev[0].EID)
}

func TestStore_Delete(t *testing.T) {
	s := NewStore(Config{MaxEvents: 8, StrictMonotonic: true})
	require.NoError(t, s.Append("x", 1, []byte("a")))
	require.True(t, s.Has("x"))
	s.Delete("x")
	require.False(t, s.Has("x"))
}

func TestStore_EmptyStreamID(t *testing.T) {
	s := NewStore(Config{})
	require.ErrorIs(t, s.Append("", 1, nil), ErrEmptyStreamID)
}
