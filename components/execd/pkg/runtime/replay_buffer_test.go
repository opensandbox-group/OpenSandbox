// Copyright 2025 Alibaba Group Holding Ltd.
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

package runtime

import (
	"bytes"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestReplayBuffer_BasicWriteRead(t *testing.T) {
	rb := newReplayBuffer()
	rb.write([]byte("hello"))
	rb.write([]byte(" world"))

	data, off := rb.ReadFrom(0)
	require.Equal(t, int64(0), off)
	require.Equal(t, []byte("hello world"), data)
	require.Equal(t, int64(11), rb.Total())
}

func TestReplayBuffer_ReadFromMiddle(t *testing.T) {
	rb := newReplayBuffer()
	rb.write([]byte("abcde"))

	data, off := rb.ReadFrom(2)
	require.Equal(t, int64(2), off)
	require.Equal(t, []byte("cde"), data)
}

func TestReplayBuffer_ReadFromCurrent(t *testing.T) {
	rb := newReplayBuffer()
	rb.write([]byte("abc"))

	data, off := rb.ReadFrom(3)
	require.Nil(t, data, "should return nil when caught up")
	require.Equal(t, int64(3), off)
}

func TestReplayBuffer_ReadFromAndSubscribeBroadcastsChanges(t *testing.T) {
	rb := newReplayBuffer()

	data1, off1, changed1 := rb.ReadFromAndSubscribe(0)
	data2, off2, changed2 := rb.ReadFromAndSubscribe(0)
	require.Nil(t, data1)
	require.Nil(t, data2)
	require.Equal(t, int64(0), off1)
	require.Equal(t, int64(0), off2)
	require.Equal(t, changed1, changed2, "current subscribers should share one notification generation")

	rb.write([]byte("hello"))

	for _, changed := range []<-chan struct{}{changed1, changed2} {
		select {
		case <-changed:
		case <-time.After(time.Second):
			t.Fatal("subscriber was not notified")
		}
	}

	data, off, nextChanged := rb.ReadFromAndSubscribe(0)
	require.Equal(t, []byte("hello"), data)
	require.Equal(t, int64(0), off)
	require.NotEqual(t, changed1, nextChanged)
	select {
	case <-nextChanged:
		t.Fatal("next notification generation closed before another write")
	default:
	}
}

func TestReplayBuffer_CircularEviction(t *testing.T) {
	rb := &replayBuffer{
		buf:  make([]byte, 8),
		size: 8,
	}

	// Write 6 bytes: "abcdef"
	rb.write([]byte("abcdef"))
	require.Equal(t, int64(6), rb.Total())

	// Write 4 more bytes: now total=10, oldest=2 (evicted "ab")
	rb.write([]byte("ghij"))
	require.Equal(t, int64(10), rb.Total())

	// offset 0 should be clamped to oldest=2
	data, off := rb.ReadFrom(0)
	require.Equal(t, int64(2), off)
	require.Equal(t, []byte("cdefghij"), data)

	// Read from offset 5 (within retained range)
	data, off = rb.ReadFrom(5)
	require.Equal(t, int64(5), off)
	require.Equal(t, []byte("fghij"), data)
}

func TestReplayBuffer_LargeGap(t *testing.T) {
	rb := &replayBuffer{
		buf:  make([]byte, 4),
		size: 4,
	}
	// Write "ABCDEF" — total=6, oldest=2, retained="CDEF"
	rb.write([]byte("ABCDEF"))

	// Requesting from 0 should clamp to oldest=2
	data, off := rb.ReadFrom(0)
	require.Equal(t, int64(2), off)
	require.Equal(t, []byte("CDEF"), data)

	// Requesting from 1 should also clamp to oldest=2
	data, off = rb.ReadFrom(1)
	require.Equal(t, int64(2), off)
	require.Equal(t, []byte("CDEF"), data)
}

func TestReplayBuffer_Concurrent(t *testing.T) {
	rb := newReplayBuffer()
	chunk := bytes.Repeat([]byte("x"), 1024)

	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 64 {
				rb.write(chunk)
			}
		}()
	}
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 32 {
				rb.ReadFrom(0)
				rb.Total()
			}
		}()
	}
	wg.Wait()

	total := rb.Total()
	require.Equal(t, int64(16*64*1024), total)
}

func TestReplayBuffer_ExactlyFull(t *testing.T) {
	rb := &replayBuffer{
		buf:  make([]byte, 4),
		size: 4,
	}
	rb.write([]byte("1234"))
	require.Equal(t, int64(4), rb.Total())

	data, off := rb.ReadFrom(0)
	require.Equal(t, int64(0), off)
	require.Equal(t, []byte("1234"), data)
}

func TestReplayBuffer_WriteWrapsCorrectly(t *testing.T) {
	rb := &replayBuffer{
		buf:  make([]byte, 4),
		size: 4,
	}
	// Write "ABCD" — buffer full
	rb.write([]byte("ABCD"))
	// Write "EF" — evicts "AB", retained "CDEF"
	rb.write([]byte("EF"))

	data, off := rb.ReadFrom(0)
	require.Equal(t, int64(2), off, "offset should be clamped to oldest=2")
	require.Equal(t, []byte("CDEF"), data)
}
