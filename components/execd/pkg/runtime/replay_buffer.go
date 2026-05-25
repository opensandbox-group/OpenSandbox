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

import "sync"

const replayBufferSize = 1 << 20 // 1 MiB

// replayBuffer is a fixed-capacity circular byte buffer with a monotonic write counter.
//
// Invariant: head == total % size (where size is the buffer capacity).
// This means the byte at absolute offset o (o >= oldest) is stored at buf[o % size].
type replayBuffer struct {
	mu    sync.Mutex
	buf   []byte
	size  int   // == replayBufferSize (or smaller in tests)
	head  int   // next write position; always == total % size
	total int64 // monotonic byte counter (total bytes ever written)
}

func newReplayBuffer() *replayBuffer {
	return &replayBuffer{
		buf:  make([]byte, replayBufferSize),
		size: replayBufferSize,
	}
}

// write appends p to the buffer, evicting the oldest bytes when the buffer is full.
// The invariant head == total % size is preserved on every call.
func (r *replayBuffer) write(p []byte) {
	if len(p) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	// When p is larger than the whole buffer we only keep the last size bytes,
	// but we still advance total by the full len(p) to maintain the invariant.
	if len(p) >= r.size {
		skip := len(p) - r.size
		r.total += int64(skip)
		r.head = (r.head + skip) % r.size
		p = p[skip:]
		// len(p) == r.size now
	}

	// len(p) <= r.size — split into at most two contiguous copies.
	n := copy(r.buf[r.head:], p)
	r.head = (r.head + n) % r.size
	r.total += int64(n)

	if n < len(p) {
		rest := p[n:]
		copy(r.buf[r.head:], rest)
		r.head = (r.head + len(rest)) % r.size
		r.total += int64(len(rest))
	}
}

// Total returns the total number of bytes ever written to the buffer.
func (r *replayBuffer) Total() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.total
}

// ReadFrom returns a snapshot of all bytes starting from the given absolute byte offset.
//
// Returns (data, actualOffset) where actualOffset is the offset of the first returned byte:
//   - If offset >= total, returns (nil, total) — caller is already caught up.
//   - If offset < oldest retained byte, clamps to oldest (bytes were evicted).
//   - Otherwise returns bytes [offset, total).
func (r *replayBuffer) ReadFrom(offset int64) ([]byte, int64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if offset >= r.total {
		return nil, r.total
	}

	// Oldest retained absolute offset.
	oldest := r.total - int64(r.size)
	if oldest < 0 {
		oldest = 0
	}
	if offset < oldest {
		offset = oldest
	}

	count := int(r.total - offset)
	if count <= 0 {
		return nil, r.total
	}

	// Thanks to the invariant head == total % size, the byte at absolute offset o
	// is stored at buf[o % size].  This holds whether or not the buffer has wrapped.
	start := int(offset % int64(r.size))
	end := start + count

	result := make([]byte, count)
	if end <= r.size {
		copy(result, r.buf[start:end])
	} else {
		n := copy(result, r.buf[start:])
		copy(result[n:], r.buf[:count-n])
	}
	return result, offset
}
