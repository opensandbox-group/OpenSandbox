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

package revision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const testSessionToken = "0123456789abcdef0123456789abcdef"

type testEnvelope struct {
	Revision Identity `json:"revision"`
	Payload  []byte   `json:"payload"`
}

type marshalProbe struct {
	called *bool
}

func (p marshalProbe) MarshalJSON() ([]byte, error) {
	*p.called = true
	return nil, errors.New("marshal should not run")
}

func testIdentity() Identity {
	return Identity{
		ControlGeneration: "control-a",
		SubjectGeneration: "subject-a",
		DecisionEpoch:     1,
		VaultRevision:     2,
		PolicyEpoch:       3,
		Digest:            strings.Repeat("a", 64),
	}
}

func startUnixHTTPServer(t *testing.T, handler http.Handler) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "osri-go-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(dir)) })
	path := filepath.Join(dir, "receiver.sock")
	listener, err := net.Listen("unix", path)
	require.NoError(t, err)
	server := &http.Server{Handler: handler}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	return path
}

func writeTestAck(t *testing.T, w http.ResponseWriter, revision *Identity) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	require.NoError(t, json.NewEncoder(w).Encode(struct {
		Revision *Identity `json:"revision"`
	}{Revision: revision}))
}

func TestUnixTransportRoundTripContract(t *testing.T) {
	identity := testIdentity()
	payload := []byte(`{"revision":2,"secret":"never-log-me"}`)
	var active *Identity
	path := startUnixHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer "+testSessionToken, r.Header.Get("Authorization"))
		require.Equal(t, "application/json", r.Header.Get("Content-Type"))
		require.Equal(t, "close", r.Header.Get("Connection"))
		switch r.URL.Path {
		case "/v1/revisions/prepare":
			var request testEnvelope
			require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
			require.Equal(t, identity, request.Revision)
			require.Equal(t, payload, request.Payload)
			writeTestAck(t, w, &identity)
		case "/v1/revisions/commit":
			var request struct {
				Revision Identity `json:"revision"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
			active = &request.Revision
			writeTestAck(t, w, active)
		case "/v1/revisions/abort":
			writeTestAck(t, w, &identity)
		case "/v1/revisions/active":
			writeTestAck(t, w, active)
		default:
			http.NotFound(w, r)
		}
	}))
	transport, err := NewUnixTransport(path, testSessionToken, 1024)
	require.NoError(t, err)
	require.NotContains(t, fmt.Sprintf("%+v", transport), testSessionToken)
	require.NotContains(t, fmt.Sprintf("%#v", transport), testSessionToken)
	readback, err := transport.Readback(context.Background())
	require.NoError(t, err)
	require.Nil(t, readback)

	ack, err := transport.Prepare(context.Background(), identity, payload)
	require.NoError(t, err)
	require.Equal(t, identity, ack)
	ack, err = transport.Commit(context.Background(), identity)
	require.NoError(t, err)
	require.Equal(t, identity, ack)
	readback, err = transport.Readback(context.Background())
	require.NoError(t, err)
	require.Equal(t, identity, *readback)
	ack, err = transport.Abort(context.Background(), identity)
	require.NoError(t, err)
	require.Equal(t, identity, ack)
}

func TestUnixTransportEncodesZeroLengthPayloadAsBase64String(t *testing.T) {
	var encoded json.RawMessage
	path := startUnixHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]json.RawMessage
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		encoded = request["payload"]
		identity := testIdentity()
		writeTestAck(t, w, &identity)
	}))
	transport, err := NewUnixTransport(path, testSessionToken, 1)
	require.NoError(t, err)
	_, err = transport.Prepare(context.Background(), testIdentity(), nil)
	require.NoError(t, err)
	require.JSONEq(t, `""`, string(encoded))
}

func TestUnixTransportDoesNotFollowRedirects(t *testing.T) {
	requests := 0
	path := startUnixHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.Header().Set("Location", "http://revision-ipc/replayed")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	transport, err := NewUnixTransport(path, testSessionToken, 1024)
	require.NoError(t, err)
	_, err = transport.Readback(context.Background())
	require.ErrorIs(t, err, ErrTransportRejected)
	require.Equal(t, 1, requests)
}

func TestUnixTransportRequiresRevisionForCommandAcknowledgement(t *testing.T) {
	path := startUnixHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeTestAck(t, w, nil)
	}))
	transport, err := NewUnixTransport(path, testSessionToken, 1024)
	require.NoError(t, err)
	_, err = transport.Commit(context.Background(), testIdentity())
	require.ErrorIs(t, err, ErrInvalidTransportResponse)
}

func TestUnixTransportRejectsUntrustedResponsesWithoutLeakingBody(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		contentType string
		body        string
	}{
		{"remote rejection", http.StatusConflict, "text/plain", "secret-bearing detail"},
		{"wrong content type", http.StatusOK, "text/plain", `{"revision":null}`},
		{"unknown field", http.StatusOK, "application/json", `{"revision":null,"extra":1}`},
		{"duplicate field", http.StatusOK, "application/json", `{"revision":null,"revision":null}`},
		{"duplicate identity field", http.StatusOK, "application/json", `{"revision":{"controlGeneration":"a","controlGeneration":"b","subjectGeneration":"s","decisionEpoch":1,"vaultRevision":1,"policyEpoch":1,"digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}`},
		{"trailing value", http.StatusOK, "application/json", `{"revision":null}{}`},
		{"invalid identity", http.StatusOK, "application/json", `{"revision":{"controlGeneration":"a","subjectGeneration":"s","decisionEpoch":0,"vaultRevision":1,"policyEpoch":1,"digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}`},
		{"oversized", http.StatusOK, "application/json", strings.Repeat("x", 4097)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := startUnixHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			transport, err := NewUnixTransport(path, testSessionToken, 1024)
			require.NoError(t, err)
			_, err = transport.Readback(context.Background())
			require.Error(t, err)
			require.NotContains(t, err.Error(), "secret-bearing")
		})
	}
}

func TestUnixTransportValidatesConfigurationAndRequests(t *testing.T) {
	identity := testIdentity()
	for _, tc := range []struct {
		path, token string
		limit       int
	}{
		{"relative.sock", testSessionToken, 1},
		{"/tmp/socket", "short", 1},
		{"/tmp/socket", testSessionToken, 0},
	} {
		_, err := NewUnixTransport(tc.path, tc.token, tc.limit)
		require.ErrorIs(t, err, ErrInvalidTransport)
	}
	path := startUnixHTTPServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("invalid request reached transport")
	}))
	transport, err := NewUnixTransport(path, testSessionToken, 3)
	require.NoError(t, err)
	_, err = transport.Prepare(context.Background(), identity, []byte("four"))
	require.ErrorIs(t, err, ErrInvalidTransport)
	identity.Digest = "bad"
	_, err = transport.Commit(context.Background(), identity)
	require.ErrorIs(t, err, ErrInvalidTransport)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = transport.Readback(ctx)
	require.True(t, errors.Is(err, context.Canceled))

	one, err := NewSessionToken()
	require.NoError(t, err)
	two, err := NewSessionToken()
	require.NoError(t, err)
	require.NotEqual(t, one, two)
	require.Len(t, one, 43)
}

func TestUnixTransportChecksCancellationBeforeEncoding(t *testing.T) {
	transport, err := NewUnixTransport("/tmp/not-used.sock", testSessionToken, 1)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	_, err = transport.do(ctx, http.MethodPost, "/not-used", marshalProbe{called: &called})
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, called)
}
