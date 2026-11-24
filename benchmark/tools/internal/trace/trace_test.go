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

package trace

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRenderWaterfall(t *testing.T) {
	tr := &Trace{
		TraceID: "abc123",
		Spans: []Span{
			{TraceID: "abc123", SpanID: "1", ServiceName: "opensandbox-bench", Name: "bench.create", StartTimeUs: 0, DurationUs: 100000},
			{TraceID: "abc123", SpanID: "2", ParentSpanID: "1", ServiceName: "opensandbox-server", Name: "POST /sandboxes", StartTimeUs: 5000, DurationUs: 90000},
			{TraceID: "abc123", SpanID: "3", ParentSpanID: "2", ServiceName: "fsb-vm", Name: "fsb_vm.ublkd_acquire", StartTimeUs: 20000, DurationUs: 40000},
		},
	}

	out := RenderWaterfall(tr)
	require.Contains(t, out, "Trace abc123 (total: 100.00ms, spans: 3)")
	require.Contains(t, out, "[opensandbox-bench] bench.create  100.00ms (self 10.00ms, 100.0%)")
	require.Contains(t, out, "[fsb-vm] fsb_vm.ublkd_acquire  40.00ms (self 40.00ms, 40.0%)")
}

func TestRenderWaterfallOverlappingChildrenSelfTime(t *testing.T) {
	// Parent is [0, 100ms]; two overlapping children cover [0, 70ms] (union = 70ms),
	// leaving 30ms of uncovered parent self-time.
	tr := &Trace{
		TraceID: "overlap",
		Spans: []Span{
			{TraceID: "overlap", SpanID: "p", ServiceName: "svc", Name: "parent", StartTimeUs: 0, DurationUs: 100000},
			{TraceID: "overlap", SpanID: "c1", ParentSpanID: "p", ServiceName: "svc", Name: "child1", StartTimeUs: 0, DurationUs: 60000},
			{TraceID: "overlap", SpanID: "c2", ParentSpanID: "p", ServiceName: "svc", Name: "child2", StartTimeUs: 10000, DurationUs: 60000},
		},
	}

	out := RenderWaterfall(tr)
	require.Contains(t, out, "[svc] parent  100.00ms (self 30.00ms, 100.0%)")
}
