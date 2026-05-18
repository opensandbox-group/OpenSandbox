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
	"fmt"
	"sync/atomic"
	"testing"
)

// payload used across benchmarks (typical SSE JSON line order of magnitude).
var benchPayload = []byte(`{"type":"stdout","eid":1,"text":"hello","timestamp":0}`)

// BenchmarkRing_pushNoEvict measures ring.push when the ring is not full (no evictHead).
func BenchmarkRing_pushNoEvict(b *testing.B) {
	r := newRing(1 << 20)
	b.SetBytes(int64(len(benchPayload)))
	var eid atomic.Int64
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.push(eid.Add(1), benchPayload, 0)
	}
}

// BenchmarkRing_pushWithEvict measures push when the ring stays at capacity (each push may evict oldest).
func BenchmarkRing_pushWithEvict(b *testing.B) {
	const cap = 64
	r := newRing(cap)
	// Fill ring so every subsequent push evicts one slot.
	for i := int64(1); i <= cap; i++ {
		r.push(i, benchPayload, 0)
	}
	b.SetBytes(int64(len(benchPayload)))
	var eid atomic.Int64
	eid.Store(cap)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.push(eid.Add(1), benchPayload, 0)
	}
}

// BenchmarkStore_Append_noEvict is Append on a warm stream with a large MaxEvents (no ring eviction).
func BenchmarkStore_Append_noEvict(b *testing.B) {
	s := NewStore(Config{MaxEvents: 1 << 20, StrictMonotonic: true})
	if err := s.Append("s", 1, benchPayload); err != nil {
		b.Fatal(err)
	}
	var eid int64 = 1
	b.SetBytes(int64(len(benchPayload)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		eid++
		if err := s.Append("s", eid, benchPayload); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkStore_Append_evicting keeps a small ring so almost every Append evicts the oldest event.
func BenchmarkStore_Append_evicting(b *testing.B) {
	s := NewStore(Config{MaxEvents: 64, StrictMonotonic: true})
	if err := s.Append("s", 1, benchPayload); err != nil {
		b.Fatal(err)
	}
	var eid int64 = 1
	b.SetBytes(int64(len(benchPayload)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		eid++
		if err := s.Append("s", eid, benchPayload); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkStore_EventsAfter measures snapshot copy cost after many appends.
func BenchmarkStore_EventsAfter(b *testing.B) {
	const n = 1000
	s := NewStore(Config{MaxEvents: n + 10, StrictMonotonic: true})
	for i := int64(1); i <= n; i++ {
		if err := s.Append("s", i, benchPayload); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = s.EventsAfter("s", 0)
	}
}

// BenchmarkStore_Append_ParallelDifferentStreams: one stream per goroutine (minimal lock contention on streamBuf).
func BenchmarkStore_Append_ParallelDifferentStreams(b *testing.B) {
	s := NewStore(Config{MaxEvents: 1 << 16, StrictMonotonic: true})
	b.SetBytes(int64(len(benchPayload)))
	var id atomic.Int64
	b.RunParallel(func(pb *testing.PB) {
		// Unique stream id per goroutine iteration batch.
		my := id.Add(1)
		sid := fmt.Sprintf("s-%d", my)
		var e int64
		for pb.Next() {
			e++
			if err := s.Append(sid, e, benchPayload); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkStore_Append_ParallelSameStream: all goroutines append to one stream (serialized on streamBuf.mu).
// StrictMonotonic is off: parallel workers would observe eids out of arrival order if enforced.
func BenchmarkStore_Append_ParallelSameStream(b *testing.B) {
	s := NewStore(Config{MaxEvents: 1 << 20, StrictMonotonic: false})
	var eid atomic.Int64
	b.SetBytes(int64(len(benchPayload)))
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			n := eid.Add(1)
			if err := s.Append("s", n, benchPayload); err != nil {
				b.Fatal(err)
			}
		}
	})
}
