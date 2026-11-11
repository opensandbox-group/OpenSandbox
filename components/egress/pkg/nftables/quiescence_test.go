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

package nftables

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var errQuiescenceStatic = errors.New("ruleset submitted, result unavailable")

type quiescenceRunner struct {
	mu      sync.Mutex
	scripts []string
	errors  []error
}

func (r *quiescenceRunner) run(_ context.Context, script string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Record submission before returning the injected result. The Manager must
	// not assume an error means the new ruleset was never submitted.
	r.scripts = append(r.scripts, script)
	if len(r.errors) == 0 {
		return nil, nil
	}
	err := r.errors[0]
	r.errors = r.errors[1:]
	return nil, err
}

func (r *quiescenceRunner) failNext(errs ...error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errors = errs
}

func (r *quiescenceRunner) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.scripts)
}

func quiescencePolicy(t *testing.T) *policy.NetworkPolicy {
	t.Helper()
	p, err := policy.ParsePolicy(`{"defaultAction":"deny","egress":[{"action":"allow","target":"*.example.com"}]}`)
	require.NoError(t, err)
	return p
}

func quiescenceIPs() []ResolvedIP {
	return []ResolvedIP{
		{Addr: netip.MustParseAddr("192.0.2.1")},
		{Addr: netip.MustParseAddr("2001:db8::1")},
	}
}

func seededQuiescenceManager(t *testing.T, enabled bool) (*Manager, *quiescenceRunner) {
	t.Helper()
	r := &quiescenceRunner{}
	m := NewManagerWithRunnerAndOptions(r.run, Options{
		QuiesceOnApplyFailure: enabled,
		UpstreamProxy:         &UpstreamProxyEndpoint{Port: 3128, UID: 10042},
	})
	now := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	m.tracker.now = func() time.Time { return now }
	require.NoError(t, m.ApplyStatic(context.Background(), quiescencePolicy(t)))
	require.NoError(t, m.AddResolvedDomain(context.Background(), "old.example.com", quiescenceIPs()))
	require.NoError(t, m.tracker.refreshActiveConnections(context.Background(), []tcpConnection{
		{remote: quiescenceIPs()[0].Addr, state: "ESTABLISHED"},
	}, m))
	require.NoError(t, m.AddUpstreamProxyIPs(context.Background(), quiescenceIPs()))
	return m, r
}

func freezeWithStaticFailure(t *testing.T, m *Manager, r *quiescenceRunner) {
	t.Helper()
	before := r.calls()
	r.failNext(errQuiescenceStatic)
	require.ErrorIs(t, m.ApplyStatic(context.Background(), policy.DefaultDenyPolicy()), errQuiescenceStatic)
	require.Equal(t, before+1, r.calls(), "the injected error follows submission")
}

func TestManagerQuiescence_StaticFailure(t *testing.T) {
	m, r := seededQuiescenceManager(t, true)
	oldPolicy := m.domainPolicy
	oldDomain := m.domains["old.example.com"]
	oldIPs := maps.Clone(m.tracker.dynamicIPs)
	freezeWithStaticFailure(t, m, r)
	calls := r.calls()
	assert.ErrorIs(t, m.AddResolvedDomain(context.Background(), "old.example.com", quiescenceIPs()), ErrQuiesced)
	require.Equal(t, calls, r.calls())
	require.Same(t, oldPolicy, m.domainPolicy)
	require.Same(t, oldDomain, m.domains["old.example.com"])
	require.Equal(t, oldIPs, m.tracker.dynamicIPs)
}

func TestManagerQuiescence_AllWriters(t *testing.T) {
	writers := map[string]func(*Manager) error{
		"static":     func(m *Manager) error { return m.ApplyStatic(context.Background(), policy.DefaultDenyPolicy()) },
		"direct IPs": func(m *Manager) error { return m.AddResolvedIPs(context.Background(), quiescenceIPs()) },
		"DNS callback": func(m *Manager) error {
			return m.AddResolvedDomain(context.Background(), "old.example.com", quiescenceIPs())
		},
		"TCP refresh": func(m *Manager) error {
			return m.tracker.refreshActiveConnections(context.Background(), []tcpConnection{
				{remote: quiescenceIPs()[0].Addr, state: "ESTABLISHED"},
			}, m)
		},
		"upstream":       func(m *Manager) error { return m.AddUpstreamProxyIPs(context.Background(), quiescenceIPs()) },
		"empty IPs":      func(m *Manager) error { return m.AddResolvedIPs(context.Background(), nil) },
		"empty upstream": func(m *Manager) error { return m.AddUpstreamProxyIPs(context.Background(), nil) },
	}
	for name, write := range writers {
		t.Run(name, func(t *testing.T) {
			m, r := seededQuiescenceManager(t, true)
			freezeWithStaticFailure(t, m, r)
			calls := r.calls()
			oldIPs := maps.Clone(m.tracker.dynamicIPs)
			oldActive := maps.Clone(m.tracker.previousActiveIPs)
			assert.ErrorIs(t, write(m), ErrQuiesced)
			require.Equal(t, calls, r.calls())
			require.Equal(t, oldIPs, m.tracker.dynamicIPs)
			require.Equal(t, oldActive, m.tracker.previousActiveIPs)
		})
	}
	t.Run("domain refresh", func(t *testing.T) {
		m, r := seededQuiescenceManager(t, true)
		freezeWithStaticFailure(t, m, r)
		calls := r.calls()
		entry := m.domains["old.example.com"]
		entry.failures = 2
		entry.retryAt = m.tracker.now().Add(time.Minute)
		before := *entry
		before.addresses = maps.Clone(entry.addresses)
		ips := quiescenceIPs()[1:]
		ips[0].TTL = time.Minute
		m.applyDomainRefresh(context.Background(), "old.example.com", entry, ips)
		require.Equal(t, calls, r.calls())
		require.Equal(t, before, *entry, "a late refresh must not publish addresses or reset backoff")
	})
	t.Run("upstream unset", func(t *testing.T) {
		r := &quiescenceRunner{}
		m := NewManagerWithRunner(r.run)
		m.Quiesce()
		require.ErrorIs(t, m.AddUpstreamProxyIPs(context.Background(), quiescenceIPs()), ErrQuiesced)
		require.Zero(t, r.calls())
	})
}

func TestManagerQuiescence_QueuedWriter(t *testing.T) {
	m, r := seededQuiescenceManager(t, true)
	entered, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	m.run = func(ctx context.Context, script string) ([]byte, error) {
		out, err := r.run(ctx, script)
		close(entered)
		<-release
		return out, err
	}
	r.failNext(errQuiescenceStatic)
	staticDone := make(chan error, 1)
	go func() { staticDone <- m.ApplyStatic(context.Background(), policy.DefaultDenyPolicy()) }()
	<-entered // ApplyStatic holds Manager.mu through the failing submission.
	writers := []func() error{
		func() error { return m.AddResolvedDomain(context.Background(), "old.example.com", quiescenceIPs()) },
		func() error {
			return m.tracker.refreshActiveConnections(context.Background(), []tcpConnection{
				{remote: quiescenceIPs()[0].Addr, state: "ESTABLISHED"},
			}, m)
		},
		func() error { return m.AddUpstreamProxyIPs(context.Background(), quiescenceIPs()) },
	}
	started := make(chan struct{}, len(writers))
	done := make(chan error, len(writers))
	for _, write := range writers {
		go func() {
			started <- struct{}{}
			done <- write()
		}()
	}
	for range writers {
		<-started
	}
	select {
	case err := <-done:
		t.Fatalf("writer passed a held Manager lock: %v", err)
	default:
	}
	// Restore the recorder while the static runner owns the lock, before it
	// releases queued writers. Their only permitted result is ErrQuiesced.
	m.run = r.run
	calls := r.calls()
	unblock()
	require.ErrorIs(t, <-staticDone, errQuiescenceStatic)
	for range writers {
		assert.ErrorIs(t, <-done, ErrQuiesced)
	}
	require.Equal(t, calls, r.calls())
}

func TestManagerQuiescence_LateDomainResult(t *testing.T) {
	for _, lookupErr := range []error{nil, errors.New("DNS answer unavailable")} {
		t.Run(fmt.Sprintf("error=%v", lookupErr), func(t *testing.T) {
			m, r := seededQuiescenceManager(t, true)
			// TCP renewal seeded a longer lease. Make the domain due again.
			for addr := range m.tracker.dynamicIPs {
				m.tracker.dynamicIPs[addr] = m.tracker.now().Add(time.Minute)
			}
			entry := m.domains["old.example.com"]
			entry.failures = 2
			entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			go func() {
				m.refreshDomains(context.Background(), func(context.Context, string) ([]ResolvedIP, error) {
					close(entered)
					<-release
					return quiescenceIPs()[1:], lookupErr
				})
				close(done)
			}()
			<-entered
			freezeWithStaticFailure(t, m, r)
			calls := r.calls()
			m.mu.Lock()
			before := *entry
			before.addresses = maps.Clone(entry.addresses)
			oldIPs := maps.Clone(m.tracker.dynamicIPs)
			m.mu.Unlock()
			close(release)
			<-done
			require.Equal(t, calls, r.calls())
			require.Equal(t, before, *entry, "success and failure results must both be discarded")
			require.Equal(t, oldIPs, m.tracker.dynamicIPs)
			var lookups int
			for range 3 {
				m.refreshDomains(context.Background(), func(context.Context, string) ([]ResolvedIP, error) {
					lookups++
					return quiescenceIPs(), nil
				})
			}
			require.Zero(t, lookups, "frozen rounds must not schedule more DNS work")
		})
	}
}

func TestManagerQuiescence_QueuedDomainWork(t *testing.T) {
	m, r := seededQuiescenceManager(t, false)
	for index := range domainRefreshWorkers + 1 {
		require.NoError(t, m.AddResolvedDomain(context.Background(), fmt.Sprintf("%d.example.com", index), quiescenceIPs()))
	}
	entered := make(chan struct{}, domainRefreshWorkers+2)
	release, done := make(chan struct{}), make(chan struct{})
	var lookups atomic.Int32
	go func() {
		m.refreshDomains(context.Background(), func(context.Context, string) ([]ResolvedIP, error) {
			lookups.Add(1)
			entered <- struct{}{}
			<-release
			return quiescenceIPs(), nil
		})
		close(done)
	}()
	for range domainRefreshWorkers {
		<-entered
	}
	m.Quiesce()
	calls := r.calls()
	close(release)
	<-done
	require.EqualValues(t, domainRefreshWorkers, lookups.Load(), "selected but unstarted jobs must be abandoned")
	require.Equal(t, calls, r.calls())
}

// awaitClosedAdmission observes the admission boundary while the caller holds
// Manager.mu. The timer is only a deadlock watchdog; no assertion depends on
// goroutine scheduling order or on how long draining takes.
func awaitClosedAdmission(t *testing.T, m *Manager) {
	t.Helper()
	closed := make(chan struct{})
	go func() {
		for !m.isQuiesced() {
			runtime.Gosched()
		}
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("admission did not close while Manager.mu was held")
	}
}

func TestManagerQuiescence_ClosesAdmissionBeforeDrain(t *testing.T) {
	m, r := seededQuiescenceManager(t, false)
	calls := r.calls()
	m.mu.Lock()
	unlock := sync.OnceFunc(m.mu.Unlock)
	defer unlock()
	writerDone := make(chan error, 1)
	go func() { writerDone <- m.AddResolvedIPs(context.Background(), quiescenceIPs()) }()
	quiesceDone := make(chan struct{})
	go func() {
		m.Quiesce()
		close(quiesceDone)
	}()
	awaitClosedAdmission(t, m)
	select {
	case <-quiesceDone:
		t.Fatal("Quiesce returned before the current lock holder drained")
	default:
	}
	unlock()
	// Rejection must hold regardless of which mutex waiter is scheduled first.
	require.ErrorIs(t, <-writerDone, ErrQuiesced)
	<-quiesceDone
	require.Equal(t, calls, r.calls())
}

func TestManagerQuiescence_AdmittedDomainRefreshCompletes(t *testing.T) {
	m, r := seededQuiescenceManager(t, false)
	calls := r.calls()
	entry := m.domains["old.example.com"]
	entered, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	now := m.tracker.now().Add(time.Minute)
	markEntered := sync.OnceFunc(func() { close(entered) })
	m.tracker.now = func() time.Time {
		markEntered()
		<-release
		return now
	}
	refreshDone := make(chan struct{})
	go func() {
		m.applyDomainRefresh(context.Background(), "old.example.com", entry, quiescenceIPs()[1:])
		close(refreshDone)
	}()
	<-entered // The refresh passed admission and still owns Manager.mu.
	quiesceDone := make(chan struct{})
	go func() {
		m.Quiesce()
		close(quiesceDone)
	}()
	awaitClosedAdmission(t, m)
	unblock()
	<-refreshDone
	<-quiesceDone
	require.Len(t, entry.addresses, 1, "an admitted refresh completes its state publication")
	require.Contains(t, entry.addresses, quiescenceIPs()[1].Addr)
	require.Equal(t, calls+1, r.calls(), "an admitted refresh completes its nft update")
}

func TestManagerQuiescence_AlreadyRunningWriter(t *testing.T) {
	m, r := seededQuiescenceManager(t, false)
	entered, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	m.run = func(ctx context.Context, script string) ([]byte, error) {
		close(entered)
		<-release
		return r.run(ctx, script)
	}
	writeDone := make(chan error, 1)
	go func() { writeDone <- m.AddResolvedDomain(context.Background(), "live.example.com", quiescenceIPs()) }()
	<-entered
	quiesceStarted, quiesceDone := make(chan struct{}), make(chan struct{})
	go func() {
		close(quiesceStarted)
		m.Quiesce()
		close(quiesceDone)
	}()
	<-quiesceStarted
	select {
	case <-quiesceDone:
		t.Fatal("Quiesce returned while a writer still held Manager.mu")
	default:
	}
	unblock()
	require.NoError(t, <-writeDone, "a writer already holding the lock can finish")
	<-quiesceDone
	m.run = r.run
	require.Contains(t, m.domains, "live.example.com")
	calls := r.calls()
	require.ErrorIs(t, m.AddResolvedIPs(context.Background(), quiescenceIPs()), ErrQuiesced)
	require.Equal(t, calls, r.calls())
}

func TestManagerQuiescence_FallbackAndLegacy(t *testing.T) {
	missing := errors.New("delete table inet opensandbox: No such file or directory")
	t.Run("fallback success", func(t *testing.T) {
		m, r := seededQuiescenceManager(t, true)
		r.failNext(missing, nil)
		calls := r.calls()
		require.NoError(t, m.ApplyStatic(context.Background(), quiescencePolicy(t)))
		require.Equal(t, calls+2, r.calls())
		require.NoError(t, m.AddResolvedDomain(context.Background(), "old.example.com", quiescenceIPs()))
		require.Equal(t, calls+3, r.calls())
	})
	t.Run("fallback final failure", func(t *testing.T) {
		m, r := seededQuiescenceManager(t, true)
		r.failNext(missing, errQuiescenceStatic)
		calls := r.calls()
		require.Error(t, m.ApplyStatic(context.Background(), quiescencePolicy(t)))
		require.Equal(t, calls+2, r.calls())
		require.ErrorIs(t, m.AddResolvedIPs(context.Background(), quiescenceIPs()), ErrQuiesced)
		require.Equal(t, calls+2, r.calls())
	})
	t.Run("legacy retries", func(t *testing.T) {
		m, r := seededQuiescenceManager(t, false)
		freezeWithStaticFailure(t, m, r)
		calls := r.calls()
		require.NoError(t, m.AddResolvedDomain(context.Background(), "old.example.com", quiescenceIPs()))
		require.NoError(t, m.AddUpstreamProxyIPs(context.Background(), quiescenceIPs()))
		require.NoError(t, m.tracker.refreshActiveConnections(context.Background(), []tcpConnection{
			{remote: quiescenceIPs()[0].Addr, state: "ESTABLISHED"},
		}, m))
		require.NoError(t, m.ApplyStatic(context.Background(), quiescencePolicy(t)))
		require.Equal(t, calls+4, r.calls())
	})
	t.Run("build failure", func(t *testing.T) {
		r := &quiescenceRunner{}
		m := NewManagerWithRunnerAndOptions(r.run, Options{
			QuiesceOnApplyFailure: true, DoHBlocklistV4: []string{"invalid-prefix"},
		})
		require.Error(t, m.ApplyStatic(context.Background(), policy.DefaultDenyPolicy()))
		require.False(t, m.isQuiesced())
		require.Zero(t, r.calls())
	})
}

func TestManagerQuiescence_TeardownSticky(t *testing.T) {
	for _, removalErr := range []error{nil, errors.New("delete failed"), errors.New("No such file or directory")} {
		t.Run(fmt.Sprintf("error=%v", removalErr), func(t *testing.T) {
			m, r := seededQuiescenceManager(t, false)
			m.Quiesce()
			m.Quiesce()
			calls := r.calls()
			r.failNext(removalErr)
			err := m.RemoveEnforcement(context.Background())
			if removalErr != nil && removalErr.Error() == "delete failed" {
				require.ErrorIs(t, err, removalErr)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, calls+1, r.calls(), "shutdown remains an explicit write exception")
			require.ErrorIs(t, m.ApplyStatic(context.Background(), quiescencePolicy(t)), ErrQuiesced)
			require.ErrorIs(t, m.AddResolvedDomain(context.Background(), "old.example.com", quiescenceIPs()), ErrQuiesced)
			require.ErrorIs(t, m.AddResolvedIPs(context.Background(), quiescenceIPs()), ErrQuiesced)
			require.Equal(t, calls+1, r.calls())
			require.NoError(t, m.RemoveEnforcement(context.Background()))
			require.ErrorIs(t, m.AddUpstreamProxyIPs(context.Background(), quiescenceIPs()), ErrQuiesced)
			require.Equal(t, calls+2, r.calls())
		})
	}
}

func TestManagerQuiescence_BackgroundStops(t *testing.T) {
	m, r := seededQuiescenceManager(t, false)
	m.Quiesce()
	calls := r.calls()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var polls int
	m.tracker.listConnections = func(context.Context) ([]tcpConnection, error) {
		polls++
		cancel() // Bound the legacy RED run without relying on timing.
		return nil, errors.New("proc scan failed")
	}
	oldActive := maps.Clone(m.tracker.previousActiveIPs)
	m.tracker.run(ctx, time.Nanosecond, m)
	require.Zero(t, polls, "a frozen connection worker must stop before another poll")
	require.Equal(t, calls, r.calls())
	require.Equal(t, oldActive, m.tracker.previousActiveIPs)
}
