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
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/iptables"
	"github.com/alibaba/opensandbox/egress/pkg/log"
	"github.com/alibaba/opensandbox/egress/pkg/mitmproxy"
	"github.com/alibaba/opensandbox/egress/pkg/revision"
	"github.com/alibaba/opensandbox/internal/safego"
)

// exitEvent carries an OnExit notification tagged with the mitmdump generation
// that produced it, so the watcher can tell the live process dying apart from
// a killed half-launched attempt being reaped.
type exitEvent struct {
	gen uint64
	err error
}

type mitmTransparent struct {
	mu              sync.RWMutex
	running         *mitmproxy.Running
	revisionSession revisionProcessSession
	currentGen      uint64 // generation of the mitmdump currently considered live
	stopping        bool
	port            int
	uid             uint32
	dports          string           // iptables --dports list (e.g. "80,443" or "80,443,8080")
	cfg             mitmproxy.Config // OnExit must NOT be set here; built per-Launch
	revisionOwner   *revisionLaunchOwner
	nextGen         uint64 // owned by mu; monotonic gen counter handed to each Launch
	launchGen       uint64 // unpublished attempt; the launch caller owns its resources
	launchExited    bool   // OnExit and publication serialize under mu
	pending         *revisionLaunchResult
	shutdownOnce    sync.Once
	restartCh       chan exitEvent
	shutdownCh      chan struct{} // closed on ctx cancel or shutdown; unblocks OnExit and backoff
	watchDone       chan struct{}
}

func (m *mitmTransparent) publishRunning(
	r *mitmproxy.Running,
	session revisionProcessSession,
	gen uint64,
) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopping || (m.revisionOwner != nil && m.revisionOwner.server != nil) {
		return false
	}
	m.running = r
	m.revisionSession = session
	m.currentGen = gen
	m.launchGen, m.pending = 0, nil
	return true
}

func (m *mitmTransparent) claimForShutdown() (*mitmproxy.Running, revisionProcessSession) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopping = true
	if m.revisionOwner != nil && m.revisionOwner.server != nil {
		m.revisionOwner.server.mitmGate.SetReady(false)
	}
	running, session := m.running, m.revisionSession
	m.running = nil
	m.revisionSession = nil
	return running, session
}

func (m *mitmTransparent) signalShutdown() {
	m.shutdownOnce.Do(func() {
		if m.shutdownCh != nil {
			close(m.shutdownCh)
		}
	})
}

func (m *mitmTransparent) stopChild(running *mitmproxy.Running, timeout time.Duration) {
	if running == nil {
		return
	}
	if m.revisionOwner != nil && m.revisionOwner.stop != nil {
		m.revisionOwner.stop(running)
	} else {
		mitmproxy.GracefulShutdown(running, timeout)
	}
}

func (m *mitmTransparent) shutdown(timeout time.Duration) {
	running, session := m.claimForShutdown()
	m.signalShutdown()
	m.stopChild(running, timeout)
	if err := m.revisionOwner.closeSession(session); err != nil {
		log.Errorf("[mitmproxy] revision session cleanup failed: %v", err)
	}
	if m.watchDone != nil {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case <-m.watchDone:
		case <-timer.C:
			log.Errorf("[mitmproxy] restart watcher did not stop within %s", timeout)
		}
	}
}

// OnExit runs after cmd.Wait has reaped the child. Detach that generation before
// closing its session so shutdown can never claim the same child a second time.
func (m *mitmTransparent) closeRevisionSession(gen uint64) {
	var server *policyServer
	if m.revisionOwner != nil {
		server = m.revisionOwner.server
	}
	if server != nil {
		server.mu.Lock()
		defer server.mu.Unlock()
	}
	m.mu.Lock()
	if m.currentGen != gen {
		m.mu.Unlock()
		return
	}
	session := m.revisionSession
	m.running, m.revisionSession = nil, nil
	if server != nil {
		server.mitmGate.SetReady(false)
	}
	m.mu.Unlock()
	// Keep the policy barrier until cleanup is known, excluding fresh bootstrap
	// and publication. Never reacquire it while holding the lifecycle lock.
	if session != nil {
		if err := session.Close(); err != nil {
			if server != nil {
				server.requireRevisionRecoveryLocked(revisionRecoverySessionCleanupFailed)
			}
			log.Errorf("[mitmproxy] revision session cleanup failed")
		}
	}
}

func (m *mitmTransparent) getCurrentGen() uint64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.currentGen
}

// launchTagged starts mitmdump with an OnExit closure that publishes the death
// of this specific process (identified by gen) into restartCh. The send blocks
// (shutdownCh is the only escape): losing an exit event would leave the watcher
// blind to a dead mitmdump, while stale events from killed attempts are cheap
// to discard via the gen check in watchMitmproxy.
func launchTagged(cfg mitmproxy.Config, restartCh chan<- exitEvent, shutdownCh <-chan struct{}, gen uint64) (*mitmproxy.Running, error) {
	cfg.OnExit = func(err error) {
		select {
		case restartCh <- exitEvent{gen: gen, err: err}:
		case <-shutdownCh:
			log.Warnf("[mitmproxy] dropping exit event during shutdown (gen=%d): %v", gen, err)
		}
	}
	return mitmproxy.Launch(cfg)
}

// mitmLaunchDependencies are the process and listener boundaries used by both
// startup and restart. They do not replace readiness or lifecycle decisions.
type mitmLaunchDependencies struct {
	launch func(mitmproxy.Config) (*mitmproxy.Running, error)
	listen func(context.Context, string, time.Duration) error
	retry  func(context.Context, <-chan struct{}, time.Duration) bool
}

func defaultMitmLaunchDependencies() mitmLaunchDependencies {
	return mitmLaunchDependencies{launch: mitmproxy.Launch, listen: mitmproxy.WaitListenPortContext, retry: waitMitmRetry}
}

func waitMitmRetry(ctx context.Context, shutdown <-chan struct{}, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-shutdown:
		return false
	case <-timer.C:
		return true
	}
}

func (m *mitmTransparent) recordExit(gen uint64) {
	m.mu.Lock()
	var failedOwner *policyServer
	if m.launchGen == gen {
		m.launchExited = true
	}
	if m.currentGen == gen && m.revisionOwner != nil && m.revisionOwner.server != nil {
		m.revisionOwner.server.mitmGate.SetReady(false)
		if !m.stopping && m.running != nil && m.revisionOwner.server.quarantine != nil {
			failedOwner = m.revisionOwner.server
		}
	}
	m.mu.Unlock()
	// Preserve s.mu -> m.mu lock order. The sticky recovery latch invalidates
	// bootstrap tickets before any retry can publish readiness.
	if failedOwner != nil {
		failedOwner.mu.Lock()
		m.mu.Lock()
		stillOwned := !m.stopping && m.running != nil && m.currentGen == gen && m.revisionOwner != nil && m.revisionOwner.server == failedOwner
		m.mu.Unlock()
		if stillOwned {
			failedOwner.requireRevisionRecoveryLocked(revisionRecoveryUnknown)
		}
		failedOwner.mu.Unlock()
	}
}

func (m *mitmTransparent) launchTaggedWithRevision(
	ctx context.Context, launchProcess func(mitmproxy.Config) (*mitmproxy.Running, error),
) (*revisionLaunchResult, error) {
	m.mu.Lock()
	if m.stopping {
		m.mu.Unlock()
		return nil, revision.ErrClosed
	}
	if m.launchGen != 0 {
		m.mu.Unlock()
		return nil, revision.ErrBusy
	}
	m.nextGen++
	gen := m.nextGen
	m.launchGen, m.launchExited = gen, false
	m.mu.Unlock()
	cfg := m.cfg
	cfg.OnExit = func(err error) {
		// This record is serialized with the final ready write. The watcher may be
		// busy launching, so queuing an event alone cannot guard publication.
		m.recordExit(gen)
		select {
		case m.restartCh <- exitEvent{gen: gen, err: err}:
		case <-m.shutdownCh:
		}
	}
	var result *revisionLaunchResult
	var err error
	if m.revisionOwner == nil {
		var running *mitmproxy.Running
		running, err = launchProcess(cfg)
		if err == nil {
			result = &revisionLaunchResult{running: running}
		}
	} else {
		result, err = m.revisionOwner.launch(ctx, cfg, launchProcess)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		m.launchGen = 0
		return nil, err
	}
	result.generation = gen
	m.pending = result
	return result, nil
}

// cleanupLaunch disposes only the caller's unpublished attempt. Shutdown owns
// published resources exclusively; this result was never attached to m.running.
func (m *mitmTransparent) cleanupLaunch(result *revisionLaunchResult) {
	m.stopChild(result.running, time.Second)
	if err := m.revisionOwner.closeSession(result.session); err != nil {
		log.Errorf("[mitmproxy] revision session cleanup failed: %v", err)
	}
	m.mu.Lock()
	if m.pending == result {
		m.pending, m.launchGen = nil, 0
	}
	m.mu.Unlock()
}

func (m *mitmTransparent) launchAndListen(ctx context.Context, deps mitmLaunchDependencies) (*revisionLaunchResult, error) {
	launchCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	result, err := m.launchTaggedWithRevision(launchCtx, deps.launch)
	if err != nil {
		return nil, err
	}
	waitAddr := fmt.Sprintf("127.0.0.1:%d", m.cfg.ListenPort)
	if err := deps.listen(launchCtx, waitAddr, 15*time.Second); err != nil {
		m.cleanupLaunch(result)
		return nil, fmt.Errorf("wait listen %s: %w", waitAddr, err)
	}
	return result, nil
}

// startInitial includes redirect and CA preparation before the same readiness
// checkpoint used by restart. Publication uses the live caller context, not the
// now-cancelled launch/listen timeout context.
func (m *mitmTransparent) startInitial(ctx context.Context, deps mitmLaunchDependencies, prepare func() error) error {
	// The always-rule reload starts before MITM and can invalidate a clean
	// capture. Bound retries below restartCh's capacity: the watcher starts only
	// after initial startup, so rejected children leave queued exit events.
	const maxAttempts = 3
	prepared := false
	for attempt := 1; ; attempt++ {
		result, err := m.launchAndListen(ctx, deps)
		if err != nil {
			return err
		}
		if !prepared {
			if err := prepare(); err != nil {
				m.cleanupLaunch(result)
				return err
			}
			// Redirect installation appends rules; never repeat its side effects.
			prepared = true
		}
		if m.revisionOwner != nil && m.revisionOwner.server != nil {
			err = publishRevisionReady(ctx, m, result)
		} else if !m.publishRunning(result.running, result.session, result.generation) {
			err = revision.ErrClosed
		}
		if err == nil {
			return nil
		}
		m.cleanupLaunch(result)
		if !errors.Is(err, errStaleRevisionBootstrap) || attempt == maxAttempts {
			return err
		}
		// The next launch recaptures only after cleanup and rechecks shutdown,
		// cancellation, and sticky recovery before creating another session.
	}
}

// startMitmproxyTransparentIfEnabled starts mitmdump in transparent mode, waits for the listener, and installs OUTPUT REDIRECT, then syncs the CA.
func startMitmproxyTransparentIfEnabled(
	ctx context.Context,
	policyServer *policyServer,
) (*mitmTransparent, error) {
	if !constants.IsTruthy(os.Getenv(constants.EnvMitmproxyTransparent)) {
		return nil, nil
	}

	mpPort := constants.EnvIntOrDefault(constants.EnvMitmproxyPort, constants.DefaultMitmproxyPort)
	mpUID, _, mpHome, err := mitmproxy.LookupUser(mitmproxy.RunAsUser)
	if err != nil {
		return nil, fmt.Errorf("lookup user %q: %w (ensure this user exists in the image)", mitmproxy.RunAsUser, err)
	}

	dports, err := constants.BuildMitmproxyPortList(os.Getenv(constants.EnvMitmproxyExtraPorts))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", constants.EnvMitmproxyExtraPorts, err)
	}

	cfg := mitmproxy.Config{
		ListenPort:  mpPort,
		UserName:    mitmproxy.RunAsUser,
		ScriptPaths: parseScriptPaths(os.Getenv(constants.EnvMitmproxyScript)),
	}
	revisionOwner, err := newSidecarRevisionLaunchOwner(policyServer)
	if err != nil {
		return nil, fmt.Errorf("configure revision runtime: %w", err)
	}
	m := &mitmTransparent{
		port: mpPort, uid: mpUID, dports: dports, cfg: cfg, revisionOwner: revisionOwner,
		restartCh: make(chan exitEvent, 64), shutdownCh: make(chan struct{}), watchDone: make(chan struct{}),
	}
	err = m.startInitial(ctx, defaultMitmLaunchDependencies(), func() error {
		if err := iptables.SetupTransparentHTTP(mpPort, mpUID, dports); err != nil {
			return fmt.Errorf("iptables transparent: %w", err)
		}
		log.Infof("mitmproxy: transparent intercept active (OUTPUT tcp %s -> %d; trust mitm CA in clients)", dports, mpPort)
		if err := mitmproxy.SyncRootCA("", mpHome); err != nil {
			return fmt.Errorf("mitm CA export: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return m, nil
}

// watchMitmproxy monitors mitmdump for unexpected exits, logs the error, and restarts it.
// Must be called after startMitmproxyTransparentIfEnabled.
func (m *mitmTransparent) watchMitmproxy(ctx context.Context, gate *mitmproxy.HealthGate) {
	// Closing shutdownCh on ctx cancel unblocks any OnExit closures that are
	// parked on the (now-unread) restartCh send so they don't leak past
	// shutdown.
	safego.Go(func() {
		select {
		case <-ctx.Done():
			m.signalShutdown()
		case <-m.shutdownCh:
		}
	})
	safego.Go(func() {
		defer close(m.watchDone)
		for {
			select {
			case ev := <-m.restartCh:
				select {
				case <-ctx.Done():
					return
				default:
				}
				cur := m.getCurrentGen()
				if ev.gen != cur {
					// Stale event: a previous half-launched attempt that we
					// killed is just now being reaped. The currently-live
					// mitmdump is unaffected; ignore and keep watching.
					log.Infof("[mitmproxy] ignoring stale exit event (gen=%d, current=%d): %v", ev.gen, cur, ev.err)
					continue
				}

				log.Errorf("[mitmproxy] mitmdump exited (gen=%d): %v; restarting...", ev.gen, ev.err)
				gate.SetReady(false)
				m.closeRevisionSession(ev.gen)
				m.restartWithBackoff(ctx, gate)

			case <-ctx.Done():
				return
			case <-m.shutdownCh:
				return
			}
		}
	})
}

// restartWithBackoff retries mitmdump launch indefinitely with exponential
// backoff (1s..30s) until it succeeds or ctx is cancelled, so transient OOM /
// resource pressure cannot leave egress permanently dead.
//
// Each attempt gets a fresh generation. Exit events for older (killed)
// generations are filtered by watchMitmproxy, so restartCh must not be drained
// here — doing so could swallow a real death of the freshly-restarted mitmdump.
func (m *mitmTransparent) restartWithBackoff(ctx context.Context, gate *mitmproxy.HealthGate) {
	m.restartWithBackoffUsing(ctx, gate, defaultMitmLaunchDependencies())
}

func (m *mitmTransparent) restartWithBackoffUsing(ctx context.Context, gate *mitmproxy.HealthGate, deps mitmLaunchDependencies) {
	const maxBackoff = 30 * time.Second
	backoff := time.Second
	for attempt := 1; ; attempt++ {
		if ctx.Err() != nil {
			return
		}
		result, err := m.launchAndListen(ctx, deps)
		if err == nil {
			if m.revisionOwner != nil && m.revisionOwner.server != nil {
				err = publishRevisionReady(ctx, m, result)
			} else if !m.publishRunning(result.running, result.session, result.generation) {
				err = revision.ErrClosed
			} else {
				gate.SetReady(true)
			}
			if err == nil {
				log.Infof("[mitmproxy] mitmdump restarted (gen %d, attempt %d)", result.generation, attempt)
				return
			}
			m.cleanupLaunch(result)
		}
		if errors.Is(err, errRevisionRecoveryRequired) || errors.Is(err, revision.ErrClosed) || ctx.Err() != nil {
			return
		}
		// Cleanup itself may have introduced uncertainty. Stop, rather than making
		// another launch (or spinning on a permanently rejected capture).
		if m.revisionOwner != nil && m.revisionOwner.server != nil {
			s := m.revisionOwner.server
			s.mu.Lock()
			recoveryErr := s.revisionRecovery.recoveryErrorLocked()
			s.mu.Unlock()
			if recoveryErr != nil {
				return
			}
		}
		log.Warnf("[mitmproxy] restart attempt %d failed: %v; retrying in %s", attempt, err, backoff)
		if !deps.retry(ctx, m.shutdownCh, backoff) {
			return
		}
		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

func parseScriptPaths(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if s := strings.TrimSpace(p); s != "" {
			out = append(out, s)
		}
	}
	return out
}
