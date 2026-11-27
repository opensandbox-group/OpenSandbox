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
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHasHeaderName_MatchesAnyCasing(t *testing.T) {
	headers := http.Header{}
	require.True(t, !hasHeaderName(headers, "OPEN-SANDBOX-API-KEY"))

	headers.Set("open-sandbox-api-key", "explicit-key")
	require.True(t, hasHeaderName(headers, "OPEN-SANDBOX-API-KEY"))
	require.True(t, hasHeaderName(headers, "Open-Sandbox-Api-Key"))
}

func TestHasHeaderName_PresentEmptyValueCountsAsPresent(t *testing.T) {
	headers := http.Header{}
	headers.Set("OPEN-SANDBOX-API-KEY", "")
	require.True(t, hasHeaderName(headers, "OPEN-SANDBOX-API-KEY"))
}

func TestNewClient_ExplicitAuthHeaderWinsOverAPIKey(t *testing.T) {
	var gotKey string
	var values []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("OPEN-SANDBOX-API-KEY")
		values = r.Header.Values("OPEN-SANDBOX-API-KEY")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "configured-key", "OPEN-SANDBOX-API-KEY",
		WithHeaders(map[string]string{"OPEN-SANDBOX-API-KEY": "explicit-key"}))
	err := c.doRequest(context.Background(), http.MethodGet, "/", nil, nil)
	require.NoError(t, err)
	require.Equal(t, "explicit-key", gotKey)
	require.Equal(t, []string{"explicit-key"}, values)
}

func TestNewClient_ExplicitLowercaseAuthHeaderWins(t *testing.T) {
	var values []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		values = r.Header.Values("OPEN-SANDBOX-API-KEY")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "configured-key", "OPEN-SANDBOX-API-KEY",
		WithHeaders(map[string]string{"open-sandbox-api-key": "explicit-key"}))
	err := c.doRequest(context.Background(), http.MethodGet, "/", nil, nil)
	require.NoError(t, err)
	require.Equal(t, []string{"explicit-key"}, values)
}

func TestNewClient_ExplicitEmptyAuthHeaderIsPreserved(t *testing.T) {
	var values []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		values = r.Header.Values("OPEN-SANDBOX-API-KEY")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "configured-key", "OPEN-SANDBOX-API-KEY",
		WithHeaders(map[string]string{"OPEN-SANDBOX-API-KEY": ""}))
	err := c.doRequest(context.Background(), http.MethodGet, "/", nil, nil)
	require.NoError(t, err)
	require.Equal(t, []string{""}, values)
}

func TestNewClient_DefaultAPIKeyStillAppliedWithoutExplicitHeader(t *testing.T) {
	var gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("OPEN-SANDBOX-API-KEY")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "configured-key", "OPEN-SANDBOX-API-KEY",
		WithHeaders(map[string]string{"X-Custom": "value"}))
	err := c.doRequest(context.Background(), http.MethodGet, "/", nil, nil)
	require.NoError(t, err)
	require.Equal(t, "configured-key", gotKey)
}

// rawAuthCapturingServer records the execd auth header of each request.
func rawAuthCapturingServer(t *testing.T, respond func(w http.ResponseWriter)) (*httptest.Server, *[]string) {
	t.Helper()
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get(execdAuthHeader))
		respond(w)
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func newExecdClientWithExplicitToken(url string) *ExecdClient {
	return NewExecdClient(url, "configured-token",
		WithHeaders(map[string]string{execdAuthHeader: "explicit-token"}))
}

func TestExecdClient_ExplicitTokenWinsOnGetCommandLogs(t *testing.T) {
	srv, got := rawAuthCapturingServer(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{}"))
	})
	c := newExecdClientWithExplicitToken(srv.URL)
	_, _ = c.GetCommandLogs(context.Background(), "cmd-1", nil)
	require.Equal(t, []string{"explicit-token"}, *got)
}

func TestExecdClient_ExplicitLowercaseTokenWinsOnGetCommandLogs(t *testing.T) {
	srv, got := rawAuthCapturingServer(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{}"))
	})
	c := NewExecdClient(srv.URL, "configured-token",
		WithHeaders(map[string]string{"x-execd-access-token": "explicit-token"}))
	_, _ = c.GetCommandLogs(context.Background(), "cmd-1", nil)
	require.Equal(t, []string{"explicit-token"}, *got)
}

func TestExecdClient_ExplicitTokenWinsOnUploadFiles(t *testing.T) {
	srv, got := rawAuthCapturingServer(t, func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusNoContent)
	})
	c := newExecdClientWithExplicitToken(srv.URL)
	_ = c.UploadFiles(context.Background(), []UploadFileEntry{{
		File:    strings.NewReader("body"),
		Options: UploadFileOptions{FileName: "a.txt", Metadata: FileMetadata{Path: "/tmp/a.txt"}},
	}})
	require.Equal(t, []string{"explicit-token"}, *got)
}

func TestExecdClient_ExplicitTokenWinsOnDownloadFileResponse(t *testing.T) {
	srv, got := rawAuthCapturingServer(t, func(w http.ResponseWriter) {
		_, _ = w.Write([]byte("bytes"))
	})
	c := newExecdClientWithExplicitToken(srv.URL)
	_, _ = c.DownloadFileResponse(context.Background(), "/tmp/a.txt", "")
	require.Equal(t, []string{"explicit-token"}, *got)
}

func TestExecdClient_ExplicitTokenWinsOnIsolatedRunLogs(t *testing.T) {
	srv, got := rawAuthCapturingServer(t, func(w http.ResponseWriter) {
		_, _ = w.Write([]byte("log text"))
	})
	c := newExecdClientWithExplicitToken(srv.URL)
	_, _, _ = c.IsolatedRunLogs(context.Background(), "sess-1", "run-1", 0)
	require.Equal(t, []string{"explicit-token"}, *got)
}

func TestExecdClient_ExplicitTokenWinsOnStreamRequest(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get(execdAuthHeader)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"execution_complete\"}\n\n")
	}))
	defer srv.Close()

	c := newExecdClientWithExplicitToken(srv.URL)
	err := c.RunCommand(context.Background(), RunCommandRequest{Command: "echo hi"}, func(StreamEvent) error {
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, "explicit-token", got)
}

func TestReportSandboxCreateMetric_ExplicitAuthHeaderWins(t *testing.T) {
	var got atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Store(r.Header.Get("OPEN-SANDBOX-API-KEY"))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	cfg := ConnectionConfig{
		Domain:   server.URL,
		Protocol: "http",
		APIKey:   "configured-key",
		Headers:  map[string]string{"OPEN-SANDBOX-API-KEY": "explicit-key"},
	}
	reportSandboxCreateMetric(cfg, "sbx", "python:3.12", 42, true)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if v, ok := got.Load().(string); ok {
			require.Equal(t, "explicit-key", v)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("metrics POST was not received")
}

func TestReportSandboxCreateMetric_ExplicitEmptyAuthHeaderSuppressesKey(t *testing.T) {
	var got atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Store(r.Header.Get("OPEN-SANDBOX-API-KEY"))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	cfg := ConnectionConfig{
		Domain:   server.URL,
		Protocol: "http",
		APIKey:   "configured-key",
		Headers:  map[string]string{"OPEN-SANDBOX-API-KEY": ""},
	}
	reportSandboxCreateMetric(cfg, "sbx", "python:3.12", 42, true)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if v, ok := got.Load().(string); ok {
			require.Equal(t, "", v)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("metrics POST was not received")
}
