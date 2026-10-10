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

package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/ingress/pkg/routescope"
	"github.com/alibaba/opensandbox/ingress/pkg/sandbox"
	"github.com/alibaba/opensandbox/ingress/pkg/wake"
	"github.com/stretchr/testify/require"
)

// scriptedWakeProvider resolves with a per-call script; the last entry
// repeats.
type scriptedWakeProvider struct {
	mu        sync.Mutex
	responses []resolveAttempt
	calls     int
	invalids  int
}

type resolveAttempt struct {
	info *sandbox.EndpointInfo
	err  error
}

func (p *scriptedWakeProvider) Start(context.Context) error { return nil }

func (*scriptedWakeProvider) RequiresAuthenticatedRouteScope() {}

func (p *scriptedWakeProvider) Invalidate(sandbox.EndpointTarget) { p.invalids++ }

func (p *scriptedWakeProvider) ResolveEndpoint(_ context.Context, _ sandbox.EndpointTarget) (*sandbox.EndpointInfo, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	call := p.calls
	p.calls++
	if call >= len(p.responses) {
		call = len(p.responses) - 1
	}
	return p.responses[call].info, p.responses[call].err
}

// fakeLifecycle satisfies wake.Lifecycle with a scripted probe sequence.
type fakeLifecycle struct {
	mu         sync.Mutex
	probes     []sandbox.SandboxProbe
	probeErrs  []error
	probeCalls int
	resumes    int
}

func (f *fakeLifecycle) ProbeSandbox(context.Context, sandbox.EndpointTarget) (sandbox.SandboxProbe, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	call := f.probeCalls
	f.probeCalls++
	if call >= len(f.probes) {
		call = len(f.probes) - 1
	}
	var probeErr error
	if call < len(f.probeErrs) {
		probeErr = f.probeErrs[call]
	}
	return f.probes[call], probeErr
}

func (f *fakeLifecycle) ResumeSandbox(context.Context, sandbox.EndpointTarget, string, string) (sandbox.ResumeOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resumes++
	return sandbox.ResumeAccepted, nil
}

func (f *fakeLifecycle) resumeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.resumes
}

type recordingActivity struct {
	mu      sync.Mutex
	entries []string
}

func (a *recordingActivity) Record(namespace, sandboxID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entries = append(a.entries, namespace+"/"+sandboxID)
}

func (a *recordingActivity) snapshot() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.entries...)
}

func newWakeTestProxy(t *testing.T, provider sandbox.Provider, lifecycle wake.Lifecycle, act *recordingActivity, budget wake.Config) *Proxy {
	t.Helper()
	waker, err := wake.NewWaker(budget, lifecycle, act)
	require.NoError(t, err)
	return NewProxy(
		context.Background(),
		provider,
		ModeHeader,
		nil,
		nil,
		&routescope.Verifier{Keys: map[string][]byte{"k": []byte("shared-secret")}},
		WithActivityRecorder(act),
		WithWaker(waker),
	)
}

func fsbInfo(backendURL string) *sandbox.EndpointInfo {
	return &sandbox.EndpointInfo{
		UpstreamURL: backendURL,
		UpstreamHeaders: http.Header{
			sandbox.FastSandboxCredential: []string{"issued-credential"},
		},
	}
}

func TestProxyRecordsActivityForEveryRoutedRequest(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer backend.Close()

	provider := &scriptedWakeProvider{responses: []resolveAttempt{{info: fsbInfo(backend.URL)}}}
	act := &recordingActivity{}
	p := newWakeTestProxy(t, provider, &fakeLifecycle{probes: []sandbox.SandboxProbe{{Phase: sandbox.SandboxPhaseReady}}}, act, wake.Config{ParkBudget: time.Second, ParkMax: 8, RetryInterval: time.Millisecond})

	for _, renewSkip := range []bool{false, true} {
		request := httptest.NewRequest(http.MethodGet, "http://ingress/", nil)
		request.Header.Set(SandboxIngress, fsbScopeVector)
		if renewSkip {
			request.Header.Set(AccessRenew, "skip")
		}
		response := httptest.NewRecorder()
		p.ServeHTTP(response, request)
		require.Equal(t, http.StatusNoContent, response.Code)
	}
	require.Equal(t, []string{"tenant-a/sandbox-123", "tenant-a/sandbox-123"}, act.snapshot())
}

func TestProxyWakesPausedSandboxOnResolutionFailure(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("served"))
	}))
	defer backend.Close()

	provider := &scriptedWakeProvider{responses: []resolveAttempt{
		{err: fmt.Errorf("%w: FastPath resolution temporarily unavailable", sandbox.ErrSandboxNotReady)},
		{info: fsbInfo(backend.URL)},
	}}
	lifecycle := &fakeLifecycle{probes: []sandbox.SandboxProbe{{Phase: sandbox.SandboxPhaseWakeable}, {Phase: sandbox.SandboxPhaseReady}}}
	act := &recordingActivity{}
	p := newWakeTestProxy(t, provider, lifecycle, act, wake.Config{ParkBudget: 5 * time.Second, ParkMax: 8, RetryInterval: time.Millisecond})

	request := httptest.NewRequest(http.MethodGet, "http://ingress/", nil)
	request.Header.Set(SandboxIngress, fsbScopeVector)
	response := httptest.NewRecorder()
	p.ServeHTTP(response, request)

	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, "served", response.Body.String())
	require.Equal(t, 2, provider.calls)    // failed resolution + retry after wake
	require.Equal(t, 1, lifecycle.resumes) // one resume flight
	require.Zero(t, provider.invalids)     // resolution failures never touch the route cache
}

func TestProxyWakeBudgetExhaustionAnswersRetryAfter(t *testing.T) {
	provider := &scriptedWakeProvider{responses: []resolveAttempt{
		{err: fmt.Errorf("%w: FastPath resolution temporarily unavailable", sandbox.ErrSandboxNotReady)},
	}}
	lifecycle := &fakeLifecycle{probes: []sandbox.SandboxProbe{{Phase: sandbox.SandboxPhaseWakeable}}}
	p := newWakeTestProxy(t, provider, lifecycle, &recordingActivity{}, wake.Config{ParkBudget: 120 * time.Millisecond, ParkMax: 8, RetryInterval: 10 * time.Millisecond})

	request := httptest.NewRequest(http.MethodGet, "http://ingress/", nil)
	request.Header.Set(SandboxIngress, fsbScopeVector)
	response := httptest.NewRecorder()
	p.ServeHTTP(response, request)

	require.Equal(t, http.StatusServiceUnavailable, response.Code)
	require.Equal(t, "1", response.Header().Get("Retry-After"))
	// Budget exhaustion must not cancel the restore.
	require.Equal(t, 1, lifecycle.resumes)
}

func TestProxyWakeTerminalSandboxAnswersNotFound(t *testing.T) {
	provider := &scriptedWakeProvider{responses: []resolveAttempt{
		{err: fmt.Errorf("%w: FastPath resolution temporarily unavailable", sandbox.ErrSandboxNotReady)},
	}}
	lifecycle := &fakeLifecycle{probes: []sandbox.SandboxProbe{{Phase: sandbox.SandboxPhaseTerminal}}}
	p := newWakeTestProxy(t, provider, lifecycle, &recordingActivity{}, wake.Config{ParkBudget: time.Second, ParkMax: 8, RetryInterval: time.Millisecond})

	request := httptest.NewRequest(http.MethodGet, "http://ingress/", nil)
	request.Header.Set(SandboxIngress, fsbScopeVector)
	response := httptest.NewRecorder()
	p.ServeHTTP(response, request)

	require.Equal(t, http.StatusNotFound, response.Code)
	require.Zero(t, lifecycle.resumes)
}

func TestProxyStaleRouteReplaysReplayableRequestAfterWake(t *testing.T) {
	var hits hitCounter
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.Equal(t, "hello", string(body))
		if hits.add(1) == 1 {
			w.Header().Set(sandbox.FastSandboxProxyError, "stale_route")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("recovered"))
	}))
	defer backend.Close()

	provider := &scriptedWakeProvider{responses: []resolveAttempt{{info: fsbInfo(backend.URL)}}}
	lifecycle := &fakeLifecycle{probes: []sandbox.SandboxProbe{{Phase: sandbox.SandboxPhaseWakeable}, {Phase: sandbox.SandboxPhaseReady}}}
	p := newWakeTestProxy(t, provider, lifecycle, &recordingActivity{}, wake.Config{ParkBudget: 5 * time.Second, ParkMax: 8, RetryInterval: time.Millisecond})

	request := httptest.NewRequest(http.MethodPost, "http://ingress/exec", strings.NewReader("hello"))
	request.Header.Set(SandboxIngress, fsbScopeVector)
	response := httptest.NewRecorder()
	p.ServeHTTP(response, request)

	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, "recovered", response.Body.String())
	require.Equal(t, 2, hits.load())
	require.Equal(t, 1, lifecycle.resumes)
	require.Positive(t, provider.invalids) // stale route invalidated the cache
}

func TestProxyStaleRouteKeepsFiftyThreeForOversizedBody(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(sandbox.FastSandboxProxyError, "stale_route")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer backend.Close()

	provider := &scriptedWakeProvider{responses: []resolveAttempt{{info: fsbInfo(backend.URL)}}}
	lifecycle := &fakeLifecycle{probes: []sandbox.SandboxProbe{{Phase: sandbox.SandboxPhaseWakeable}, {Phase: sandbox.SandboxPhaseReady}}}
	p := newWakeTestProxy(t, provider, lifecycle, &recordingActivity{}, wake.Config{ParkBudget: time.Second, ParkMax: 8, RetryInterval: time.Millisecond})

	request := httptest.NewRequest(http.MethodPost, "http://ingress/exec", strings.NewReader(strings.Repeat("x", wakeReplayBodyMax+1)))
	request.Header.Set(SandboxIngress, fsbScopeVector)
	response := httptest.NewRecorder()
	p.ServeHTTP(response, request)

	// Not replayable: today's stale-route 503 passes through untouched and
	// no resume is triggered.
	require.Equal(t, http.StatusServiceUnavailable, response.Code)
	require.Equal(t, "1", response.Header().Get("Retry-After"))
	require.Zero(t, lifecycle.resumes)
}

func TestCaptureBodyForReplayShortCircuitsAndBuffers(t *testing.T) {
	newRequest := func(body string, contentLength int64) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "http://ingress/exec", strings.NewReader(body))
		r.ContentLength = contentLength
		return r
	}

	t.Run("bodyless request skips reading", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "http://ingress/", nil)
		r.ContentLength = 0
		copied := captureBodyForReplay(r)
		require.NotNil(t, copied)
		require.Empty(t, copied)
	})

	t.Run("known length within cap is fully buffered", func(t *testing.T) {
		r := newRequest("hello", 5)
		copied := captureBodyForReplay(r)
		require.Equal(t, "hello", string(copied))
		payload, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.Equal(t, "hello", string(payload))
	})

	t.Run("exact cap length is replayable", func(t *testing.T) {
		body := strings.Repeat("x", wakeReplayBodyMax)
		r := newRequest(body, int64(len(body)))
		copied := captureBodyForReplay(r)
		require.Len(t, copied, wakeReplayBodyMax)
	})

	t.Run("known oversized length skips buffering and stays streaming", func(t *testing.T) {
		body := strings.Repeat("x", wakeReplayBodyMax+10)
		r := newRequest(body, int64(len(body)))
		require.Nil(t, captureBodyForReplay(r))
		payload, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.Equal(t, body, string(payload))
	})

	t.Run("chunked oversized body keeps streaming", func(t *testing.T) {
		body := strings.Repeat("y", wakeReplayBodyMax+3)
		r := newRequest(body, -1)
		require.Nil(t, captureBodyForReplay(r))
		payload, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.Equal(t, body, string(payload))
	})

	t.Run("chunked small body is buffered and scratch released", func(t *testing.T) {
		r := newRequest("hello", -1)
		copied := captureBodyForReplay(r)
		require.Equal(t, "hello", string(copied))
		payload, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.Equal(t, "hello", string(payload))
	})

	t.Run("mid-body I/O failure is not marked replayable", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "http://ingress/exec", nil)
		r.ContentLength = -1
		r.Body = bodyWithClose{
			Reader:  io.MultiReader(strings.NewReader("hello"), failingReader{}),
			closeFn: func() error { return nil },
		}
		require.Nil(t, captureBodyForReplay(r))
		// The consumed prefix stays in front of the original body so the
		// forwarded request carries the full stream and the failure
		// resurfaces downstream instead of shipping a truncated body.
		payload, err := io.ReadAll(r.Body)
		require.Equal(t, "hello", string(payload))
		require.Error(t, err)
		require.NotErrorIs(t, err, io.EOF)
	})
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

func TestProxyReplayResolutionFailureCarriesRetryAfter(t *testing.T) {
	// The replay's fresh resolution can still hit a not-ready route right
	// after a resume; that 503 must keep the Retry-After invitation the
	// other 503 branches carry.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(sandbox.FastSandboxProxyError, "stale_route")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer backend.Close()

	provider := &scriptedWakeProvider{responses: []resolveAttempt{
		{info: fsbInfo(backend.URL)},
		{err: fmt.Errorf("%w: FastPath resolution temporarily unavailable", sandbox.ErrSandboxNotReady)},
	}}
	lifecycle := &fakeLifecycle{probes: []sandbox.SandboxProbe{{Phase: sandbox.SandboxPhaseReady}}}
	p := newWakeTestProxy(t, provider, lifecycle, &recordingActivity{}, wake.Config{ParkBudget: time.Second, ParkMax: 8, RetryInterval: time.Millisecond})

	request := httptest.NewRequest(http.MethodPost, "http://ingress/exec", strings.NewReader("hello"))
	request.Header.Set(SandboxIngress, fsbScopeVector)
	response := httptest.NewRecorder()
	p.ServeHTTP(response, request)

	require.Equal(t, http.StatusServiceUnavailable, response.Code)
	require.Equal(t, "1", response.Header().Get("Retry-After"))
}

type hitCounter struct {
	mu   sync.Mutex
	next int
}

func (s *hitCounter) add(delta int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next += delta
	return s.next
}

func (s *hitCounter) load() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.next
}
