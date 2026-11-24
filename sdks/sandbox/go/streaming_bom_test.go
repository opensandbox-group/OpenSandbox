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

func TestStreamSSE_LeadingBOM(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload string
		want    []StreamEvent
	}{
		{name: "data", payload: "\ufeffdata: first\n\n", want: []StreamEvent{{Data: "first"}}},
		{name: "event", payload: "\ufeffevent: stdout\ndata: first\n\n", want: []StreamEvent{{Event: "stdout", Data: "first"}}},
		{name: "id", payload: "\ufeffid: first\ndata: hello\n\n", want: []StreamEvent{{ID: "first", Data: "hello"}}},
		{name: "comment", payload: "\ufeff: comment\ndata: first\n\n", want: []StreamEvent{{Data: "first"}}},
		{name: "bare-json", payload: "\ufeff{\"type\":\"init\",\"text\":\"cmd-1\"}\n\n", want: []StreamEvent{{Event: "init", Data: `{"type":"init","text":"cmd-1"}`}}},
		{name: "payload-bom", payload: "\ufeffdata: \ufeffcontent\n\n", want: []StreamEvent{{Data: "\ufeffcontent"}}},
		{name: "later-bom", payload: "\n\ufeffdata: ignored\n\ndata: kept\n\n", want: []StreamEvent{{Data: "kept"}}},
		{name: "only-one-bom", payload: "\ufeff\ufeffdata: ignored\n\ndata: kept\n\n", want: []StreamEvent{{Data: "kept"}}},
	} {
		for _, ending := range []struct{ name, value string }{{"LF", "\n"}, {"CRLF", "\r\n"}, {"CR", "\r"}} {
			for _, chunking := range []string{"whole", "one-byte"} {
				t.Run(tc.name+"/"+ending.name+"/"+chunking, func(t *testing.T) {
					var reader io.Reader = strings.NewReader(strings.ReplaceAll(tc.payload, "\n", ending.value))
					if chunking == "one-byte" {
						reader = iotest.OneByteReader(reader)
					}
					body := &sseLineEndingBody{Reader: reader}
					client := NewExecdClient("http://unused.invalid", "", WithHTTPClient(&http.Client{
						Transport: sseLineEndingTransport{body: body},
					}))
					var got []StreamEvent
					err := client.RunCommand(context.Background(), RunCommandRequest{Command: "true"}, func(event StreamEvent) error {
						got = append(got, event)
						return nil
					})
					require.NoError(t, err)
					require.Equal(t, tc.want, got)
					require.True(t, body.closed)
				})
			}
		}
	}
}

func TestSandboxCommand_LeadingBOMPreservesExecutionID(t *testing.T) {
	body := &sseLineEndingBody{Reader: iotest.OneByteReader(strings.NewReader(
		"\ufeffdata: {\"type\":\"init\",\"text\":\"cmd-1\"}\n\n" +
			"data: {\"type\":\"execution_complete\",\"exit_code\":0}\n\n",
	))}
	client := NewExecdClient("http://unused.invalid", "", WithHTTPClient(&http.Client{
		Transport: sseLineEndingTransport{body: body},
	}))
	sandbox := &Sandbox{execd: client}
	execution, err := sandbox.RunCommand(context.Background(), "true", nil)
	require.NoError(t, err)
	require.Equal(t, "cmd-1", execution.ID)
	require.NotNil(t, execution.Complete)
	require.True(t, body.closed)
}
