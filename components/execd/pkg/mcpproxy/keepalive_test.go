// Copyright 2025 The OpenSandbox Authors
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

package mcpproxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func waitForIdleInjections(t *testing.T, k *keepaliveWriter, want int) {
	t.Helper()
	require.Eventually(t, func() bool {
		return k.IdleInjections() >= want
	}, 5*time.Second, 5*time.Millisecond, "waiting for %d keepalive injections", want)
}

func TestKeepaliveWriterDisabledByDefault(t *testing.T) {
	rec := httptest.NewRecorder()
	k := newKeepaliveWriter(rec, 0)

	rec.Header().Set("Content-Type", "text/event-stream")
	k.WriteHeader(http.StatusOK)
	_, err := k.Write([]byte("data: 1\n\n"))
	require.NoError(t, err)

	time.Sleep(30 * time.Millisecond)
	k.Stop()
	require.Equal(t, 0, k.IdleInjections())
	require.Contains(t, rec.Body.String(), "data: 1")
	require.NotContains(t, rec.Body.String(), ": keepalive")
}

func TestKeepaliveWriterNonSSEPassthrough(t *testing.T) {
	rec := httptest.NewRecorder()
	k := newKeepaliveWriter(rec, 5*time.Millisecond)

	rec.Header().Set("Content-Type", "application/json")
	k.WriteHeader(http.StatusOK)
	_, err := k.Write([]byte(`{"jsonrpc":"2.0"}`))
	require.NoError(t, err)

	waitForIdleInjections(t, k, 0)
	time.Sleep(30 * time.Millisecond)
	k.Stop()
	require.Equal(t, 0, k.IdleInjections())
	require.JSONEq(t, `{"jsonrpc":"2.0"}`, rec.Body.String())
}

func TestKeepaliveWriterInjectsOnIdleSSE(t *testing.T) {
	rec := httptest.NewRecorder()
	k := newKeepaliveWriter(rec, 5*time.Millisecond)

	rec.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	k.WriteHeader(http.StatusOK)
	_, err := k.Write([]byte("data: first\n\n"))
	require.NoError(t, err)

	waitForIdleInjections(t, k, 2)
	k.Stop()
	body := rec.Body.String()
	require.Contains(t, body, "data: first")
	require.GreaterOrEqual(t, strings.Count(body, ": keepalive\n\n"), 2)
	// Frames must be flushed through to the recorder's Flusher.
	require.True(t, rec.Flushed)
}

func TestKeepaliveWriterResetsOnActivity(t *testing.T) {
	rec := httptest.NewRecorder()
	k := newKeepaliveWriter(rec, 20*time.Millisecond)

	rec.Header().Set("Content-Type", "text/event-stream")
	k.WriteHeader(http.StatusOK)

	deadline := time.Now().Add(300 * time.Millisecond)
	writes := 0
	for time.Now().Before(deadline) {
		_, err := k.Write([]byte(": tick\n\n"))
		require.NoError(t, err)
		writes++
		time.Sleep(5 * time.Millisecond)
	}
	k.Stop()

	// Active writing keeps resetting the idle clock: injections stay far
	// below the number of intervals that elapsed.
	require.Less(t, k.IdleInjections(), writes/2)
}

func TestKeepaliveWriterNeverSplitsPartialEvents(t *testing.T) {
	rec := httptest.NewRecorder()
	k := newKeepaliveWriter(rec, 5*time.Millisecond)

	rec.Header().Set("Content-Type", "text/event-stream")
	k.WriteHeader(http.StatusOK)
	// A partial event with no trailing blank line: injecting here would
	// splice a comment into the middle of the event.
	_, err := k.Write([]byte("data: part"))
	require.NoError(t, err)

	time.Sleep(50 * time.Millisecond)
	k.Stop()
	require.Equal(t, 0, k.IdleInjections(), "keepalive injected mid-event")
	require.Equal(t, "data: part", rec.Body.String())

	// Completing the event re-enables injection at the boundary.
	k2 := newKeepaliveWriter(httptest.NewRecorder(), 5*time.Millisecond)
	k2.ResponseWriter.(*httptest.ResponseRecorder).Header().Set("Content-Type", "text/event-stream")
	k2.WriteHeader(http.StatusOK)
	_, err = k2.Write([]byte("data: par"))
	require.NoError(t, err)
	_, err = k2.Write([]byte("tial\n\n"))
	require.NoError(t, err)
	waitForIdleInjections(t, k2, 1)
	k2.Stop()
}

func TestSSEBoundaryTracker(t *testing.T) {
	cases := []struct {
		name     string
		chunks   []string
		boundary bool
	}{
		{"empty stream", nil, true},
		{"complete event", []string{"data: x\n\n"}, true},
		{"complete event crlf", []string{"data: x\r\n\r\n"}, true},
		{"split across chunks", []string{"data: ", "x", "\n", "\n"}, true},
		{"partial event", []string{"data: x\n"}, false},
		{"mid-field", []string{"data"}, false},
		{"second line started", []string{"data: x\ndata: y"}, false},
		{"event then partial", []string{"data: x\n\ndata"}, false},
		{"comment frames", []string{": tick\n\n"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := newSSEBoundaryTracker()
			for _, c := range tc.chunks {
				tr.observe([]byte(c))
			}
			require.Equal(t, tc.boundary, tr.atBoundary())
		})
	}
}

func TestKeepaliveWriterSkipsContentEncodedResponses(t *testing.T) {
	rec := httptest.NewRecorder()
	k := newKeepaliveWriter(rec, 5*time.Millisecond)

	// A gzip-encoded SSE stream: appending plaintext comments would corrupt
	// the encoded body, so injection must stay off.
	rec.Header().Set("Content-Type", "text/event-stream")
	rec.Header().Set("Content-Encoding", "gzip")
	k.WriteHeader(http.StatusOK)
	_, err := k.Write([]byte("data: first\n\n"))
	require.NoError(t, err)

	time.Sleep(50 * time.Millisecond)
	k.Stop()
	require.Equal(t, 0, k.IdleInjections())
}

func TestKeepaliveWriterStopIdempotent(t *testing.T) {
	rec := httptest.NewRecorder()
	k := newKeepaliveWriter(rec, 5*time.Millisecond)
	rec.Header().Set("Content-Type", "text/event-stream")
	k.WriteHeader(http.StatusOK)
	k.Stop()
	k.Stop()
	k.Stop()
}

func TestMediaType(t *testing.T) {
	require.Equal(t, "text/event-stream", mediaType("text/event-stream; charset=utf-8"))
	require.Equal(t, "application/json", mediaType("application/json"))
	require.Equal(t, "", mediaType(""))
}
