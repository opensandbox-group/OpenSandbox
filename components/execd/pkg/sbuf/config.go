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

// Config controls per-stream bounds and append policy.
type Config struct {
	// MaxEvents is the maximum number of events retained per stream. Oldest events are dropped when exceeded.
	// Zero defaults to DefaultMaxEvents.
	MaxEvents int
	// MaxBytes is the approximate upper bound on total payload bytes per stream (sum of len(Payload)).
	// Oldest events are dropped until under the limit. Zero means no byte limit.
	MaxBytes int64
	// StrictMonotonic rejects Append when eid <= last eid for that stream. Recommended for execd SSE eids.
	StrictMonotonic bool
}

const DefaultMaxEvents = 1024

func (c *Config) normalized() Config {
	out := *c
	if out.MaxEvents <= 0 {
		out.MaxEvents = DefaultMaxEvents
	}
	return out
}
