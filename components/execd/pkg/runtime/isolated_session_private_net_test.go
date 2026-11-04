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

//go:build !windows

package runtime

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/alibaba/opensandbox/execd/pkg/isolation"
)

func TestValidateShareNetAvailable(t *testing.T) {
	privateNet := false
	tests := []struct {
		name    string
		opts    *IsolatedSessionOptions
		caps    isolation.Capabilities
		wantErr error
	}{
		{
			name: "shared net is never gated",
			opts: &IsolatedSessionOptions{},
			caps: isolation.Capabilities{},
		},
		{
			name: "omitted share_net keeps the legacy shared default",
			opts: &IsolatedSessionOptions{ShareNet: nil},
			caps: isolation.Capabilities{},
		},
		{
			name: "private net with capable setpriv mode",
			opts: &IsolatedSessionOptions{ShareNet: &privateNet},
			caps: isolation.Capabilities{SetprivNetAvailable: true},
		},
		{
			name: "private net with capable userns mode",
			opts: &IsolatedSessionOptions{ShareNet: &privateNet, UidMode: "userns"},
			caps: isolation.Capabilities{UsernsNetAvailable: true},
		},
		{
			name:    "private net with default setpriv mode unavailable",
			opts:    &IsolatedSessionOptions{ShareNet: &privateNet},
			caps:    isolation.Capabilities{UsernsNetAvailable: true},
			wantErr: ErrSessionPrivateNetUnavailable,
		},
		{
			name:    "private net with unavailable userns mode",
			opts:    &IsolatedSessionOptions{ShareNet: &privateNet, UidMode: "userns"},
			caps:    isolation.Capabilities{SetprivNetAvailable: true},
			wantErr: ErrSessionPrivateNetUnavailable,
		},
		{
			name:    "unknown uid mode is deferred to start",
			opts:    &IsolatedSessionOptions{ShareNet: &privateNet, UidMode: "bogus"},
			caps:    isolation.Capabilities{},
			wantErr: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := &IsolatedRunner{isolator: &stubIsolator{caps: tt.caps}}
			err := runner.validateShareNetAvailable(tt.opts)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("validateShareNetAvailable() error = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr != nil && !strings.Contains(err.Error(), "share_net=false") {
				t.Errorf("error %q does not explain the share_net=false requirement", err)
			}
		})
	}
}

// closedLifecycleStub is a WorkloadLifecycle whose status stream already
// reached its terminal state, optionally carrying an exit code.
type closedLifecycleStub struct {
	done    chan struct{}
	code    int
	hasCode bool
}

func newClosedLifecycleStub(code int, hasCode bool) *closedLifecycleStub {
	done := make(chan struct{})
	close(done)
	return &closedLifecycleStub{done: done, code: code, hasCode: hasCode}
}

func (*closedLifecycleStub) WaitForIdentity(
	context.Context,
) (isolation.WorkloadIdentity, error) {
	return isolation.WorkloadIdentity{}, errors.New("not implemented")
}

func (*closedLifecycleStub) MarkReady() error { return errors.New("not implemented") }
func (*closedLifecycleStub) Abort()           {}
func (s *closedLifecycleStub) DrainDone() <-chan struct{} {
	return s.done
}
func (*closedLifecycleStub) DrainError() error { return nil }
func (s *closedLifecycleStub) ExitCode() (int, bool) {
	return s.code, s.hasCode
}
func (*closedLifecycleStub) Close() error { return nil }

// newEvidenceTestSession builds a session whose process already started and
// whose output pipe carries the given bytes (bwrap's stderr is merged into
// the session output). The channel state mirrors a reaped workload so
// failStartup teardown does not wait.
func newEvidenceTestSession(
	t *testing.T,
	pipeData []byte,
	closeWriter bool,
	lifecycle isolation.WorkloadLifecycle,
) *isolatedSession {
	t.Helper()

	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start evidence probe process: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Wait() })

	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatalf("create output pipe: %v", err)
	}
	t.Cleanup(func() { _ = stdoutR.Close() })
	if len(pipeData) > 0 {
		if _, err := stdoutW.Write(pipeData); err != nil {
			t.Fatalf("write evidence bytes: %v", err)
		}
	}
	if closeWriter {
		if err := stdoutW.Close(); err != nil {
			t.Fatalf("close output writer: %v", err)
		}
	} else {
		t.Cleanup(func() { _ = stdoutW.Close() })
	}

	processWaited := make(chan struct{})
	close(processWaited)
	doneCh := make(chan struct{})
	close(doneCh)
	return &isolatedSession{
		cmd:           cmd,
		stdout:        stdoutR,
		lifecycle:     lifecycle,
		processWaited: processWaited,
		processExited: true,
		doneCh:        doneCh,
	}
}

func TestFailStartupAttachesBwrapEvidence(t *testing.T) {
	lifecycle := newClosedLifecycleStub(1, true)
	session := newEvidenceTestSession(
		t,
		[]byte("bwrap: loopback: Failed RTM_NEWADDR: Operation not permitted\n"),
		true,
		lifecycle,
	)

	startErr := errors.New(
		"start bwrap: wait for isolated workload identity: read native workload gate: EOF",
	)
	err := session.failStartup(startErr)

	if !errors.Is(err, startErr) {
		t.Fatalf("failStartup error %v does not wrap the original error", err)
	}
	if !strings.Contains(err.Error(), "bwrap exit code 1") {
		t.Errorf("error %q does not carry the bwrap exit code", err)
	}
	if !strings.Contains(err.Error(), "session output tail (bwrap stderr merged): bwrap: loopback: Failed RTM_NEWADDR") {
		t.Errorf("error %q does not carry the bwrap stderr tail", err)
	}
}

func TestFailStartupWithoutExitCodeKeepsStderrEvidence(t *testing.T) {
	// Setup-stage bwrap deaths end the status stream without an exit-code
	// document; the stderr tail must still be attached.
	lifecycle := newClosedLifecycleStub(0, false)
	session := newEvidenceTestSession(
		t,
		[]byte("bwrap: Can't mount proc at /proc: Operation not permitted\n"),
		true,
		lifecycle,
	)

	startErr := errors.New(
		"start bwrap: wait for isolated workload identity: read native workload gate: EOF",
	)
	err := session.failStartup(startErr)

	if !errors.Is(err, startErr) {
		t.Fatalf("failStartup error %v does not wrap the original error", err)
	}
	if strings.Contains(err.Error(), "bwrap exit code") {
		t.Errorf("error %q unexpectedly carries an exit code", err)
	}
	if !strings.Contains(err.Error(), "session output tail (bwrap stderr merged): bwrap: Can't mount proc") {
		t.Errorf("error %q does not carry the bwrap stderr tail", err)
	}
}

func TestFailStartupKeepsOutputTailWhenLarge(t *testing.T) {
	// A gate-released workload may write more than the tail window before
	// dying; the evidence must keep the END of the stream (the root cause)
	// and drop the head instead of stopping at the first few kilobytes.
	lifecycle := newClosedLifecycleStub(0, false)
	headMarker := "first-line-must-be-dropped\n"
	rootCause := "bwrap: loopback: Failed RTM_NEWADDR: Operation not permitted\n"
	session := newEvidenceTestSession(t, []byte(headMarker+strings.Repeat("A", 4000)+"\n"+rootCause), true, lifecycle)

	err := session.failStartup(errors.New("start bwrap: wait for isolated workload identity: EOF"))

	if !strings.Contains(err.Error(), "session output tail (bwrap stderr merged)") {
		t.Errorf("error %q does not carry the session output tail", err)
	}
	if !strings.Contains(err.Error(), strings.TrimSpace(rootCause)) {
		t.Errorf("error %q does not keep the end of the output stream", err)
	}
	if strings.Contains(err.Error(), "first-line-must-be-dropped") {
		t.Errorf("error %q kept evicted head output instead of the tail", err)
	}
}

func TestFailStartupWithoutEvidenceReturnsOriginalError(t *testing.T) {
	lifecycle := newClosedLifecycleStub(0, false)
	// The writer stays open: the read must honor its short deadline and the
	// original error must pass through unchanged.
	session := newEvidenceTestSession(t, nil, false, lifecycle)

	startErr := errors.New("start bwrap: bwrap: binary not found")
	err := session.failStartup(startErr)

	if err.Error() != startErr.Error() {
		t.Fatalf("failStartup error = %q, want unchanged %q", err, startErr)
	}
}
