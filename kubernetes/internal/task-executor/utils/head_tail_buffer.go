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

package utils

import "sync"

// HeadTailBuffer is a bounded writer that retains the beginning and end of
// everything written to it. It always reports a full write so discarded bytes
// do not affect the producer.
type HeadTailBuffer struct {
	mu        sync.Mutex
	head      []byte
	tail      []byte
	headLimit int
	tailLimit int
	marker    string
	size      int
	truncated bool
}

func NewHeadTailBuffer(headBytes, tailBytes int, marker string) *HeadTailBuffer {
	validateHeadTailLimits(headBytes, tailBytes)
	return &HeadTailBuffer{
		head:      make([]byte, 0, headBytes),
		tail:      make([]byte, 0, tailBytes),
		headLimit: headBytes,
		tailLimit: tailBytes,
		marker:    marker,
	}
}

func (b *HeadTailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	written := len(p)
	if !b.truncated {
		if written > b.headLimit+b.tailLimit-b.size {
			b.truncated = true
		} else {
			b.size += written
		}
	}

	if remaining := b.headLimit - len(b.head); remaining > 0 {
		toHead := min(remaining, len(p))
		b.head = append(b.head, p[:toHead]...)
		p = p[toHead:]
	}

	b.writeTail(p)
	return written, nil
}

func (b *HeadTailBuffer) writeTail(p []byte) {
	if b.tailLimit == 0 {
		return
	}
	if len(p) >= b.tailLimit {
		b.tail = append(b.tail[:0], p[len(p)-b.tailLimit:]...)
		return
	}

	if overflow := len(b.tail) + len(p) - b.tailLimit; overflow > 0 {
		copy(b.tail, b.tail[overflow:])
		b.tail = b.tail[:len(b.tail)-overflow]
	}
	b.tail = append(b.tail, p...)
}

func (b *HeadTailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	markerBytes := 0
	if b.truncated {
		markerBytes = len(b.marker)
	}
	output := make([]byte, 0, len(b.head)+markerBytes+len(b.tail))
	output = append(output, b.head...)
	if b.truncated {
		output = append(output, b.marker...)
	}
	output = append(output, b.tail...)
	return string(output)
}

// TruncateHeadTail bounds text without first copying the complete input.
func TruncateHeadTail(value string, headBytes, tailBytes int, marker string) string {
	validateHeadTailLimits(headBytes, tailBytes)
	if len(value) <= headBytes+tailBytes {
		return value
	}
	return value[:headBytes] + marker + value[len(value)-tailBytes:]
}

func validateHeadTailLimits(headBytes, tailBytes int) {
	if headBytes < 0 || tailBytes < 0 {
		panic("head and tail limits must not be negative")
	}
}
