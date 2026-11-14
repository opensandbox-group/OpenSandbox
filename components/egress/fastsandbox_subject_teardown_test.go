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

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/mitmproxy"
	"github.com/alibaba/opensandbox/egress/pkg/revision"
	"github.com/alibaba/opensandbox/egress/pkg/telemetry"
	"github.com/stretchr/testify/require"
)

// teardownPeer is a strict v2 lifecycle peer. Its first Unload confirms
// routing revocation with unconfirmed proxied transports; a Register that
// follows it reuses the source and is rejected fail-closed, as the Python
// owner does while an earlier client is still live.
type teardownPeer struct {
	mu       sync.Mutex
	commands []string
	receipt  revision.LifecycleReceipt
	dirty    bool
}

func startTeardownPeer(t *testing.T, s *fastSandboxPolicyServer, remaining uint64) *teardownPeer {
	t.Helper()
	o := subjectTestOwner(t)
	path := filepath.Join(o.cfg.parent, "teardown.sock")
	listener, err := net.Listen("unix", path)
	require.NoError(t, err)
	token, err := revision.NewSessionToken()
	require.NoError(t, err)
	p := &teardownPeer{}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/v2/revisions/") {
			var command struct {
				Revision revision.Identity `json:"revision"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&command))
			require.NoError(t, json.NewEncoder(w).Encode(command))
			return
		}
		if strings.HasSuffix(r.URL.Path, "lifecycle-status") {
			raw, _ := json.Marshal(p.receipt)
			_, _ = w.Write([]byte(strings.Replace(string(raw), `"lifecycleSequence":`, `"lastSequence":`, 1)))
			return
		}
		operation := strings.TrimPrefix(r.URL.Path, "/v2/subjects/")
		raw, _ := io.ReadAll(r.Body)
		var command struct {
			Sequence int64                  `json:"lifecycleSequence"`
			Target   revision.SubjectTarget `json:"target"`
		}
		require.NoError(t, json.Unmarshal(raw, &command))
		p.commands = append(p.commands, operation+":"+command.Target.SubjectKey)
		result := revision.LifecycleResult{HTTPStatus: 200, Outcome: "registered"}
		switch {
		case operation == "unload" && remaining > 0:
			closed := uint64(1)
			left := remaining
			result = revision.LifecycleResult{HTTPStatus: 503, Outcome: "subject_failed", Unload: &revision.SubjectUnloadResult{RoutingRevoked: true, TransportObligations: "pending", Completion: "fenced", ClosedConnections: &closed, RemainingConnections: &left}}
			p.dirty = true
		case operation == "unload":
			closed, left := uint64(2), uint64(0)
			result = revision.LifecycleResult{HTTPStatus: 200, Outcome: "unloaded", Unload: &revision.SubjectUnloadResult{RoutingRevoked: true, TransportObligations: "none", Completion: "terminal", ClosedConnections: &closed, RemainingConnections: &left}}
		case p.dirty:
			result = revision.LifecycleResult{HTTPStatus: 503, Outcome: "subject_failed"}
		}
		digest := sha256.Sum256(append([]byte("opensandbox.subject-lifecycle.v2\x00"+operation+"\x00"), raw...))
		p.receipt = revision.LifecycleReceipt{LifecycleSequence: command.Sequence, Operation: operation, CommandDigest: hex.EncodeToString(digest[:]), Result: result}
		w.WriteHeader(result.HTTPStatus)
		require.NoError(t, json.NewEncoder(w).Encode(p.receipt))
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	client, err := revision.NewSubjectLifecycleClient(path, token, "control-test")
	require.NoError(t, err)
	s.startSubjectDrain()
	t.Cleanup(func() { s.subjectDrain.cancel(); <-s.subjectDrain.done })
	s.publishSubjectClient(client, mitmproxy.SubjectRevisionIPCConfig{SocketPath: path, SessionToken: token, ControlGeneration: "control-test", MaxSnapshotBytes: defaultRevisionMaxSnapshotSize})
	return p
}

func (p *teardownPeer) waitCommands(t *testing.T, want ...string) {
	t.Helper()
	require.Eventually(t, func() bool { p.mu.Lock(); defer p.mu.Unlock(); return len(p.commands) >= len(want) }, 3*time.Second, 10*time.Millisecond)
	// A confirmed failure is never followed by an automatic replay.
	require.Never(t, func() bool { p.mu.Lock(); defer p.mu.Unlock(); return len(p.commands) > len(want) }, 100*time.Millisecond, 10*time.Millisecond)
	p.mu.Lock()
	defer p.mu.Unlock()
	require.Equal(t, want, p.commands)
}

func TestFastSandboxSubjectTeardownUnloadPrecedesReuse(t *testing.T) {
	// Rebind (same UID, new fence) and IP takeover (new UID, same IP) both run
	// the existing workflow: the old Unload is sent before the new Register.
	for _, workflow := range []string{"rebind", "takeover"} {
		for _, remaining := range []uint64{0, 2} {
			t.Run(workflow+map[bool]string{true: "-unconfirmed", false: "-confirmed"}[remaining > 0], func(t *testing.T) {
				s, _, _ := fastSandboxTestServer(t)
				p := startTeardownPeer(t, s, remaining)
				require.Equal(t, 200, doAction(t, s, bindBody(t, testUID, testIP, testFenceR, testFenceA, 1, nil)).Code)
				p.waitCommands(t, "register:s-"+testUID)
				successor := "s-" + testUID
				if workflow == "rebind" {
					require.Equal(t, 200, doAction(t, s, bindBody(t, testUID, testIP, "r2", "a2", 2, nil)).Code)
				} else {
					successor = "s-successor"
					require.Equal(t, 200, doAction(t, s, bindBody(t, "successor", testIP, "r3", "a3", 1, nil)).Code)
				}
				p.waitCommands(t, "register:s-"+testUID, "unload:s-"+testUID, "register:"+successor)
				degraded := s.subjectLifecycleDegraded()
				if remaining == 0 {
					require.Empty(t, degraded)
					return
				}
				// Unconfirmed teardown is a closed degraded reason, not a clean
				// unload; the successor Register is a confirmed fail-closed rejection.
				require.Contains(t, degraded, "subject_transport_unconfirmed")
				require.Contains(t, degraded, "subject_failed")
				for _, reason := range degraded {
					require.NotContains(t, reason, testUID)
					require.NotContains(t, reason, testIP)
				}
			})
		}
	}
}

func TestFastSandboxSubjectTeardownReceiptClassification(t *testing.T) {
	closed, left, none := uint64(1), uint64(3), uint64(0)
	unconfirmed := revision.LifecycleReceipt{LifecycleSequence: 4, Result: revision.LifecycleResult{HTTPStatus: 503, Outcome: "subject_failed", Unload: &revision.SubjectUnloadResult{RoutingRevoked: true, TransportObligations: "pending", Completion: "fenced", ClosedConnections: &closed, RemainingConnections: &left}}}
	require.Equal(t, "subject_transport_unconfirmed", subjectLifecycleReason(unconfirmed))
	clean := unconfirmed
	clean.Result = revision.LifecycleResult{HTTPStatus: 503, Outcome: "subject_failed", Unload: &revision.SubjectUnloadResult{RoutingRevoked: true, TransportObligations: "none", Completion: "fenced", ClosedConnections: &closed, RemainingConnections: &none}}
	require.Equal(t, "subject_failed", subjectLifecycleReason(clean))
	legacy := clean
	legacy.Result.Unload = &revision.SubjectUnloadResult{RoutingRevoked: true, TransportObligations: "none", Completion: "fenced"}
	require.Equal(t, "subject_failed", subjectLifecycleReason(legacy))
	// R9 metric results keep their closed meaning: a sequenced receipt is a
	// confirmed rejection whether or not teardown was confirmed.
	unload := fastSandboxSubjectIntent{unload: true}
	require.Equal(t, telemetry.CredentialTransitionRejected, subjectLifecycleResult(unload, unconfirmed, revision.ErrTransportRejected, func() bool { return false }))
}
