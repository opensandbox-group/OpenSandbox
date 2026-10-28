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
	"math"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// RoundTripper tests exercise framing and state without claiming real Unix IPC.
// The separate cross-language integration test supplies that acceptance signal.
type subjectRoundTrip func(*http.Request) (*http.Response, error)

func (f subjectRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func subjectHTTPResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}

func subjectTestTarget() SubjectTarget { return SubjectTarget{"s-sandbox-a", "subject-a"} }

func subjectTestTransport(t *testing.T, responder subjectRoundTrip) *SubjectUnixTransport {
	t.Helper()
	transport, err := NewSubjectUnixTransport("/tmp/not-dialed.sock", testSessionToken, "control-a", subjectTestTarget(), 1024)
	require.NoError(t, err)
	transport.ipc.client.Transport = responder
	return transport
}

func TestSubjectTransportRoundTripContract(t *testing.T) {
	identity := testIdentity()
	payload := []byte(`{"raw":"unchanged"}`)
	var paths []string
	transport := subjectTestTransport(t, func(r *http.Request) (*http.Response, error) {
		paths = append(paths, r.URL.Path)
		require.Equal(t, http.MethodPost, r.Method)
		require.True(t, r.Close)
		require.Equal(t, []string{"Bearer " + testSessionToken}, r.Header.Values("Authorization"))
		require.Equal(t, "application/json", r.Header.Get("Content-Type"))
		var body map[string]json.RawMessage
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		var target SubjectTarget
		require.NoError(t, json.Unmarshal(body["target"], &target))
		require.Equal(t, subjectTestTarget(), target)
		if strings.HasSuffix(r.URL.Path, "/active") {
			require.Len(t, body, 2)
			require.JSONEq(t, `"control-a"`, string(body["controlGeneration"]))
		} else {
			var got Identity
			require.NoError(t, json.Unmarshal(body["revision"], &got))
			require.Equal(t, identity, got)
			if strings.HasSuffix(r.URL.Path, "/prepare") {
				require.Len(t, body, 3)
				var got []byte
				require.NoError(t, json.Unmarshal(body["payload"], &got))
				require.Equal(t, payload, got)
			} else {
				require.Len(t, body, 2)
			}
		}
		bodyJSON, err := json.Marshal(ipcResponse{Revision: &identity})
		require.NoError(t, err)
		return subjectHTTPResponse(200, string(bodyJSON)), nil
	})
	for _, run := range []func() (Identity, error){
		func() (Identity, error) { return transport.Prepare(context.Background(), identity, payload) },
		func() (Identity, error) { return transport.Commit(context.Background(), identity) },
		func() (Identity, error) { return transport.Abort(context.Background(), identity) },
	} {
		got, err := run()
		require.NoError(t, err)
		require.Equal(t, identity, got)
	}
	got, err := transport.Readback(context.Background())
	require.NoError(t, err)
	require.Equal(t, &identity, got)
	require.Equal(t, []string{"/v2/revisions/prepare", "/v2/revisions/commit", "/v2/revisions/abort", "/v2/revisions/active"}, paths)
	for _, format := range []string{"%v", "%+v", "%#v"} {
		require.NotContains(t, fmt.Sprintf(format, transport), testSessionToken)
		require.NotContains(t, fmt.Sprintf(format, transport), "control-a")
	}
}

func TestSubjectTransportPreflightAndBinding(t *testing.T) {
	for _, target := range []SubjectTarget{{}, {"s", ""}, {strings.Repeat("a", 257), "g"}, {"s", strings.Repeat("g", 129)}, {"\xff", "g"}} {
		_, err := NewSubjectUnixTransport("/tmp/socket", testSessionToken, "control-a", target, 1)
		require.ErrorIs(t, err, ErrInvalidTransport)
	}
	for _, control := range []string{"", "\xff", strings.Repeat("x", 129)} {
		_, err := NewSubjectUnixTransport("/tmp/socket", testSessionToken, control, subjectTestTarget(), 1)
		require.ErrorIs(t, err, ErrInvalidTransport)
	}
	for _, limit := range []int{0, -1, math.MaxInt} {
		_, err := NewSubjectUnixTransport("/tmp/socket", testSessionToken, "control-a", subjectTestTarget(), limit)
		require.ErrorIs(t, err, ErrInvalidTransport)
	}
	_, err := NewSubjectUnixTransport("relative", testSessionToken, "control-a", subjectTestTarget(), 1)
	require.ErrorIs(t, err, ErrInvalidTransport)
	_, err = NewSubjectUnixTransport("/tmp/socket", "short", "control-a", subjectTestTarget(), 1)
	require.ErrorIs(t, err, ErrInvalidTransport)
	transport := subjectTestTransport(t, func(*http.Request) (*http.Response, error) {
		t.Fatal("preflight failed to prevent request")
		return nil, nil
	})
	for _, field := range []string{"control", "subject", "digest"} {
		identity := testIdentity()
		switch field {
		case "control":
			identity.ControlGeneration = "other"
		case "subject":
			identity.SubjectGeneration = "other"
		case "digest":
			identity.Digest = "invalid"
		}
		_, err = transport.Commit(context.Background(), identity)
		require.ErrorIs(t, err, ErrInvalidTransport)
	}
	_, err = transport.Prepare(context.Background(), testIdentity(), make([]byte, 1025))
	require.ErrorIs(t, err, ErrInvalidTransport)
	_, err = transport.Readback(nil)
	require.ErrorIs(t, err, ErrInvalidTransport)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = transport.Readback(ctx)
	require.ErrorIs(t, err, context.Canceled)
}

func TestSubjectTransportStrictResponses(t *testing.T) {
	identity, _ := json.Marshal(testIdentity())
	valid := `{"revision":` + string(identity) + `}`
	cases := map[string]string{
		"extra":            strings.Replace(valid, `"revision":`, `"extra":0,"revision":`, 1),
		"duplicate":        strings.Replace(valid, `"decisionEpoch":1`, `"decisionEpoch":1,"decisionEpoch":1`, 1),
		"null integer":     strings.Replace(valid, `"vaultRevision":2`, `"vaultRevision":null`, 1),
		"float integer":    strings.Replace(valid, `"policyEpoch":3`, `"policyEpoch":3.0`, 1),
		"bool integer":     strings.Replace(valid, `"decisionEpoch":1`, `"decisionEpoch":true`, 1),
		"overflow integer": strings.Replace(valid, `"decisionEpoch":1`, `"decisionEpoch":9223372036854775808`, 1),
		"invalid utf8":     strings.Replace(valid, "control-a", "\xff", 1),
		"unpaired high":    strings.Replace(valid, "control-a", `\ud800`, 1),
		"unpaired low":     strings.Replace(valid, "control-a", `\udfff`, 1),
		"wrong pair":       strings.Replace(valid, "control-a", `\ud800\u0061`, 1),
		"trailing":         valid + `{}`,
		"huge":             strings.Repeat(" ", 4097),
		"wrong control":    strings.Replace(valid, "control-a", "other", 1),
		"wrong subject":    strings.Replace(valid, "subject-a", "other", 1),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			transport := subjectTestTransport(t, func(*http.Request) (*http.Response, error) { return subjectHTTPResponse(200, body), nil })
			_, err := transport.Readback(context.Background())
			require.ErrorIs(t, err, ErrInvalidTransportResponse)
		})
	}
	t.Run("unknown versus uninstalled", func(t *testing.T) {
		transport := subjectTestTransport(t, func(*http.Request) (*http.Response, error) { return subjectHTTPResponse(200, `{"revision":null}`), nil })
		got, err := transport.Readback(context.Background())
		require.NoError(t, err)
		require.Nil(t, got)
		_, err = transport.Commit(context.Background(), testIdentity())
		require.ErrorIs(t, err, ErrInvalidTransportResponse)
		transport.ipc.client.Transport = subjectRoundTrip(func(*http.Request) (*http.Response, error) {
			return subjectHTTPResponse(404, `{"error":"subject_unknown"}`), nil
		})
		_, err = transport.Readback(context.Background())
		require.ErrorIs(t, err, ErrTransportRejected)
	})
	t.Run("sanitized errors", func(t *testing.T) {
		transport := subjectTestTransport(t, func(*http.Request) (*http.Response, error) { return nil, errors.New("secret-value") })
		_, err := transport.Readback(context.Background())
		require.ErrorIs(t, err, ErrTransportUnavailable)
		require.NotContains(t, err.Error(), "secret-value")
	})
}

func TestSubjectTransportUnicodeAndMetadataLimits(t *testing.T) {
	for _, valid := range []string{`"\ud83d\ude00"`, `"\\ud800"`, `"�"`, `{"x":"\u0000"}`} {
		require.True(t, validSubjectJSON([]byte(valid)), valid)
	}
	for _, invalid := range []string{`"\ud83d"`, `"\ude00"`, `"\ud83d\ud83d"`, `"\ud83d\n"`, `"\ud83d\\ude00"`, "\"\xff\""} {
		require.False(t, validSubjectJSON([]byte(invalid)), invalid)
	}
	for _, escape := range []string{"😀", `\ud83d\ude00`, `\u0000`} {
		key, generation := strings.Repeat(escape, 256), strings.Repeat(escape, 128)
		metadata := fmt.Sprintf(`{"target":{"subjectKey":"%s","subjectGeneration":"%s"},"revision":{"controlGeneration":"%s","subjectGeneration":"%s","decisionEpoch":9223372036854775807,"vaultRevision":9223372036854775807,"policyEpoch":9223372036854775807,"digest":"%s"},"payload":""}`, key, generation, generation, generation, strings.Repeat("a", 64))
		require.True(t, validSubjectJSON([]byte(metadata)))
		require.LessOrEqual(t, len(metadata), maxSubjectMetadataBytes)
		lifecycle := fmt.Sprintf(`{"controlGeneration":"%s","lifecycleSequence":9223372036854775807,"target":{"subjectKey":"%s","subjectGeneration":"%s"}}`, generation, key, generation)
		require.True(t, validSubjectJSON([]byte(lifecycle)))
		require.LessOrEqual(t, len(lifecycle), maxSubjectMetadataBytes)
	}
	for size, want := range map[int]int{1: 8196, 2: 8196, 3: 8196, 4: 8200} {
		got, err := subjectBodyLimit(size)
		require.NoError(t, err)
		require.Equal(t, want, got)
	}
	_, err := subjectBodyLimit(math.MaxInt)
	require.ErrorIs(t, err, ErrInvalidTransport)
}
