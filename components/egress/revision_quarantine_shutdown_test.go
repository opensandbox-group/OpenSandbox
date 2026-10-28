// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/dnsproxy"
	"github.com/alibaba/opensandbox/egress/pkg/events"
	"github.com/stretchr/testify/require"
)

type quarantineCleanupNft struct {
	stubNft
	removed bool
}

func (n *quarantineCleanupNft) RemoveEnforcement(context.Context) error { n.removed = true; return nil }

type quarantineBlockedSubscriber struct {
	entered, release chan struct{}
	after            func()
}

func (s *quarantineBlockedSubscriber) HandleBlocked(context.Context, events.BlockedEvent) {
	close(s.entered)
	<-s.release
	s.after()
}

func TestRuntimeQuarantineShutdownNeverDeletesEnforcement(t *testing.T) {
	for _, stage := range []string{"normal", "prior-recovery", "during-drain", "session-close"} {
		t.Run(stage, func(t *testing.T) {
			s := recoveryPolicyFixture(t)
			f := runtimeQuarantineFixture(s)
			dir := t.TempDir()
			commands := filepath.Join(dir, "commands")
			// Observe all redirect cleanup backends without ever changing host rules.
			for _, name := range []string{"nft", "iptables", "ip6tables"} {
				require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\necho called >> '"+commands+"'\n"), 0700))
			}
			t.Setenv("PATH", dir)
			n := &quarantineCleanupNft{}
			var broadcaster *events.Broadcaster
			var blocked *quarantineBlockedSubscriber
			var m *mitmTransparent
			fail := func() {
				s.mu.Lock()
				defer s.mu.Unlock()
				f.fail = "ensure"
				s.requireRevisionRecoveryLocked(revisionRecoveryExternalEffectsUnknown)
			}
			switch stage {
			case "prior-recovery":
				fail()
			case "during-drain":
				broadcaster = events.NewBroadcaster(context.Background(), events.BroadcasterConfig{})
				blocked = &quarantineBlockedSubscriber{entered: make(chan struct{}), release: make(chan struct{}), after: fail}
				broadcaster.AddSubscriber(blocked)
				broadcaster.Publish(events.BlockedEvent{})
				<-blocked.entered
			case "session-close":
				f.fail = "ensure"
				session := &mutationTestSession{close: func() error { return errors.New("cleanup failed") }}
				m = &mitmTransparent{port: 8080, dports: "80,443", revisionSession: session, revisionOwner: &revisionLaunchOwner{server: s}}
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			done := make(chan struct{})
			go func() { defer close(done); waitForShutdown(ctx, &dnsproxy.Proxy{}, nil, nil, n, m, broadcaster, s) }()
			if blocked != nil {
				select {
				case <-done:
					t.Fatal("shutdown skipped the blocked drain")
				case <-time.After(20 * time.Millisecond):
				}
				close(blocked.release)
			}
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("shutdown blocked")
			}
			require.False(t, n.removed)
			_, err := os.Stat(commands)
			require.True(t, os.IsNotExist(err), "redirect cleanup ran")
			if stage == "normal" {
				require.Empty(t, f.calls)
				require.False(t, s.revisionRecoveryRequired.Load())
			} else {
				require.True(t, s.revisionRecoveryRequired.Load())
				require.Equal(t, quarantineUnknown, packetQuarantineState(s.quarantine.state.Load()))
			}
		})
	}
}
