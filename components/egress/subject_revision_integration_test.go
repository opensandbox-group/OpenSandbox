// Copyright 2026 The OpenSandbox Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
// http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/credentialvault"
	"github.com/alibaba/opensandbox/egress/pkg/revision"
	"github.com/stretchr/testify/require"
)

// These tests are mandatory: missing Python, imports or socket capability fail
// rather than skip. They do not exercise TLS, nft, source-IP routing or launch.
type subjectIPCReport struct {
	Ready      bool               `json:"ready"`
	Error      string             `json:"error"`
	Revision   *revision.Identity `json:"revision"`
	Digest     string             `json:"digest"`
	Action     string             `json:"action"`
	Reason     string             `json:"reason"`
	Dropped    int                `json:"dropped"`
	AbortCalls int                `json:"abortCalls"`
	LastAbort  *revision.Identity `json:"lastAbort"`
}

type subjectIPCChild struct {
	input         io.WriteCloser
	output        *json.Decoder
	socket, token string
}

func (c *subjectIPCChild) exchange(t *testing.T, value any) subjectIPCReport {
	t.Helper()
	require.NoError(t, json.NewEncoder(c.input).Encode(value))
	type result struct {
		report subjectIPCReport
		err    error
	}
	done := make(chan result, 1)
	go func() { var report subjectIPCReport; err := c.output.Decode(&report); done <- result{report, err} }()
	select {
	case got := <-done:
		require.NoError(t, got.err)
		require.Empty(t, got.report.Error, "real subject IPC child could not execute")
		return got.report
	case <-time.After(5 * time.Second):
		t.Fatal("real subject IPC child response timed out")
		return subjectIPCReport{}
	}
}

func launchSubjectIPCChild(t *testing.T) *subjectIPCChild {
	t.Helper()
	python, err := exec.LookPath("python3")
	require.NoError(t, err, "real subject IPC requires python3; no skip allowed")
	dir, err := os.MkdirTemp("", "os-subject-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(dir)) })
	token, err := revision.NewSessionToken()
	require.NoError(t, err)
	cmd := exec.Command(python, "tests/fixtures/subject_revision_child.py")
	cmd.Stderr = io.Discard // Do not forward potentially payload-bearing tracebacks.
	input, err := cmd.StdinPipe()
	require.NoError(t, err)
	output, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = input.Close()
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
		_ = output.Close()
	})
	c := &subjectIPCChild{input: input, output: json.NewDecoder(output), socket: filepath.Join(dir, "ipc.sock"), token: token}
	require.True(t, c.exchange(t, map[string]any{"control": "subject-test-control", "socket": c.socket, "token": token}).Ready)
	return c
}

func (c *subjectIPCChild) inspect(t *testing.T, target revision.SubjectTarget) subjectIPCReport {
	t.Helper()
	return c.exchange(t, map[string]any{"command": "inspect", "subjectKey": target.SubjectKey, "subjectGeneration": target.SubjectGeneration, "sni": "api.example.com"})
}

func subjectPayload(t *testing.T, bound bool, version int64) []byte {
	t.Helper()
	snapshot := credentialvault.ActiveSnapshot{Revision: version}
	if bound {
		snapshot.Bindings = []credentialvault.ActiveBinding{{Name: "api", Match: credentialvault.Match{Schemes: []string{"https"}, Hosts: []string{"api.example.com"}, Methods: []string{"GET"}, Paths: []string{"/api/*"}}, Headers: []credentialvault.InjectionHeader{{Name: "X-Private", Value: "fixture-value"}}}}
		snapshot.Redactions = []string{"fixture-value"}
	}
	payload, err := credentialvault.MarshalDecisionSnapshot(snapshot, 0)
	require.NoError(t, err)
	return payload
}

func subjectTransport(t *testing.T, c *subjectIPCChild, target revision.SubjectTarget) *revision.SubjectUnixTransport {
	t.Helper()
	transport, err := revision.NewSubjectUnixTransport(c.socket, c.token, "subject-test-control", target, 65536)
	require.NoError(t, err)
	return transport
}

func subjectIdentity(target revision.SubjectTarget, epoch, vault int64, payload []byte) revision.Identity {
	digest := sha256.Sum256(payload)
	return revision.Identity{ControlGeneration: "subject-test-control", SubjectGeneration: target.SubjectGeneration, DecisionEpoch: epoch, VaultRevision: vault, PolicyEpoch: 0, Digest: hex.EncodeToString(digest[:])}
}

func (c *subjectIPCChild) post(t *testing.T, path string, body []byte, token string, duplicateAuth bool) int {
	t.Helper()
	status, _ := c.response(t, path, body, token, duplicateAuth)
	return status
}

func (c *subjectIPCChild) response(t *testing.T, path string, body []byte, token string, duplicateAuth bool) (int, []byte) {
	t.Helper()
	transport := &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", c.socket)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second}
	request, err := http.NewRequest(http.MethodPost, "http://unix"+path, bytes.NewReader(body))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	if duplicateAuth {
		request.Header.Add("Authorization", "Bearer "+token)
	}
	response, err := client.Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	require.NoError(t, err)
	require.LessOrEqual(t, len(data), 4096)
	require.NotContains(t, string(data), "fixture-value")
	return response.StatusCode, data
}

func TestSubjectRevisionRealIPCIsolationAndReplay(t *testing.T) {
	child := launchSubjectIPCChild(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	lifecycle, err := revision.NewSubjectLifecycleClient(child.socket, child.token, "subject-test-control")
	require.NoError(t, err)
	a := revision.SubjectTarget{SubjectKey: "s-a", SubjectGeneration: "generation-a"}
	b := revision.SubjectTarget{SubjectKey: "s-b", SubjectGeneration: "generation-b"}
	_, err = lifecycle.Register(ctx, a)
	require.NoError(t, err)
	_, err = lifecycle.Register(ctx, b)
	require.NoError(t, err)
	ta, tb := subjectTransport(t, child, a), subjectTransport(t, child, b)
	active, err := ta.Readback(ctx)
	require.NoError(t, err)
	require.Nil(t, active)
	require.Equal(t, "snapshot_missing", child.inspect(t, a).Reason)
	unknown := revision.SubjectTarget{SubjectKey: "s-unknown", SubjectGeneration: "unknown"}
	_, err = subjectTransport(t, child, unknown).Readback(ctx)
	require.Error(t, err)
	require.Equal(t, "unknown_identity", child.inspect(t, unknown).Reason)
	for _, command := range []map[string]any{
		{"command": "inspect", "subjectKey": "s-unknown", "subjectGeneration": "unknown", "ech": true},
		{"command": "inspect", "subjectKey": "s-unknown", "subjectGeneration": "unknown"},
	} {
		require.Equal(t, "deny", child.exchange(t, command).Action)
	}
	ca, err := revision.New("subject-test-control", a.SubjectGeneration, ta, 65536)
	require.NoError(t, err)
	cb, err := revision.New("subject-test-control", b.SubjectGeneration, tb, 65536)
	require.NoError(t, err)
	pa, pb := subjectPayload(t, true, 1), subjectPayload(t, false, 0)
	ra, err := ca.Apply(ctx, 1, 0, pa)
	require.NoError(t, err)
	rb, err := cb.Apply(ctx, 0, 0, pb)
	require.NoError(t, err)
	report := child.inspect(t, a)
	require.Equal(t, &ra, report.Revision)
	require.Equal(t, ra.Digest, report.Digest)
	require.Equal(t, "needs_registry", report.Action)
	require.Equal(t, "passthrough", child.inspect(t, b).Action)
	stagedPayload := subjectPayload(t, false, 2)
	staged := subjectIdentity(a, 2, 2, stagedPayload)
	_, err = ta.Prepare(ctx, staged, stagedPayload)
	require.NoError(t, err)
	require.Equal(t, &ra, child.inspect(t, a).Revision)
	require.Equal(t, &rb, child.inspect(t, b).Revision)
	_, err = ta.Commit(ctx, staged)
	require.NoError(t, err)
	require.Equal(t, &staged, child.inspect(t, a).Revision)
	require.Equal(t, &rb, child.inspect(t, b).Revision)
	_, err = tb.Commit(ctx, staged)
	require.Error(t, err)
	// Capacity rejection consumes a lifecycle sequence, but cannot evict A or B.
	_, err = lifecycle.Register(ctx, unknown)
	require.Error(t, err)
	require.Equal(t, &rb, child.inspect(t, b).Revision)
	_, err = lifecycle.Unload(ctx, a)
	require.NoError(t, err)
	// Replay while the key is absent: a replacement generation must not mask
	// a missing lifecycle high-water check by rejecting this as a conflict.
	raw, err := json.Marshal(map[string]any{"controlGeneration": "subject-test-control", "lifecycleSequence": 1, "target": a})
	require.NoError(t, err)
	status, rejection := child.response(t, "/v2/subjects/register", raw, child.token, false)
	require.Equal(t, 409, status)
	require.JSONEq(t, `{"error":"stale_lifecycle_sequence"}`, string(rejection))
	_, err = ta.Readback(ctx)
	require.Error(t, err)
	replacement := revision.SubjectTarget{SubjectKey: a.SubjectKey, SubjectGeneration: "generation-a-new"}
	_, err = lifecycle.Register(ctx, replacement)
	require.NoError(t, err)
	for _, operation := range []string{"commit", "abort"} {
		if operation == "commit" {
			_, err = ta.Commit(ctx, staged)
		} else {
			_, err = ta.Abort(ctx, staged)
		}
		require.Error(t, err)
	}
	_, err = ta.Readback(ctx)
	require.Error(t, err)
	raw, _ = json.Marshal(map[string]any{"controlGeneration": "subject-test-control", "lifecycleSequence": 1, "target": a})
	require.Equal(t, 409, child.post(t, "/v2/subjects/register", raw, child.token, false))
	raw, _ = json.Marshal(map[string]any{"controlGeneration": "subject-test-control", "lifecycleSequence": 4, "target": a})
	require.Equal(t, 409, child.post(t, "/v2/subjects/unload", raw, child.token, false))
	active, err = subjectTransport(t, child, replacement).Readback(ctx)
	require.NoError(t, err)
	require.Nil(t, active)
	require.Equal(t, &rb, child.inspect(t, b).Revision)
}

func TestSubjectRevisionRealIPCLostRepliesAndRejection(t *testing.T) {
	child := launchSubjectIPCChild(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	lifecycle, err := revision.NewSubjectLifecycleClient(child.socket, child.token, "subject-test-control")
	require.NoError(t, err)
	a := revision.SubjectTarget{SubjectKey: "s-a", SubjectGeneration: "generation-a"}
	b := revision.SubjectTarget{SubjectKey: "s-b", SubjectGeneration: "generation-b"}
	require.True(t, child.exchange(t, map[string]any{"command": "fault", "operation": "register"}).Ready)
	_, err = lifecycle.Register(ctx, a)
	require.NoError(t, err)
	_, err = lifecycle.Register(ctx, b)
	require.NoError(t, err)
	ta, tb := subjectTransport(t, child, a), subjectTransport(t, child, b)
	ca, err := revision.New("subject-test-control", a.SubjectGeneration, ta, 65536)
	require.NoError(t, err)
	cb, err := revision.New("subject-test-control", b.SubjectGeneration, tb, 65536)
	require.NoError(t, err)
	pa := subjectPayload(t, true, 1)
	require.True(t, child.exchange(t, map[string]any{"command": "fault", "operation": "prepare"}).Ready)
	_, err = ca.Apply(ctx, 1, 0, pa)
	require.ErrorIs(t, err, revision.ErrPrepareRejected)
	active, err := ta.Readback(ctx)
	require.NoError(t, err)
	require.Nil(t, active)
	rb, err := cb.Apply(ctx, 0, 0, subjectPayload(t, false, 0))
	require.NoError(t, err)
	// Both replies are lost after the real receiver staged then aborted. The
	// Coordinator must retain the exact abort, while B remains independently usable.
	require.True(t, child.exchange(t, map[string]any{"command": "fault", "operations": []string{"prepare", "abort"}}).Ready)
	attemptCtx, cancelAttempt := context.WithTimeout(context.Background(), 3*time.Second)
	_, err = ca.Apply(attemptCtx, 1, 0, pa)
	cancelAttempt()
	require.ErrorIs(t, err, revision.ErrIndeterminate)
	pendingAbort := child.inspect(t, a)
	require.Nil(t, pendingAbort.Revision)
	require.NotNil(t, pendingAbort.LastAbort)
	require.Equal(t, 2, pendingAbort.AbortCalls)
	_, err = ca.Apply(ctx, 1, 0, pa)
	require.ErrorIs(t, err, revision.ErrIndeterminate)
	rb, err = cb.Apply(ctx, 1, 0, subjectPayload(t, false, 1))
	require.NoError(t, err)
	reconcileCtx, cancelReconcile := context.WithTimeout(context.Background(), 3*time.Second)
	recovered, err := ca.Reconcile(reconcileCtx)
	cancelReconcile()
	require.NoError(t, err)
	require.Nil(t, recovered)
	afterAbort := child.inspect(t, a)
	require.Equal(t, pendingAbort.LastAbort, afterAbort.LastAbort)
	require.Equal(t, 3, afterAbort.AbortCalls)
	require.Nil(t, afterAbort.Revision)
	require.Equal(t, &rb, child.inspect(t, b).Revision)
	require.True(t, child.exchange(t, map[string]any{"command": "fault", "operation": "commit"}).Ready)
	candidate, err := ca.Apply(ctx, 1, 0, pa)
	// The child committed but deliberately sent no HTTP reply. Apply must
	// retain uncertainty until explicit readback reconciliation confirms it.
	require.ErrorIs(t, err, revision.ErrIndeterminate)
	confirmed, err := ca.Reconcile(ctx)
	require.NoError(t, err)
	require.Equal(t, &candidate, confirmed)
	active, err = ta.Readback(ctx)
	require.NoError(t, err)
	require.NotNil(t, active)
	require.Equal(t, "needs_registry", child.inspect(t, a).Action)
	require.Equal(t, 5, child.inspect(t, a).Dropped)
	require.Equal(t, &rb, child.inspect(t, b).Revision)
	request, _ := json.Marshal(map[string]any{"controlGeneration": "subject-test-control", "target": a})
	require.Equal(t, 401, child.post(t, "/v2/revisions/active", request, strings.Repeat("x", 43), false))
	require.Equal(t, 401, child.post(t, "/v2/revisions/active", request, child.token, true))
	// Keep the target valid so only duplicate/extra-field strictness can reject.
	targetJSON, err := json.Marshal(a)
	require.NoError(t, err)
	duplicateTarget := []byte(`{"controlGeneration":"subject-test-control","target":` + string(targetJSON) + `,"target":` + string(targetJSON) + `}`)
	require.Equal(t, 400, child.post(t, "/v2/revisions/active", duplicateTarget, child.token, false))
	extraField, err := json.Marshal(map[string]any{"controlGeneration": "subject-test-control", "target": a, "extra": true})
	require.NoError(t, err)
	require.Equal(t, 400, child.post(t, "/v2/revisions/active", extraField, child.token, false))
	wrongControl, _ := json.Marshal(map[string]any{"controlGeneration": "other", "target": a})
	require.Equal(t, 409, child.post(t, "/v2/revisions/active", wrongControl, child.token, false))
	require.Equal(t, 413, child.post(t, "/v2/subjects/register", bytes.Repeat([]byte(" "), 8193), child.token, false))
	badTarget := revision.SubjectTarget{SubjectKey: a.SubjectKey, SubjectGeneration: b.SubjectGeneration}
	raw, _ := json.Marshal(map[string]any{"target": badTarget, "revision": rb})
	require.Equal(t, 409, child.post(t, "/v2/revisions/commit", raw, child.token, false))
	// Exact bytes and validator are independently necessary, even with a valid
	// outer target and fresh revision epoch.
	// Valid schema and digest, but payload vault revision 3 disagrees with
	// envelope revision 2. Only snapshot validation should reject this case.
	schemaMismatchPayload := subjectPayload(t, false, 3)
	bad := subjectIdentity(a, active.DecisionEpoch+1, 2, schemaMismatchPayload)
	_, err = ta.Prepare(ctx, bad, schemaMismatchPayload)
	require.Error(t, err)
	mismatch := subjectIdentity(a, active.DecisionEpoch+1, 2, subjectPayload(t, false, 2))
	mismatch.Digest = strings.Repeat("0", 64)
	_, err = ta.Prepare(ctx, mismatch, subjectPayload(t, false, 2))
	require.Error(t, err)
	require.Equal(t, active, child.inspect(t, a).Revision)
	require.Equal(t, &rb, child.inspect(t, b).Revision)
}

func TestSubjectLifecycleRealIPCConsumptionAndExactReplay(t *testing.T) {
	child := launchSubjectIPCChild(t)
	raw := []byte(`{"controlGeneration":"subject-test-control","lifecycleSequence":1,"target":null}`)
	status, receipt := child.response(t, "/v2/subjects/register", raw, child.token, false)
	require.Equal(t, 400, status)
	var consumed revision.LifecycleReceipt
	require.NoError(t, json.Unmarshal(receipt, &consumed))
	require.Equal(t, int64(1), consumed.LifecycleSequence)
	require.Equal(t, "invalid_target", consumed.Result.Outcome)
	status, retry := child.response(t, "/v2/subjects/register", raw, child.token, false)
	require.Equal(t, 400, status)
	require.JSONEq(t, string(receipt), string(retry))
	statusBody := []byte(`{"controlGeneration":"subject-test-control"}`)
	checkSequence := func(want int64) {
		t.Helper()
		status, body := child.response(t, "/v2/subjects/lifecycle-status", statusBody, child.token, false)
		require.Equal(t, 200, status)
		var state struct {
			LastSequence  int64  `json:"lastSequence"`
			CommandDigest string `json:"commandDigest"`
		}
		require.NoError(t, json.Unmarshal(body, &state))
		require.Equal(t, want, state.LastSequence)
		if want == 1 {
			require.Equal(t, consumed.CommandDigest, state.CommandDigest)
		}
	}
	checkSequence(1)
	// Same semantic command with different raw bytes is a conflict, not a retry.
	require.Equal(t, 409, child.post(t, "/v2/subjects/register", append([]byte(" "), raw...), child.token, false))
	require.Equal(t, 400, child.post(t, "/v2/subjects/register", []byte(`{"controlGeneration":"subject-test-control","lifecycleSequence":2,"target":null,"extra":1}`), child.token, false))
	require.Equal(t, 409, child.post(t, "/v2/subjects/register", []byte(`{"controlGeneration":"other","lifecycleSequence":2,"target":null}`), child.token, false))
	require.Equal(t, 401, child.post(t, "/v2/subjects/register", raw, strings.Repeat("z", 43), false))
	checkSequence(1)
	target := revision.SubjectTarget{SubjectKey: "s-a", SubjectGeneration: "generation-a"}
	next, _ := json.Marshal(map[string]any{"controlGeneration": "subject-test-control", "lifecycleSequence": 2, "target": target})
	require.Equal(t, 200, child.post(t, "/v2/subjects/register", next, child.token, false))
	checkSequence(2)
	// An exact retry must retain a pending snapshot instead of reconstructing its owner.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	transport := subjectTransport(t, child, target)
	payload := subjectPayload(t, true, 1)
	identity := subjectIdentity(target, 1, 1, payload)
	_, err := transport.Prepare(ctx, identity, payload)
	require.NoError(t, err)
	require.Equal(t, 200, child.post(t, "/v2/subjects/register", next, child.token, false))
	_, err = transport.Commit(ctx, identity)
	require.NoError(t, err)
	require.Equal(t, &identity, child.inspect(t, target).Revision)
}
