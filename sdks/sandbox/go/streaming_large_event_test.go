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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestStreaming_LargeEventLine: execd emits each stdout line of a command as a
// single event on a single line, without capping its size, so an event line
// larger than the old 4 MiB scanner limit (e.g. `base64 -w0` of a few MiB) must
// still be delivered instead of failing the whole stream.
func TestStreaming_LargeEventLine(t *testing.T) {
	text := strings.Repeat("a", 5*1024*1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"type":"stdout","text":"` + text + `","timestamp":1}` + "\n\n"))
		_, _ = w.Write([]byte(`{"type":"execution_complete","timestamp":2,"execution_time":1}` + "\r\n\r\n"))
	}))
	defer srv.Close()

	client := NewExecdClient(srv.URL, "tok")
	var events []StreamEvent
	err := client.RunCommand(context.Background(), RunCommandRequest{Command: "base64 -w0 big.bin"}, func(e StreamEvent) error {
		events = append(events, e)
		return nil
	})
	require.NoError(t, err)
	require.Len(t, events, 2)
	require.Equal(t, "stdout", events[0].Event)
	require.True(t, strings.Contains(events[0].Data, text), "stdout payload must be delivered intact")
	require.Equal(t, "execution_complete", events[1].Event)
	require.Equal(t, `{"type":"execution_complete","timestamp":2,"execution_time":1}`, events[1].Data)
}
