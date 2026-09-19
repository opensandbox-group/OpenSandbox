// Copyright 2026 Alibaba Group Holding Ltd.
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

package revisionruntime

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/credentialvault"
	"github.com/alibaba/opensandbox/egress/pkg/revision"
	"github.com/stretchr/testify/require"
)

func TestProcessSessionBootstrapInstallsAuthoritativeEmptySnapshot(t *testing.T) {
	session, server := newBootstrapSession(t)
	identity, err := session.Bootstrap(context.Background(), credentialvault.ActiveSnapshot{}, 0)
	require.NoError(t, err)
	require.Equal(t, int64(1), identity.DecisionEpoch)
	require.Zero(t, identity.VaultRevision)
	require.Zero(t, identity.PolicyEpoch)
	require.JSONEq(t, `{
		"version": 1,
		"vaultRevision": 0,
		"effectivePolicyEpoch": 0,
		"interceptionMode": "credential-bound",
		"state": "active-empty",
		"tlsBindingHostSelectors": [],
		"fullRenderedBindings": [],
		"redactions": []
	}`, string(server.payload()))
	require.Equal(t, &identity, server.activeIdentity())

	coordinator, err := session.Coordinator()
	require.NoError(t, err)
	confirmed, err := coordinator.Confirmed()
	require.NoError(t, err)
	require.Equal(t, &identity, confirmed)

	_, err = session.Bootstrap(context.Background(), credentialvault.ActiveSnapshot{}, 0)
	require.ErrorIs(t, err, revision.ErrIndeterminate)
}

func TestProcessSessionBootstrapInstallsRenderedRecoverySnapshot(t *testing.T) {
	session, server := newBootstrapSession(t)
	snapshot := credentialvault.ActiveSnapshot{
		Revision: 7,
		Bindings: []credentialvault.ActiveBinding{{
			Name: "api",
			Match: credentialvault.Match{
				Schemes: []string{"https"},
				Hosts:   []string{"api.example.com"},
				Methods: []string{"GET"},
				Paths:   []string{"/v1/*"},
			},
			Headers: []credentialvault.InjectionHeader{{Name: "Private-Token", Value: "never-log-me"}},
		}},
		Redactions: []string{"never-log-me"},
	}
	identity, err := session.Bootstrap(context.Background(), snapshot, 11)
	require.NoError(t, err)
	require.Equal(t, int64(7), identity.VaultRevision)
	require.Equal(t, int64(11), identity.PolicyEpoch)

	var payload map[string]any
	require.NoError(t, json.Unmarshal(server.payload(), &payload))
	require.Equal(t, "active", payload["state"])
	require.Equal(t, float64(7), payload["vaultRevision"])
	require.Equal(t, float64(11), payload["effectivePolicyEpoch"])
	require.Equal(t, []any{"api.example.com"}, payload["tlsBindingHostSelectors"])
	require.Equal(t, []string{"https"}, snapshot.Bindings[0].Match.Schemes)
}

func TestProcessSessionBootstrapRejectsInvalidSnapshotBeforeDial(t *testing.T) {
	session, err := NewProcessSession(processSessionConfig(processSessionParent(t)))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, session.Close()) })
	invalid := credentialvault.ActiveSnapshot{
		Revision: 1,
		Bindings: []credentialvault.ActiveBinding{{
			Name: "api",
			Match: credentialvault.Match{
				Schemes: []string{"https"},
				Hosts:   []string{"api.example.com"},
				Methods: []string{"GET"},
				Paths:   []string{"/*"},
			},
			Headers: []credentialvault.InjectionHeader{{Name: "Authorization", Value: "never-log-me"}},
		}},
	}
	_, err = session.Bootstrap(context.Background(), invalid, 1)
	require.ErrorIs(t, err, credentialvault.ErrInvalidDecisionSnapshot)
	server := serveBootstrapReceiver(t, session)
	_, err = session.Bootstrap(context.Background(), credentialvault.ActiveSnapshot{}, 0)
	require.NoError(t, err)
	require.NotEmpty(t, server.payload())
}

func TestProcessSessionBootstrapHonorsReadinessContext(t *testing.T) {
	session, err := NewProcessSession(processSessionConfig(processSessionParent(t)))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, session.Close()) })
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = session.Bootstrap(ctx, credentialvault.ActiveSnapshot{}, 0)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestProcessSessionBootstrapFencesConcurrentStaleReadiness(t *testing.T) {
	session, server := newBootstrapSession(t)
	server.staleSecondReadback = true
	server.secondReadbackSeen = make(chan struct{})
	server.releaseSecondReadback = make(chan struct{})

	results := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := session.Bootstrap(context.Background(), credentialvault.ActiveSnapshot{}, 0)
			results <- err
		}()
	}
	first := <-results
	close(server.releaseSecondReadback)
	second := <-results
	if first == nil {
		require.ErrorIs(t, second, revision.ErrBusy)
	} else {
		require.ErrorIs(t, first, revision.ErrBusy)
		require.NoError(t, second)
	}
	prepares, commits := server.commandCounts()
	require.Equal(t, 1, prepares)
	require.Equal(t, 1, commits)
}

func TestProcessSessionBootstrapPreservesIndeterminateCommit(t *testing.T) {
	session, server := newBootstrapSession(t)
	server.rejectCommitResponse = true
	_, err := session.Bootstrap(context.Background(), credentialvault.ActiveSnapshot{}, 0)
	require.ErrorIs(t, err, revision.ErrIndeterminate)
	_, err = session.Bootstrap(context.Background(), credentialvault.ActiveSnapshot{}, 0)
	require.ErrorIs(t, err, revision.ErrIndeterminate)
	prepares, commits := server.commandCounts()
	require.Equal(t, 1, prepares)
	require.Equal(t, 1, commits)
}

func TestProcessSessionReconcileBootstrapConfirmsLostCommitAck(t *testing.T) {
	session, server := newBootstrapSession(t)
	server.rejectCommitResponse = true
	_, err := session.Bootstrap(context.Background(), credentialvault.ActiveSnapshot{}, 0)
	require.ErrorIs(t, err, revision.ErrIndeterminate)
	server.rejectCommitResponse = false

	resolved, err := session.ReconcileBootstrap(context.Background())
	require.NoError(t, err)
	require.NotNil(t, resolved)
	require.Equal(t, server.activeIdentity(), resolved)
	again, err := session.ReconcileBootstrap(context.Background())
	require.NoError(t, err)
	require.Equal(t, resolved, again)
	_, err = session.Bootstrap(context.Background(), credentialvault.ActiveSnapshot{}, 0)
	require.ErrorIs(t, err, revision.ErrIndeterminate)
	prepares, commits := server.commandCounts()
	require.Equal(t, 1, prepares)
	require.Equal(t, 1, commits)
}

func TestProcessSessionReconcileBootstrapPreservesConfirmedOutcomeAfterLocalFenceFailure(t *testing.T) {
	parent := processSessionParent(t)
	session, err := NewProcessSession(processSessionConfig(parent))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, session.Close()) })
	server := serveBootstrapReceiver(t, session)
	server.rejectCommitResponse = true
	_, err = session.Bootstrap(context.Background(), credentialvault.ActiveSnapshot{}, 0)
	require.ErrorIs(t, err, revision.ErrIndeterminate)

	require.NoError(t, os.Chmod(parent, 0o777))
	t.Cleanup(func() { require.NoError(t, os.Chmod(parent, 0o700)) })
	_, err = session.ReconcileBootstrap(context.Background())
	require.ErrorIs(t, err, revision.ErrTransportUnavailable)
	require.NoError(t, os.Chmod(parent, 0o700))

	readbacks := server.readbackCount()
	_, err = session.Bootstrap(context.Background(), credentialvault.ActiveSnapshot{}, 0)
	require.ErrorIs(t, err, revision.ErrIndeterminate)
	require.Equal(t, readbacks, server.readbackCount())
}

func TestProcessSessionReconcileBootstrapClearsLostAbortAckForRetry(t *testing.T) {
	session, server := newBootstrapSession(t)
	server.rejectPrepareResponse = true
	server.rejectAbortResponse = true
	_, err := session.Bootstrap(context.Background(), credentialvault.ActiveSnapshot{}, 0)
	require.ErrorIs(t, err, revision.ErrIndeterminate)
	server.rejectAbortResponse = false

	resolved, err := session.ReconcileBootstrap(context.Background())
	require.NoError(t, err)
	require.Nil(t, resolved)
	server.rejectPrepareResponse = false
	identity, err := session.Bootstrap(context.Background(), credentialvault.ActiveSnapshot{}, 0)
	require.NoError(t, err)
	require.Equal(t, int64(2), identity.DecisionEpoch)
	prepares, commits := server.commandCounts()
	require.Equal(t, 2, prepares)
	require.Equal(t, 1, commits)
	require.Equal(t, 2, server.abortCount())
}

type bootstrapReceiver struct {
	mu                    sync.Mutex
	token                 string
	pending               *revision.Identity
	active                *revision.Identity
	data                  []byte
	prepares              int
	commits               int
	readbacks             int
	staleSecondReadback   bool
	secondReadbackSeen    chan struct{}
	releaseSecondReadback chan struct{}
	rejectPrepareResponse bool
	rejectCommitResponse  bool
	rejectAbortResponse   bool
	aborted               *revision.Identity
	aborts                int
}

func newBootstrapSession(t *testing.T) (*ProcessSession, *bootstrapReceiver) {
	t.Helper()
	session, err := NewProcessSession(processSessionConfig(processSessionParent(t)))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, session.Close()) })
	receiver := serveBootstrapReceiver(t, session)
	return session, receiver
}

func serveBootstrapReceiver(t *testing.T, session *ProcessSession) *bootstrapReceiver {
	t.Helper()
	config, err := session.MitmproxyConfig()
	require.NoError(t, err)
	receiver := &bootstrapReceiver{token: config.SessionToken}
	listener, err := net.Listen("unix", config.SocketPath)
	require.NoError(t, err)
	server := &http.Server{Handler: receiver}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	return receiver
}

func (r *bootstrapReceiver) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	if request.Header.Get("Authorization") != "Bearer "+r.token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch {
	case request.Method == http.MethodGet && request.URL.Path == "/v1/revisions/active":
		r.mu.Lock()
		r.readbacks++
		readback := r.active
		stale := r.staleSecondReadback && r.readbacks == 2
		if stale {
			close(r.secondReadbackSeen)
		}
		secondSeen := r.secondReadbackSeen
		release := r.releaseSecondReadback
		r.mu.Unlock()
		if r.staleSecondReadback && !stale {
			select {
			case <-secondSeen:
			case <-time.After(100 * time.Millisecond):
			}
		}
		if stale {
			<-release
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"revision": readback})
	case request.Method == http.MethodPost && request.URL.Path == "/v1/revisions/prepare":
		var value struct {
			Revision revision.Identity `json:"revision"`
			Payload  []byte            `json:"payload"`
		}
		if json.NewDecoder(request.Body).Decode(&value) != nil {
			http.Error(w, "malformed", http.StatusBadRequest)
			return
		}
		r.mu.Lock()
		r.pending = &value.Revision
		r.data = append([]byte(nil), value.Payload...)
		r.prepares++
		r.mu.Unlock()
		if r.rejectPrepareResponse {
			http.Error(w, "unavailable", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"revision": value.Revision})
	case request.Method == http.MethodPost && request.URL.Path == "/v1/revisions/commit":
		var value struct {
			Revision revision.Identity `json:"revision"`
		}
		if json.NewDecoder(request.Body).Decode(&value) != nil {
			http.Error(w, "malformed", http.StatusBadRequest)
			return
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.pending == nil || *r.pending != value.Revision {
			http.Error(w, "conflict", http.StatusConflict)
			return
		}
		r.active = r.pending
		r.pending = nil
		r.commits++
		if r.rejectCommitResponse {
			http.Error(w, "unavailable", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"revision": value.Revision})
	case request.Method == http.MethodPost && request.URL.Path == "/v1/revisions/abort":
		var value struct {
			Revision revision.Identity `json:"revision"`
		}
		if json.NewDecoder(request.Body).Decode(&value) != nil {
			http.Error(w, "malformed", http.StatusBadRequest)
			return
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.pending != nil && *r.pending == value.Revision {
			r.pending = nil
			copy := value.Revision
			r.aborted = &copy
		} else if r.aborted == nil || *r.aborted != value.Revision {
			http.Error(w, "conflict", http.StatusConflict)
			return
		}
		r.aborts++
		if r.rejectAbortResponse {
			http.Error(w, "unavailable", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"revision": value.Revision})
	default:
		http.Error(w, "missing", http.StatusNotFound)
	}
}

func (r *bootstrapReceiver) abortCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.aborts
}

func (r *bootstrapReceiver) readbackCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.readbacks
}

func (r *bootstrapReceiver) commandCounts() (int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.prepares, r.commits
}

func (r *bootstrapReceiver) payload() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]byte(nil), r.data...)
}

func (r *bootstrapReceiver) activeIdentity() *revision.Identity {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active == nil {
		return nil
	}
	copy := *r.active
	return &copy
}
