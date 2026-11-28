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
	"sync/atomic"
	"testing"
	"time"
)

func shortenHealthPingTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	previous := healthPingTimeout
	healthPingTimeout = d
	t.Cleanup(func() { healthPingTimeout = previous })
}

// stallingServer holds the first stalledRequests requests open until the
// client gives up on them, and answers later requests with 200.
func stallingServer(t *testing.T, stalledRequests int64) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= stalledRequests {
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestWaitUntilReady_RecoversAfterStalledHealthPing(t *testing.T) {
	shortenHealthPingTimeout(t, 100*time.Millisecond)
	srv, calls := stallingServer(t, 1)
	sb := &Sandbox{
		id:    "sbx-stalled-ping",
		execd: NewExecdClient(srv.URL, "tok", WithTimeout(30*time.Second)),
	}

	start := time.Now()
	err := sb.WaitUntilReady(context.Background(), ReadyOptions{
		Timeout:         2 * time.Second,
		PollingInterval: 10 * time.Millisecond,
	})

	require.NoError(t, err)
	require.True(t, calls.Load() >= 2, "expected a second ping after the stalled one")
	require.LessOrEqual(t, time.Since(start), time.Second)
}

func TestExecdPing_CapsClientWithoutRequestTimeout(t *testing.T) {
	shortenHealthPingTimeout(t, 100*time.Millisecond)
	srv, _ := stallingServer(t, 1)
	execd := NewExecdClient(srv.URL, "tok")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	err := execd.Ping(ctx)

	require.Error(t, err)
	require.NoError(t, ctx.Err(), "the ping cap, not the caller deadline, must end the request")
	require.LessOrEqual(t, time.Since(start), time.Second)
}

func TestExecdPing_KeepsShorterRequestTimeout(t *testing.T) {
	shortenHealthPingTimeout(t, 5*time.Second)
	srv, _ := stallingServer(t, 1)
	execd := NewExecdClient(srv.URL, "tok", WithTimeout(50*time.Millisecond))

	start := time.Now()
	err := execd.Ping(context.Background())

	require.Error(t, err)
	require.LessOrEqual(t, time.Since(start), time.Second)
}

func TestExecdPing_LeavesOtherRequestTimeoutsAlone(t *testing.T) {
	shortenHealthPingTimeout(t, 50*time.Millisecond)
	srv, _ := stallingServer(t, 2)
	const requestTimeout = 300 * time.Millisecond
	execd := NewExecdClient(srv.URL, "tok", WithTimeout(requestTimeout))

	require.Error(t, execd.Ping(context.Background()))
	require.Equal(t, requestTimeout, execd.client.httpClient.Timeout)

	start := time.Now()
	_, err := execd.GetCommandStatus(context.Background(), "cmd-1")
	require.Error(t, err)
	require.LessOrEqual(t, requestTimeout-50*time.Millisecond, time.Since(start), "other requests must keep the configured request timeout")
}
