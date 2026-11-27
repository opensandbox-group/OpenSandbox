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

package opensandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestStreamSSEReadErrorDiscardsPendingEvent(t *testing.T) {
	for _, ending := range []struct{ name, value string }{{"LF", "\n"}, {"CRLF", "\r\n"}, {"CR", "\r"}} {
		for _, background := range []bool{false, true} {
			for _, terminated := range []bool{false, true} {
				name := ending.name
				if background {
					name += "/background"
				} else {
					name += "/foreground"
				}
				if terminated {
					name += "/terminated-line"
				} else {
					name += "/unterminated-line"
				}
				t.Run(name, func(t *testing.T) {
					payload := strings.Join([]string{
						"event: stdout", "data: kept", "",
						"event: execution_complete", `data: {"type":"execution_complete"}`,
					}, ending.value)
					if terminated {
						payload += ending.value
					}
					body := &sseLineEndingBody{Reader: io.MultiReader(strings.NewReader(payload),
						sseLineEndingReadFunc(func([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }))}
					client := NewExecdClient("https://example.com", "tok", WithHTTPClient(&http.Client{
						Transport: sseLineEndingTransport{body: body},
					}))
					var events []StreamEvent
					err := client.RunCommand(context.Background(), RunCommandRequest{Command: "echo hello", Background: background}, func(event StreamEvent) error {
						events = append(events, event)
						return nil
					})
					if !errors.Is(err, io.ErrUnexpectedEOF) {
						t.Errorf("error = %v, want original read error", err)
					}
					want := []StreamEvent{{Event: "stdout", Data: "kept"}}
					if !reflect.DeepEqual(events, want) {
						t.Errorf("events = %#v, want %#v", events, want)
					}
					if !body.closed {
						t.Error("response body was not closed")
					}
				})
			}
		}
	}
}

func TestStreamSSEKeepsDelimitedRawEventsFromReadWithError(t *testing.T) {
	const rawStdout = `{"type":"stdout","text":"kept"}`
	const rawComplete = `{"type":"execution_complete"}`

	for _, ending := range []struct{ name, value string }{{"LF", "\n"}, {"CRLF", "\r\n"}, {"CR", "\r"}} {
		t.Run(ending.name, func(t *testing.T) {
			payload := rawStdout + ending.value + rawComplete
			body := &sseLineEndingBody{Reader: sseLineEndingReadFunc(func(buf []byte) (int, error) {
				if len(buf) < len(payload) {
					t.Fatal("expected the entire payload to fit in a single read")
				}
				return copy(buf, payload), io.ErrUnexpectedEOF
			})}

			var events []StreamEvent
			err := streamSSE(context.Background(), &http.Response{Body: body}, func(event StreamEvent) error {
				events = append(events, event)
				return nil
			})
			if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Errorf("error = %v, want original read error", err)
			}
			want := []StreamEvent{{Event: "stdout", Data: rawStdout}}
			if !reflect.DeepEqual(events, want) {
				t.Errorf("events = %#v, want %#v", events, want)
			}
		})
	}
}

func TestRunCommandTruncatedHTTPBodyPreservesReadError(t *testing.T) {
	const rawStdout = `{"type":"stdout","text":"kept"}`
	const rawComplete = `{"type":"execution_complete"}`

	for _, tc := range []struct {
		name      string
		payload   string
		truncated bool
		want      []StreamEvent
	}{
		{
			name:      "standard SSE block",
			payload:   "event: stdout\ndata: kept\n\nevent: execution_complete\ndata: {\"type\":\"execution_complete\"}\n",
			truncated: true,
			want:      []StreamEvent{{Event: "stdout", Data: "kept"}},
		},
		{
			name:      "raw JSON tail on read error",
			payload:   rawStdout + "\n\n" + rawComplete,
			truncated: true,
			want:      []StreamEvent{{Event: "stdout", Data: rawStdout}},
		},
		{
			name:    "raw JSON tail on clean EOF",
			payload: rawStdout + "\n\n" + rawComplete,
			want:    []StreamEvent{{Event: "stdout", Data: rawStdout}, {Event: "execution_complete", Data: rawComplete}},
		},
	} {
		for _, background := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/background=%t", tc.name, background), func(t *testing.T) {
				contentLength := len(tc.payload)
				var wantErr error
				if tc.truncated {
					contentLength += 10
					wantErr = io.ErrUnexpectedEOF
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					w.Header().Set("Content-Length", fmt.Sprint(contentLength))
					_, _ = io.WriteString(w, tc.payload)
				}))
				defer server.Close()
				client := NewExecdClient(server.URL, "tok")
				var events []StreamEvent
				err := client.RunCommand(context.Background(), RunCommandRequest{Command: "echo hello", Background: background}, func(event StreamEvent) error {
					events = append(events, event)
					return nil
				})
				if !errors.Is(err, wantErr) {
					t.Errorf("error = %v, want %v", err, wantErr)
				}
				if !reflect.DeepEqual(events, tc.want) {
					t.Errorf("events = %#v, want %#v", events, tc.want)
				}
			})
		}
	}
}
