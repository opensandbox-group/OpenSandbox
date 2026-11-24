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
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Span represents a normalized OTel span saved in <run-bundle>/traces/<trace_id>.json.
type Span struct {
	TraceID       string         `json:"trace_id"`
	SpanID        string         `json:"span_id"`
	ParentSpanID  string         `json:"parent_span_id,omitempty"`
	ServiceName   string         `json:"service_name"`
	Name          string         `json:"name"`
	StartTimeUs   int64          `json:"start_time_us"`
	DurationUs    int64          `json:"duration_us"`
	Status        string         `json:"status,omitempty"` // ok | error
	Attributes    map[string]any `json:"attributes,omitempty"`
	LinkedTraceID string         `json:"linked_trace_id,omitempty"`
}

// Trace is a normalized trace tree for offline inspection and P99 attribution.
type Trace struct {
	TraceID   string `json:"trace_id"`
	Operation string `json:"operation,omitempty"`
	Spans     []Span `json:"spans"`
}

// Backend abstracts querying an in-cluster OTel trace store (Jaeger v2 or Grafana Tempo).
type Backend interface {
	Name() string
	GetTrace(ctx context.Context, traceID string) (*Trace, error)
}

// LoadBundleTraces reads all saved trace JSON files from <bundleDir>/traces/.
func LoadBundleTraces(bundleDir string) ([]*Trace, error) {
	tracesDir := filepath.Join(bundleDir, "traces")
	entries, err := os.ReadDir(tracesDir)
	if err != nil {
		return nil, fmt.Errorf("read traces dir %q: %w", tracesDir, err)
	}
	var out []*Trace
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		t, err := LoadTraceFile(filepath.Join(tracesDir, e.Name()))
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].TotalDurationUs() > out[j].TotalDurationUs()
	})
	return out, nil
}

// LoadTraceFile loads a single trace JSON file.
func LoadTraceFile(path string) (*Trace, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read trace file %q: %w", path, err)
	}
	var t Trace
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, fmt.Errorf("parse trace file %q: %w", path, err)
	}
	return &t, nil
}

// TotalDurationUs returns the wall-clock span of the trace in microseconds.
func (t *Trace) TotalDurationUs() int64 {
	if len(t.Spans) == 0 {
		return 0
	}
	minStart := t.Spans[0].StartTimeUs
	maxEnd := t.Spans[0].StartTimeUs + t.Spans[0].DurationUs
	for _, s := range t.Spans[1:] {
		if s.StartTimeUs < minStart {
			minStart = s.StartTimeUs
		}
		if end := s.StartTimeUs + s.DurationUs; end > maxEnd {
			maxEnd = end
		}
	}
	if maxEnd <= minStart {
		return 0
	}
	return maxEnd - minStart
}

// RenderWaterfall formats the trace as an ASCII tree with duration, self-time, and share (%).
func RenderWaterfall(t *Trace) string {
	if t == nil || len(t.Spans) == 0 {
		return "(empty trace)\n"
	}
	totalUs := t.TotalDurationUs()
	byID := make(map[string]Span, len(t.Spans))
	children := make(map[string][]Span)
	var roots []Span

	for _, s := range t.Spans {
		byID[s.SpanID] = s
	}
	for _, s := range t.Spans {
		if s.ParentSpanID == "" {
			roots = append(roots, s)
			continue
		}
		if _, ok := byID[s.ParentSpanID]; !ok {
			roots = append(roots, s)
			continue
		}
		children[s.ParentSpanID] = append(children[s.ParentSpanID], s)
	}

	sortSpans := func(ss []Span) {
		sort.Slice(ss, func(i, j int) bool {
			return ss[i].StartTimeUs < ss[j].StartTimeUs
		})
	}
	sortSpans(roots)
	for k := range children {
		sortSpans(children[k])
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Trace %s (total: %.2fms, spans: %d)\n", t.TraceID, float64(totalUs)/1000.0, len(t.Spans))

	var walk func(s Span, prefix string, isLast bool, isRoot bool)
	walk = func(s Span, prefix string, isLast bool, isRoot bool) {
		kids := children[s.SpanID]
		var childSum int64
		for _, k := range kids {
			childSum += k.DurationUs
		}
		selfUs := s.DurationUs - childSum
		if selfUs < 0 {
			selfUs = 0
		}
		share := 0.0
		if totalUs > 0 {
			share = (float64(s.DurationUs) / float64(totalUs)) * 100.0
		}

		branch := ""
		nextPrefix := prefix
		if !isRoot {
			if isLast {
				branch = "└── "
				nextPrefix = prefix + "    "
			} else {
				branch = "├── "
				nextPrefix = prefix + "│   "
			}
		}

		svc := s.ServiceName
		if svc == "" {
			svc = "unknown"
		}
		status := ""
		if strings.EqualFold(s.Status, "error") {
			status = " [ERROR]"
		}
		fmt.Fprintf(&b, "%s%s[%s] %s  %.2fms (self %.2fms, %.1f%%)%s\n",
			prefix, branch, svc, s.Name,
			float64(s.DurationUs)/1000.0, float64(selfUs)/1000.0, share, status)

		for i, k := range kids {
			walk(k, nextPrefix, i == len(kids)-1, false)
		}
	}

	for i, r := range roots {
		walk(r, "", i == len(roots)-1, true)
	}
	return b.String()
}
