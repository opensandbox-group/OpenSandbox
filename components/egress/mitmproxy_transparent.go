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

package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/iptables"
	"github.com/alibaba/opensandbox/egress/pkg/log"
	"github.com/alibaba/opensandbox/egress/pkg/mitmproxy"
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
	mu         sync.Mutex
	running    *mitmproxy.Running
	currentGen uint64 // generation of the mitmdump currently considered live
	port       int
	uid        uint32
	dports     string           // iptables --dports list (e.g. "80,443" or "80,443,8080")
	cfg        mitmproxy.Config // OnExit must NOT be set here; built per-Launch
	nextGen    uint64           // atomic; monotonic gen counter handed to each Launch
	restartCh  chan exitEvent
	shutdownCh chan struct{} // closed by watchMitmproxy on ctx cancel; lets OnExit unblock during shutdown
}

func (m *mitmTransparent) getRunning() *mitmproxy.Running {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running
}

func (m *mitmTransparent) setRunning(r *mitmproxy.Running, gen uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.running = r
	m.currentGen = gen
}

func (m *mitmTransparent) getCurrentGen() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
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

// startMitmproxyTransparentIfEnabled starts mitmdump in transparent mode, waits for the listener, and installs OUTPUT REDIRECT, then syncs the CA.
func startMitmproxyTransparentIfEnabled() (*mitmTransparent, error) {
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
	// Buffer absorbs a retry storm; correctness does not depend on the size
	// (launchTagged sends block, so events are never silently dropped).
	restartCh := make(chan exitEvent, 64)
	shutdownCh := make(chan struct{})
	const initialGen uint64 = 1
	running, err := launchTagged(cfg, restartCh, shutdownCh, initialGen)
	if err != nil {
		return nil, fmt.Errorf("start mitmdump: %w", err)
	}

	waitAddr := fmt.Sprintf("127.0.0.1:%d", mpPort)
	if err := mitmproxy.WaitListenPort(waitAddr, 15*time.Second); err != nil {
		return nil, fmt.Errorf("wait listen %s: %w", waitAddr, err)
	}
	if err := iptables.SetupTransparentHTTP(mpPort, mpUID, dports); err != nil {
		return nil, fmt.Errorf("iptables transparent: %w", err)
	}
	log.Infof("mitmproxy: transparent intercept active (OUTPUT tcp %s -> %d; trust mitm CA in clients)", dports, mpPort)

	if err := mitmproxy.SyncRootCA("", mpHome); err != nil {
		return nil, fmt.Errorf("mitm CA export: %w", err)
	}
	return &mitmTransparent{
		running:    running,
		currentGen: initialGen,
		port:       mpPort,
		uid:        mpUID,
		dports:     dports,
		cfg:        cfg,
		nextGen:    initialGen,
		restartCh:  restartCh,
		shutdownCh: shutdownCh,
	}, nil
}

// watchMitmproxy monitors mitmdump for unexpected exits, logs the error, and restarts it.
// Must be called after startMitmproxyTransparentIfEnabled.
func (m *mitmTransparent) watchMitmproxy(ctx context.Context, gate *mitmproxy.HealthGate) {
	// Closing shutdownCh on ctx cancel unblocks any OnExit closures that are
	// parked on the (now-unread) restartCh send so they don't leak past
	// shutdown.
	safego.Go(func() {
		<-ctx.Done()
		close(m.shutdownCh)
	})
	safego.Go(func() {
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
				m.restartWithBackoff(ctx, gate)

			case <-ctx.Done():
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
	const (
		initialBackoff = time.Second
		maxBackoff     = 30 * time.Second
	)
	backoff := initialBackoff
	waitAddr := fmt.Sprintf("127.0.0.1:%d", m.cfg.ListenPort)

	for attempt := 1; ; attempt++ {
		select {
		case <-ctx.Done():
			return
		default:
		}

		gen := atomic.AddUint64(&m.nextGen, 1)
		newRunning, launchErr := launchTagged(m.cfg, m.restartCh, m.shutdownCh, gen)
		if launchErr == nil {
			if waitErr := mitmproxy.WaitListenPort(waitAddr, 15*time.Second); waitErr == nil {
				m.setRunning(newRunning, gen)
				gate.SetReady(true)
				log.Infof("[mitmproxy] mitmdump restarted (pid %d, gen %d, attempt %d)", newRunning.Cmd.Process.Pid, gen, attempt)
				return
			} else {
				log.Errorf("[mitmproxy] restart attempt %d (gen %d): wait listen %s: %v", attempt, gen, waitAddr, waitErr)
				// GracefulShutdown SIGTERMs then SIGKILLs and waits for reap, so
				// the listen port is released before the next attempt's Launch
				// races to bind it. Direct Process.Kill returns immediately and
				// can cause spurious WaitListenPort failures on port contention.
				mitmproxy.GracefulShutdown(newRunning, time.Second)
			}
		} else {
			log.Errorf("[mitmproxy] restart attempt %d (gen %d): launch failed: %v", attempt, gen, launchErr)
		}

		log.Warnf("[mitmproxy] restart attempt %d failed; retrying in %s", attempt, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
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
