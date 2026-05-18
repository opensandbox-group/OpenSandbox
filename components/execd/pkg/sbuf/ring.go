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

// ring is a FIFO queue with a fixed max length; push drops oldest when full.
type ring struct {
	maxLen int
	slots  []eventSlot
	head   int
	n      int
	bytes  int64
}

type eventSlot struct {
	eid     int64
	payload []byte
}

func newRing(maxLen int) *ring {
	if maxLen < 1 {
		maxLen = 1
	}
	return &ring{
		maxLen: maxLen,
		slots:  make([]eventSlot, maxLen),
	}
}

func (r *ring) push(eid int64, payload []byte, maxBytes int64) {
	pld := append([]byte(nil), payload...)
	size := int64(len(pld))

	if r.n == r.maxLen {
		r.evictHead()
	}
	idx := (r.head + r.n) % r.maxLen
	r.slots[idx] = eventSlot{eid: eid, payload: pld}
	r.n++
	r.bytes += size

	if maxBytes > 0 {
		for r.bytes > maxBytes && r.n > 0 {
			r.evictHead()
		}
	}
}

func (r *ring) evictHead() {
	if r.n == 0 {
		return
	}
	old := r.slots[r.head]
	r.bytes -= int64(len(old.payload))
	r.slots[r.head] = eventSlot{}
	r.head = (r.head + 1) % r.maxLen
	r.n--
}

func (r *ring) iterAfter(afterEid int64, fn func(eid int64, payload []byte)) {
	for i := range r.n {
		idx := (r.head + i) % r.maxLen
		s := r.slots[idx]
		if s.eid > afterEid {
			fn(s.eid, s.payload)
		}
	}
}

// snapshotAfter returns a copy slice for safe iteration outside the ring lock.
func (r *ring) snapshotAfter(afterEid int64) []Event {
	var out []Event
	r.iterAfter(afterEid, func(eid int64, payload []byte) {
		out = append(out, Event{
			EID:     eid,
			Payload: append([]byte(nil), payload...),
		})
	})
	return out
}
