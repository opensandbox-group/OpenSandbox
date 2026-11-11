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
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func subjectTestLifecycleClient(t *testing.T, responder subjectRoundTrip) *SubjectLifecycleClient {
	t.Helper()
	client, err := NewSubjectLifecycleClient("/tmp/not-dialed.sock", testSessionToken, "control-a")
	require.NoError(t, err)
	client.ipc.client.Transport = responder
	return client
}

func lifecycleTestReceipt(t *testing.T, r *http.Request, result LifecycleResult) (LifecycleReceipt, []byte) {
	t.Helper()
	data, err := io.ReadAll(r.Body)
	require.NoError(t, err)
	var command subjectLifecycleCommand
	require.NoError(t, json.Unmarshal(data, &command))
	operation := strings.TrimPrefix(r.URL.Path, "/v2/subjects/")
	return LifecycleReceipt{command.LifecycleSequence, operation, subjectLifecycleDigest(operation, data), result}, data
}

func lifecycleTestResponse(t *testing.T, receipt LifecycleReceipt, statusResponse bool) *http.Response {
	t.Helper()
	data, err := json.Marshal(receipt)
	require.NoError(t, err)
	status := receipt.Result.HTTPStatus
	if statusResponse {
		status = 200
		data = []byte(strings.Replace(string(data), `"lifecycleSequence":`, `"lastSequence":`, 1))
	}
	return subjectHTTPResponse(status, string(data))
}

const lifecycleInitialStatus = `{"lastSequence":0,"operation":null,"commandDigest":null,"result":null}`

func lifecycleRegistered() LifecycleResult { return LifecycleResult{200, "registered", nil} }

func lifecycleUnloaded() LifecycleResult {
	return LifecycleResult{200, "unloaded", &SubjectUnloadResult{true, "none", "terminal"}}
}

func TestSubjectLifecycleConfirmedSequenceAndNoHistory(t *testing.T) {
	var sequences []int64
	client := subjectTestLifecycleClient(t, func(r *http.Request) (*http.Response, error) {
		require.Equal(t, http.MethodPost, r.Method)
		require.True(t, r.Close)
		require.Equal(t, []string{"Bearer " + testSessionToken}, r.Header.Values("Authorization"))
		require.Equal(t, "application/json", r.Header.Get("Content-Type"))
		result := lifecycleRegistered()
		if r.URL.Path == "/v2/subjects/unload" {
			result = lifecycleUnloaded()
		}
		receipt, raw := lifecycleTestReceipt(t, r, result)
		require.LessOrEqual(t, len(raw), maxSubjectMetadataBytes)
		sequences = append(sequences, receipt.LifecycleSequence)
		return lifecycleTestResponse(t, receipt, false), nil
	})
	for i := 0; i < 3; i++ {
		registered, err := client.Register(context.Background(), subjectTestTarget())
		require.NoError(t, err)
		require.Equal(t, "registered", registered.Result.Outcome)
		unloaded, err := client.Unload(context.Background(), subjectTestTarget())
		require.NoError(t, err)
		require.Equal(t, "terminal", unloaded.Result.Unload.Completion)
		require.False(t, client.Pending())
		require.Nil(t, client.pending)
	}
	require.Equal(t, []int64{1, 2, 3, 4, 5, 6}, sequences)
	receipt, err := client.Reconcile(context.Background())
	require.NoError(t, err)
	require.Nil(t, receipt)
}

func TestSubjectLifecycleLostReplyReconcilesConsumedFailure(t *testing.T) {
	for _, result := range []LifecycleResult{
		{400, "invalid_target", nil},
		{409, "subject_conflict", nil},
		{503, "subject_capacity_exhausted", nil},
		{503, "subject_failed", nil},
	} {
		t.Run(result.Outcome, func(t *testing.T) {
			var stored LifecycleReceipt
			var paths []string
			client := subjectTestLifecycleClient(t, func(r *http.Request) (*http.Response, error) {
				paths = append(paths, r.URL.Path)
				if r.URL.Path == "/v2/subjects/lifecycle-status" {
					body, err := io.ReadAll(r.Body)
					require.NoError(t, err)
					require.JSONEq(t, `{"controlGeneration":"control-a"}`, string(body))
					return lifecycleTestResponse(t, stored, true), nil
				}
				stored, _ = lifecycleTestReceipt(t, r, result)
				return lifecycleTestResponse(t, stored, false), nil
			})
			got, err := client.Register(context.Background(), subjectTestTarget())
			require.ErrorIs(t, err, ErrTransportRejected)
			require.Equal(t, stored, got)
			require.False(t, client.Pending())
			require.Equal(t, []string{"/v2/subjects/register", "/v2/subjects/lifecycle-status"}, paths)
			require.Equal(t, int64(1), client.last)
		})
	}
}

func TestSubjectLifecycleRetriesFrozenBytesOnly(t *testing.T) {
	var originals [][]byte
	statusCalls := 0
	client := subjectTestLifecycleClient(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/v2/subjects/lifecycle-status" {
			statusCalls++
			return subjectHTTPResponse(200, lifecycleInitialStatus), nil
		}
		receipt, raw := lifecycleTestReceipt(t, r, lifecycleRegistered())
		originals = append(originals, raw)
		if len(originals) == 1 {
			return nil, errors.New("reply unavailable")
		}
		return lifecycleTestResponse(t, receipt, false), nil
	})
	receipt, err := client.Register(context.Background(), subjectTestTarget())
	require.NoError(t, err)
	require.Equal(t, int64(1), receipt.LifecycleSequence)
	require.Len(t, originals, 2)
	require.Equal(t, originals[0], originals[1])
	require.Equal(t, 1, statusCalls)
	require.False(t, client.Pending())
}

func TestSubjectLifecycleUnconsumedErrorsBoundRetriesAndPreservePending(t *testing.T) {
	for _, status := range []int{400, 401, 409, 413, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			commands, statuses := 0, 0
			var first []byte
			client := subjectTestLifecycleClient(t, func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/v2/subjects/lifecycle-status" {
					statuses++
					return subjectHTTPResponse(200, lifecycleInitialStatus), nil
				}
				commands++
				_, body := lifecycleTestReceipt(t, r, lifecycleRegistered())
				if first == nil {
					first = body
				} else {
					require.Equal(t, first, body)
				}
				return subjectHTTPResponse(status, `{"error":"secret detail"}`), nil
			})
			_, err := client.Register(context.Background(), subjectTestTarget())
			require.ErrorIs(t, err, ErrIndeterminate)
			require.NotContains(t, err.Error(), "secret")
			require.True(t, client.Pending())
			require.Equal(t, 2, commands)
			require.Equal(t, 2, statuses)
			pending := client.pending
			_, err = client.Register(context.Background(), SubjectTarget{"s-another", "g2"})
			require.ErrorIs(t, err, ErrIndeterminate)
			_, err = client.Unload(context.Background(), subjectTestTarget())
			require.ErrorIs(t, err, ErrIndeterminate)
			require.Same(t, pending, client.pending)
			require.Equal(t, 2, commands)
			require.Equal(t, int64(0), client.last)
		})
	}
}

func TestSubjectLifecycleCancellationReconcilesWithFreshContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var stored LifecycleReceipt
	client := subjectTestLifecycleClient(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/v2/subjects/lifecycle-status" {
			return lifecycleTestResponse(t, stored, true), nil
		}
		stored, _ = lifecycleTestReceipt(t, r, lifecycleRegistered())
		cancel()
		return nil, context.Canceled
	})
	_, err := client.Register(ctx, subjectTestTarget())
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, err, ErrIndeterminate)
	require.True(t, client.Pending())
	_, err = client.Unload(context.Background(), subjectTestTarget())
	require.ErrorIs(t, err, ErrIndeterminate)
	receipt, err := client.Reconcile(context.Background())
	require.NoError(t, err)
	require.Equal(t, &stored, receipt)
	require.False(t, client.Pending())
}

func TestSubjectLifecycleCanceledLateSuccessRetainsPending(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var stored LifecycleReceipt
	client := subjectTestLifecycleClient(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/v2/subjects/lifecycle-status" {
			return lifecycleTestResponse(t, stored, true), nil
		}
		stored, _ = lifecycleTestReceipt(t, r, lifecycleRegistered())
		cancel()
		return lifecycleTestResponse(t, stored, false), nil
	})
	_, err := client.Register(ctx, subjectTestTarget())
	require.ErrorIs(t, err, ErrIndeterminate)
	require.ErrorIs(t, err, context.Canceled)
	require.True(t, client.Pending())
	receipt, err := client.Reconcile(context.Background())
	require.NoError(t, err)
	require.Equal(t, &stored, receipt)
}

func TestSubjectLifecycleMalformedSuccessUsesStatus(t *testing.T) {
	for _, kind := range []string{"malformed", "digest mismatch", "operation mismatch", "sequence mismatch", "status mismatch", "oversized"} {
		t.Run(kind, func(t *testing.T) {
			var stored LifecycleReceipt
			statusCalls := 0
			client := subjectTestLifecycleClient(t, func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/v2/subjects/lifecycle-status" {
					statusCalls++
					return lifecycleTestResponse(t, stored, true), nil
				}
				stored, _ = lifecycleTestReceipt(t, r, lifecycleRegistered())
				bad := stored
				switch kind {
				case "malformed":
					return subjectHTTPResponse(200, `{"error":"untrusted detail"}`), nil
				case "oversized":
					return subjectHTTPResponse(200, strings.Repeat(" ", 4097)), nil
				case "digest mismatch":
					bad.CommandDigest = strings.Repeat("0", 64)
				case "operation mismatch":
					bad.Operation, bad.Result = "unload", lifecycleUnloaded()
				case "sequence mismatch":
					bad.LifecycleSequence++
				case "status mismatch":
					bad.Result = LifecycleResult{503, "subject_failed", nil}
				}
				response := lifecycleTestResponse(t, bad, false)
				response.StatusCode = 200
				return response, nil
			})
			got, err := client.Register(context.Background(), subjectTestTarget())
			require.NoError(t, err)
			require.Equal(t, stored, got)
			require.Equal(t, 1, statusCalls)
		})
	}
}

func TestSubjectLifecycleStatusConflictFailsClosed(t *testing.T) {
	for _, kind := range []string{"digest", "operation", "ahead", "behind"} {
		t.Run(kind, func(t *testing.T) {
			var stored LifecycleReceipt
			commands := 0
			client := subjectTestLifecycleClient(t, func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/v2/subjects/lifecycle-status" {
					bad := stored
					switch kind {
					case "digest":
						bad.CommandDigest = strings.Repeat("0", 64)
					case "operation":
						bad.Operation, bad.Result = "unload", lifecycleUnloaded()
					case "ahead":
						bad.LifecycleSequence++
					case "behind":
						bad.LifecycleSequence -= 2
					}
					return lifecycleTestResponse(t, bad, true), nil
				}
				commands++
				stored, _ = lifecycleTestReceipt(t, r, lifecycleRegistered())
				return nil, errors.New("lost response")
			})
			client.last = 3
			_, err := client.Register(context.Background(), subjectTestTarget())
			require.ErrorIs(t, err, ErrLifecycleSequenceConflict)
			require.True(t, client.Pending())
			_, err = client.Unload(context.Background(), subjectTestTarget())
			require.ErrorIs(t, err, ErrIndeterminate)
			require.Equal(t, 1, commands)
		})
	}
}

func TestSubjectLifecycleUnavailableOrInvalidStatusStaysPending(t *testing.T) {
	for _, body := range []string{
		`{"lastSequence":0,"operation":null,"commandDigest":null,"result":null,"extra":1}`,
		`{"lastSequence":0,"operation":"register","commandDigest":null,"result":null}`,
		`{"lastSequence":null,"operation":null,"commandDigest":null,"result":null}`,
		strings.Repeat("x", 4097),
	} {
		client := subjectTestLifecycleClient(t, func(r *http.Request) (*http.Response, error) {
			if r.URL.Path == "/v2/subjects/lifecycle-status" {
				return subjectHTTPResponse(200, body), nil
			}
			return nil, errors.New("lost")
		})
		_, err := client.Register(context.Background(), subjectTestTarget())
		require.ErrorIs(t, err, ErrIndeterminate)
		require.True(t, client.Pending())
	}
	for _, status := range []int{401, 500, 503} {
		client := subjectTestLifecycleClient(t, func(*http.Request) (*http.Response, error) {
			return subjectHTTPResponse(status, `{"error":"internal_error"}`), nil
		})
		_, err := client.Register(context.Background(), subjectTestTarget())
		require.ErrorIs(t, err, ErrIndeterminate)
		require.True(t, client.Pending())
	}
}

func TestSubjectLifecycleFailedCloseConsumesThenNewSequenceRetries(t *testing.T) {
	var stored LifecycleReceipt
	commands := 0
	client := subjectTestLifecycleClient(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/v2/subjects/lifecycle-status" {
			return lifecycleTestResponse(t, stored, true), nil
		}
		commands++
		result := lifecycleUnloaded()
		if commands == 1 {
			result = LifecycleResult{503, "subject_failed", &SubjectUnloadResult{true, "none", "fenced"}}
		}
		stored, _ = lifecycleTestReceipt(t, r, result)
		return lifecycleTestResponse(t, stored, false), nil
	})
	failed, err := client.Unload(context.Background(), subjectTestTarget())
	require.ErrorIs(t, err, ErrTransportRejected)
	require.Equal(t, "fenced", failed.Result.Unload.Completion)
	require.False(t, client.Pending())
	succeeded, err := client.Unload(context.Background(), subjectTestTarget())
	require.NoError(t, err)
	require.Equal(t, int64(2), succeeded.LifecycleSequence)
	require.Equal(t, "terminal", succeeded.Result.Unload.Completion)
	require.Equal(t, 2, commands)
}

func TestSubjectLifecyclePreflightExhaustionAndRedaction(t *testing.T) {
	for _, control := range []string{"", "\xff", strings.Repeat("x", 129)} {
		_, err := NewSubjectLifecycleClient("/tmp/socket", testSessionToken, control)
		require.ErrorIs(t, err, ErrInvalidTransport)
	}
	client := subjectTestLifecycleClient(t, func(r *http.Request) (*http.Response, error) {
		receipt, _ := lifecycleTestReceipt(t, r, lifecycleRegistered())
		return lifecycleTestResponse(t, receipt, false), nil
	})
	_, err := client.Register(nil, subjectTestTarget())
	require.ErrorIs(t, err, ErrInvalidTransport)
	_, err = client.Register(context.Background(), SubjectTarget{strings.Repeat("x", 257), "g"})
	require.ErrorIs(t, err, ErrInvalidTransport)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = client.Register(ctx, subjectTestTarget())
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, client.Pending())
	require.Equal(t, int64(0), client.last)
	client.last = math.MaxInt64 - 1
	receipt, err := client.Register(context.Background(), subjectTestTarget())
	require.NoError(t, err)
	require.Equal(t, int64(math.MaxInt64), receipt.LifecycleSequence)
	_, err = client.Register(context.Background(), subjectTestTarget())
	require.ErrorIs(t, err, ErrLifecycleSequenceExhausted)
	require.False(t, client.Pending())
	client.pending = &pendingSubjectLifecycle{raw: []byte("private raw command")}
	for _, format := range []string{"%v", "%+v", "%#v"} {
		printed := fmt.Sprintf(format, client)
		require.NotContains(t, printed, testSessionToken)
		require.NotContains(t, printed, "private raw command")
		require.NotContains(t, printed, "control-a")
	}
}

func TestSubjectLifecycleConcurrentCallsCannotReplacePending(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	client := subjectTestLifecycleClient(t, func(*http.Request) (*http.Response, error) {
		once.Do(func() { close(started); <-release })
		return nil, errors.New("unavailable")
	})
	results := make(chan error, 2)
	go func() { _, err := client.Register(context.Background(), subjectTestTarget()); results <- err }()
	<-started
	go func() { _, err := client.Unload(context.Background(), subjectTestTarget()); results <- err }()
	close(release)
	require.ErrorIs(t, <-results, ErrIndeterminate)
	require.ErrorIs(t, <-results, ErrIndeterminate)
	require.Equal(t, int64(1), client.pending.sequence)
	require.Equal(t, "register", client.pending.operation)
}

func TestSubjectLifecycleDigestGolden(t *testing.T) {
	raw := []byte(`{"controlGeneration":"control-test","lifecycleSequence":1,"target":{"subjectKey":"s-sandbox-a","subjectGeneration":"subject-a-generation-1"}}`)
	require.Equal(t, "e16efb0460b8a00399e49ee90c7cc777fdabda7beaed808519927351da92a32e", subjectLifecycleDigest("register", raw))
	require.NotEqual(t, subjectLifecycleDigest("register", raw), subjectLifecycleDigest("unload", raw))
	require.NotEqual(t, subjectLifecycleDigest("register", raw), subjectLifecycleDigest("register", append(raw, ' ')))
}

func TestSubjectLifecycleClosedReceiptSchema(t *testing.T) {
	digest := strings.Repeat("a", 64)
	valid := fmt.Sprintf(`{"lifecycleSequence":1,"operation":"register","commandDigest":"%s","result":{"httpStatus":200,"outcome":"registered","unload":null}}`, digest)
	for name, body := range map[string]string{
		"extra top":              strings.Replace(valid, `"operation":`, `"extra":1,"operation":`, 1),
		"duplicate top":          strings.Replace(valid, `"lifecycleSequence":1`, `"lifecycleSequence":1,"lifecycleSequence":1`, 1),
		"missing top":            strings.Replace(valid, `"lifecycleSequence":1,`, ``, 1),
		"null sequence":          strings.Replace(valid, `"lifecycleSequence":1`, `"lifecycleSequence":null`, 1),
		"bool sequence":          strings.Replace(valid, `"lifecycleSequence":1`, `"lifecycleSequence":true`, 1),
		"float sequence":         strings.Replace(valid, `"lifecycleSequence":1`, `"lifecycleSequence":1.0`, 1),
		"negative sequence":      strings.Replace(valid, `"lifecycleSequence":1`, `"lifecycleSequence":-1`, 1),
		"overflow sequence":      strings.Replace(valid, `"lifecycleSequence":1`, `"lifecycleSequence":9223372036854775808`, 1),
		"unknown operation":      strings.Replace(valid, `"operation":"register"`, `"operation":"delete"`, 1),
		"uppercase digest":       strings.Replace(valid, digest, strings.ToUpper(digest), 1),
		"null status":            strings.Replace(valid, `"httpStatus":200`, `"httpStatus":null`, 1),
		"float status":           strings.Replace(valid, `"httpStatus":200`, `"httpStatus":200.0`, 1),
		"wrong status":           strings.Replace(valid, `"httpStatus":200`, `"httpStatus":503`, 1),
		"extra result":           strings.Replace(valid, `"outcome":`, `"target":"secret","outcome":`, 1),
		"duplicate result":       strings.Replace(valid, `"httpStatus":200`, `"httpStatus":200,"httpStatus":200`, 1),
		"unconsumed outcome":     strings.Replace(valid, `"registered"`, `"malformed_request"`, 1),
		"wrong operation result": strings.Replace(valid, `"operation":"register"`, `"operation":"unload"`, 1),
		"unicode surrogate":      strings.Replace(valid, `"registered"`, `"\ud800"`, 1),
		"invalid utf8":           strings.Replace(valid, `"registered"`, "\"\xff\"", 1),
		"trailing":               valid + ` {}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := decodeLifecycleRecord([]byte(body), "lifecycleSequence", false)
			require.ErrorIs(t, err, ErrInvalidTransportResponse)
		})
	}
	for _, pair := range []struct {
		operation, outcome string
		status             int
		completion         string
	}{
		{"register", "registered", 200, ""}, {"register", "already_registered", 200, ""},
		{"register", "invalid_target", 400, ""}, {"register", "subject_conflict", 409, ""},
		{"register", "subject_capacity_exhausted", 503, ""}, {"register", "subject_failed", 503, ""},
		{"unload", "unloaded", 200, "terminal"}, {"unload", "already_absent", 200, "terminal"},
		{"unload", "invalid_target", 400, ""}, {"unload", "subject_generation_mismatch", 409, ""},
		{"unload", "subject_failed", 503, "fenced"},
	} {
		result := LifecycleResult{pair.status, pair.outcome, nil}
		if pair.completion != "" {
			result.Unload = &SubjectUnloadResult{true, "none", pair.completion}
		}
		data, err := json.Marshal(LifecycleReceipt{math.MaxInt64, pair.operation, digest, result})
		require.NoError(t, err)
		_, err = decodeLifecycleRecord(data, "lifecycleSequence", false)
		require.NoError(t, err, string(data))
		require.LessOrEqual(t, len(data), maxIPCResponseBytes)
		if pair.completion == "" {
			continue
		}
		for _, bad := range []string{
			strings.Replace(string(data), `"routingRevoked":true`, `"routingRevoked":false`, 1),
			strings.Replace(string(data), `"routingRevoked":true`, `"routingRevoked":null`, 1),
			strings.Replace(string(data), `"routingRevoked":true`, `"routingRevoked":1`, 1),
			strings.Replace(string(data), `"transportObligations":"none"`, `"transportObligations":"live"`, 1),
			strings.Replace(string(data), `"completion":"`+pair.completion+`"`, `"completion":"unknown"`, 1),
			strings.Replace(string(data), `"routingRevoked":true`, `"routingRevoked":true,"extra":1`, 1),
		} {
			_, err = decodeLifecycleRecord([]byte(bad), "lifecycleSequence", false)
			require.ErrorIs(t, err, ErrInvalidTransportResponse, bad)
		}
	}
	initial, err := decodeLifecycleRecord([]byte(lifecycleInitialStatus), "lastSequence", true)
	require.NoError(t, err)
	require.Zero(t, initial.LifecycleSequence)
}
