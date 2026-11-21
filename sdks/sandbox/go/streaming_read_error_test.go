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

func TestRunCommandTruncatedHTTPBodyPreservesReadError(t *testing.T) {
	for _, background := range []bool{false, true} {
		t.Run(fmt.Sprintf("background=%t", background), func(t *testing.T) {
			payload := "event: stdout\ndata: kept\n\nevent: execution_complete\ndata: {\"type\":\"execution_complete\"}\n"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Content-Length", fmt.Sprint(len(payload)+10))
				_, _ = io.WriteString(w, payload)
			}))
			defer server.Close()
			client := NewExecdClient(server.URL, "tok")
			var events []StreamEvent
			err := client.RunCommand(context.Background(), RunCommandRequest{Command: "echo hello", Background: background}, func(event StreamEvent) error {
				events = append(events, event)
				return nil
			})
			if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Errorf("error = %v, want HTTP body truncation error", err)
			}
			want := []StreamEvent{{Event: "stdout", Data: "kept"}}
			if !reflect.DeepEqual(events, want) {
				t.Errorf("events = %#v, want %#v", events, want)
			}
		})
	}
}
