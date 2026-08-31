// Copyright 2026 Alibaba Group Holding Ltd.
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

package containerlogs

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"time"
)

var errMalformedCRI = errors.New("malformed CRI record")

const maxContinuationMarkers = 128

type criLine struct {
	Timestamp time.Time
	Stream    string
	Partial   bool
	Body      []byte
}

func parseCRILine(raw []byte) (criLine, error) {
	raw = bytes.TrimSuffix(raw, []byte("\n"))
	first := bytes.IndexByte(raw, ' ')
	if first <= 0 {
		return criLine{}, errMalformedCRI
	}
	secondRel := bytes.IndexByte(raw[first+1:], ' ')
	if secondRel <= 0 {
		return criLine{}, errMalformedCRI
	}
	second := first + 1 + secondRel
	thirdRel := bytes.IndexByte(raw[second+1:], ' ')
	if thirdRel <= 0 {
		return criLine{}, errMalformedCRI
	}
	third := second + 1 + thirdRel
	timestamp, err := time.Parse(time.RFC3339Nano, string(raw[:first]))
	if err != nil {
		return criLine{}, fmt.Errorf("%w: timestamp", errMalformedCRI)
	}
	stream := string(raw[first+1 : second])
	if stream != "stdout" && stream != "stderr" {
		return criLine{}, fmt.Errorf("%w: stream", errMalformedCRI)
	}
	tag := raw[second+1 : third]
	if index := bytes.IndexByte(tag, ':'); index >= 0 {
		tag = tag[:index]
	}
	if len(tag) != 1 || tag[0] != 'F' && tag[0] != 'P' {
		return criLine{}, fmt.Errorf("%w: tag", errMalformedCRI)
	}
	body := append([]byte(nil), raw[third+1:]...)
	return criLine{Timestamp: timestamp, Stream: stream, Partial: tag[0] == 'P', Body: body}, nil
}

type partial struct {
	timestamp time.Time
	updatedAt time.Time
	body      []byte
	spans     []sourceSpan
	truncated bool
	spanLimit bool
	fragments int
	continued string
	sequence  uint64
}

type assembler struct {
	maxLineBytes        int
	timeout             time.Duration
	streams             map[partialKey]*partial
	continuation        map[partialKey]continuationMarker
	nextPartialSeq      uint64
	nextContinuationSeq uint64
}

type partialKey struct {
	restart     string
	stream      string
	repairGapID string
}

type continuationMarker struct {
	value    string
	sequence uint64
}

type assembled struct {
	timestamp   time.Time
	stream      string
	body        []byte
	spans       []sourceSpan
	dropReasons []string
}

func newAssembler(maxLineBytes int, timeout time.Duration) *assembler {
	return &assembler{maxLineBytes: maxLineBytes, timeout: timeout, streams: make(map[partialKey]*partial), continuation: make(map[partialKey]continuationMarker)}
}

func (a *assembler) consume(line criLine, span sourceSpan, now time.Time) *assembled {
	key := partialKey{restart: span.restart, stream: line.Stream, repairGapID: span.RepairGapID}
	current := a.streams[key]
	if line.Partial {
		if current == nil {
			a.nextPartialSeq++
			current = &partial{timestamp: line.Timestamp, continued: a.continuation[key].value, sequence: a.nextPartialSeq}
			delete(a.continuation, key)
			a.streams[key] = current
		}
		current.updatedAt = now
		current.spans = appendSourceSpan(current.spans, span)
		current.fragments++
		before := len(current.body)
		current.body = appendLimited(current.body, line.Body, a.maxLineBytes)
		current.truncated = current.truncated || before+len(line.Body) > len(current.body)
		if current.fragments > 4096 {
			current.spanLimit = true
			delete(a.streams, key)
			a.setContinuation(key, "[opensandbox: continuation-after-span-limit]")
			body, reasons := finishPartial(current, "")
			return &assembled{timestamp: current.timestamp, stream: line.Stream, body: body, spans: current.spans, dropReasons: reasons}
		}
		return nil
	}
	if current == nil {
		body := appendLimited(nil, line.Body, a.maxLineBytes)
		var reasons []string
		if len(line.Body) > len(body) {
			body = append(body, []byte("[opensandbox: truncated]")...)
			reasons = append(reasons, "line-truncated")
		}
		if continuation := a.continuation[key].value; continuation != "" {
			body = append([]byte(continuation), body...)
			delete(a.continuation, key)
		}
		return &assembled{timestamp: line.Timestamp, stream: line.Stream, body: body, spans: []sourceSpan{span}, dropReasons: reasons}
	}
	delete(a.streams, key)
	before := len(current.body)
	current.body = appendLimited(current.body, line.Body, a.maxLineBytes)
	current.truncated = current.truncated || before+len(line.Body) > len(current.body)
	current.spans = appendSourceSpan(current.spans, span)
	body, reasons := finishPartial(current, "")
	return &assembled{timestamp: current.timestamp, stream: line.Stream, body: body, spans: current.spans, dropReasons: reasons}
}

func finishPartial(current *partial, terminal string) ([]byte, []string) {
	body := append([]byte(nil), current.body...)
	if current.continued != "" {
		body = append([]byte(current.continued), body...)
	}
	var reasons []string
	if current.spanLimit {
		body = append(body, []byte("[opensandbox: span-limit]")...)
		reasons = append(reasons, "partial-span-limit")
	}
	if current.truncated {
		body = append(body, []byte("[opensandbox: truncated]")...)
		reasons = append(reasons, "line-truncated")
	}
	if terminal != "" {
		body = append(body, []byte(terminal)...)
	}
	return body, reasons
}

func (a *assembler) expired(now time.Time) []assembled {
	var out []assembled
	for _, entry := range a.orderedPartials(func(current *partial) bool {
		return now.Sub(current.updatedAt) >= a.timeout
	}) {
		key, current := entry.key, entry.current
		body, reasons := finishPartial(current, "[opensandbox: partial-timeout]")
		out = append(out, assembled{timestamp: current.timestamp, stream: key.stream, body: body, spans: current.spans, dropReasons: reasons})
		delete(a.streams, key)
		a.setContinuation(key, "[opensandbox: continuation-after-timeout]")
	}
	return out
}

func (a *assembler) finish() []assembled {
	var out []assembled
	for _, entry := range a.orderedPartials(nil) {
		key, current := entry.key, entry.current
		body, reasons := finishPartial(current, "[opensandbox: incomplete-partial]")
		out = append(out, assembled{timestamp: current.timestamp, stream: key.stream, body: body, spans: current.spans, dropReasons: reasons})
		delete(a.streams, key)
	}
	return out
}

type partialEntry struct {
	key     partialKey
	current *partial
}

func (a *assembler) orderedPartials(include func(*partial) bool) []partialEntry {
	entries := make([]partialEntry, 0, len(a.streams))
	for key, current := range a.streams {
		if include == nil || include(current) {
			entries = append(entries, partialEntry{key: key, current: current})
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].current.sequence < entries[j].current.sequence
	})
	return entries
}

func (a *assembler) setContinuation(key partialKey, value string) {
	if _, exists := a.continuation[key]; !exists && len(a.continuation) >= maxContinuationMarkers {
		var oldest partialKey
		var oldestSequence uint64
		first := true
		for candidate, marker := range a.continuation {
			if first || marker.sequence < oldestSequence {
				oldest = candidate
				oldestSequence = marker.sequence
				first = false
			}
		}
		delete(a.continuation, oldest)
	}
	a.nextContinuationSeq++
	a.continuation[key] = continuationMarker{value: value, sequence: a.nextContinuationSeq}
}

func restartIdentity(path string) string {
	base := filepath.Base(path)
	if match := logNamePattern.FindStringSubmatch(base); match != nil {
		return match[1]
	}
	return path
}

func appendSourceSpan(spans []sourceSpan, span sourceSpan) []sourceSpan {
	if len(spans) > 0 {
		last := &spans[len(spans)-1]
		if last.Path == span.Path && last.FileID == span.FileID && last.RepairGapID == span.RepairGapID && last.EndOffset == span.StartOffset && last.fingerprint == span.fingerprint {
			last.EndOffset = span.EndOffset
			return spans
		}
	}
	return append(spans, span)
}

func appendLimited(dst, src []byte, limit int) []byte {
	remaining := limit - len(dst)
	if remaining <= 0 {
		return dst
	}
	if len(src) > remaining {
		src = src[:remaining]
	}
	return append(dst, src...)
}
