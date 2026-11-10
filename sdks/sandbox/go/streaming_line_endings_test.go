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
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/iotest"
)

func TestStreamSSE_LineEndings(t *testing.T) {
	formats := []struct {
		name  string
		lines []string
		want  []StreamEvent
	}{
		{
			name: "SSE",
			lines: []string{
				": comment",
				"id: first",
				"event: stdout",
				"data: héllo 世界",
				"data: next line",
				"",
				"event: execution_complete",
				"data: {}",
			},
			want: []StreamEvent{
				{ID: "first", Event: "stdout", Data: "héllo 世界\nnext line"},
				{Event: "execution_complete", Data: "{}"},
			},
		},
		{
			name: "NDJSON",
			lines: []string{
				`{"type":"stdout","text":"héllo 世界","timestamp":1}`,
				"",
				`{"type":"execution_complete","timestamp":2}`,
			},
			want: []StreamEvent{
				{Event: "stdout", Data: `{"type":"stdout","text":"héllo 世界","timestamp":1}`},
				{Event: "execution_complete", Data: `{"type":"execution_complete","timestamp":2}`},
			},
		},
	}
	for _, ending := range []struct{ name, value string }{{"LF", "\n"}, {"CRLF", "\r\n"}, {"CR", "\r"}} {
		for _, format := range formats {
			for _, endingMode := range []string{"terminated", "unterminated"} {
				for _, chunking := range []string{"whole", "one-byte"} {
					t.Run(ending.name+"/"+format.name+"/"+endingMode+"/"+chunking, func(t *testing.T) {
						payload := strings.Join(format.lines, ending.value)
						if endingMode == "terminated" {
							payload += ending.value + ending.value
						}
						var reader io.Reader = strings.NewReader(payload)
						if chunking == "one-byte" {
							// Split every CRLF pair and every multibyte UTF-8 character.
							reader = iotest.OneByteReader(reader)
						}
						body := &sseLineEndingBody{Reader: reader}
						client := NewExecdClient("https://example.com", "tok", WithHTTPClient(&http.Client{
							Transport: sseLineEndingTransport{body: body},
						}))
						var events []StreamEvent
						err := client.RunCommand(context.Background(), RunCommandRequest{Command: "echo hello"}, func(event StreamEvent) error {
							events = append(events, event)
							return nil
						})
						require.NoError(t, err)
						require.Equal(t, format.want, events)
						require.True(t, body.closed, "stream response must be closed")
					})
				}
			}
		}
	}
}

func TestStreamSSE_CRDispatchesBeforeNextRead(t *testing.T) {
	var events []StreamEvent
	body := &sseLineEndingBody{Reader: io.MultiReader(
		strings.NewReader("event: stdout\rdata: hello\r\r"),
		sseLineEndingReadFunc(func([]byte) (int, error) {
			// A CR-terminated event is complete even if the server has not sent
			// another byte yet. Waiting for an optional LF would stall delivery.
			require.Len(t, events, 1)
			return 0, io.EOF
		}),
	)}
	client := NewExecdClient("https://example.com", "tok", WithHTTPClient(&http.Client{
		Transport: sseLineEndingTransport{body: body},
	}))
	err := client.RunCommand(context.Background(), RunCommandRequest{Command: "echo hello"}, func(event StreamEvent) error {
		events = append(events, event)
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, []StreamEvent{{Event: "stdout", Data: "hello"}}, events)
	require.True(t, body.closed, "stream response must be closed")
}

type sseLineEndingBody struct {
	io.Reader
	closed bool
}

func (b *sseLineEndingBody) Close() error {
	b.closed = true
	return nil
}

type sseLineEndingTransport struct {
	body io.ReadCloser
}

func (t sseLineEndingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		_ = req.Body.Close()
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       t.body,
		Request:    req,
	}, nil
}

type sseLineEndingReadFunc func([]byte) (int, error)

func (f sseLineEndingReadFunc) Read(p []byte) (int, error) {
	return f(p)
}
