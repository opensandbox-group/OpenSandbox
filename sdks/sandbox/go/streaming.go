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

package opensandbox

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// StreamEvent represents a single Server-Sent Event received from the server.
type StreamEvent struct {
	// Event is the event type (e.g. "stdout", "stderr", "result").
	// Empty string means no explicit event type was set.
	Event string

	// Data is the event payload. Multiple data lines are joined with newlines.
	Data string

	// ID is the optional event identifier sent by the server.
	ID string
}

// EventHandler is a callback invoked for each SSE event received from the
// server. Return a non-nil error to stop processing the stream.
type EventHandler func(event StreamEvent) error

// streamSSE reads Server-Sent Events from resp and calls handler for each
// complete event. It respects ctx cancellation and closes resp.Body on return.
func streamSSE(ctx context.Context, resp *http.Response, handler EventHandler) error {
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	// Increase scanner buffer from default 64KiB to 4MiB to handle large SSE data lines.
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)

	var current StreamEvent
	var dataLines []string
	eventCount := 0

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if !scanner.Scan() {
			// Stream ended. Dispatch any pending event.
			if len(dataLines) > 0 {
				current.Data = strings.Join(dataLines, "\n")
				if err := handler(current); err != nil {
					return err
				}
				eventCount++
			}
			if err := scanner.Err(); err != nil {
				return fmt.Errorf("opensandbox: sse read: %w", err)
			}
			if eventCount == 0 {
				return fmt.Errorf("opensandbox: empty sse stream")
			}
			return nil
		}

		line := scanner.Text()

		// Empty line signals end of an event block.
		if line == "" {
			if len(dataLines) > 0 {
				current.Data = strings.Join(dataLines, "\n")
				if err := handler(current); err != nil {
					return err
				}
				eventCount++
			}
			// Reset for next event.
			current = StreamEvent{}
			dataLines = nil
			continue
		}

		// Comment lines (starting with ':') are ignored per SSE spec.
		if strings.HasPrefix(line, ":") {
			continue
		}

		// NDJSON support: if a line starts with '{', treat it as a raw JSON
		// event. The execd server writes raw JSON blobs separated by blank
		// lines instead of standard SSE "data:" prefixed lines.
		if strings.HasPrefix(line, "{") {
			dataLines = append(dataLines, line)
			// Extract "type" field to populate Event so downstream handlers
			// that switch on event.Event work consistently for NDJSON streams.
			var probe struct{ Type string }
			if json.Unmarshal([]byte(line), &probe) == nil && probe.Type != "" {
				current.Event = probe.Type
			}
			continue
		}

		// Parse "field: value" or "field:value".
		field, value, _ := strings.Cut(line, ":")
		// Per SSE spec, if there is a space after the colon, remove it.
		value = strings.TrimPrefix(value, " ")

		switch field {
		case "data":
			dataLines = append(dataLines, value)
		case "event":
			current.Event = value
		case "id":
			current.ID = value
		}
	}
}
