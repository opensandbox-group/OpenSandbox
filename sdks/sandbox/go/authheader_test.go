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
	"testing"
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
