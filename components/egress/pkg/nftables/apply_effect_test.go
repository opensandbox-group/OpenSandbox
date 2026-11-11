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
	"sync"
	"testing"

	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/stretchr/testify/require"
)

func TestApplyEffectClassification(t *testing.T) {
	cause := errors.New("failure")
	for _, tc := range []struct {
		err    error
		effect ApplyEffect
	}{
		{nil, ApplyCommitted}, {cause, ApplyUnknown},
		{errors.Join(&ApplyError{Effect: ApplyUnchanged, Err: cause}, cause), ApplyUnknown},
		{errors.Join(&ApplyError{Effect: ApplyUnchanged, Err: cause}, &ApplyError{Effect: ApplyUnchanged, Err: cause}), ApplyUnchanged},
		{&ApplyError{Effect: ApplyUnchanged, Err: cause}, ApplyUnchanged},
		{fmt.Errorf("wrapped: %w", &ApplyError{Effect: ApplyUnchanged, Err: cause}), ApplyUnchanged},
		{&ApplyError{Effect: ApplyUnknown, Err: cause}, ApplyUnknown},
		{&ApplyError{Effect: ApplyCommitted, Err: cause}, ApplyUnknown},
		{&ApplyError{Effect: 99, Err: cause}, ApplyUnknown},
	} {
		require.Equal(t, tc.effect, ApplyEffectOf(tc.err))
	}
}

func TestManagerUnchangedPreservesStateAndRetry(t *testing.T) {
	for _, kind := range []string{"runner", "cancel", "build"} {
		t.Run(kind, func(t *testing.T) {
			m, r := seededQuiescenceManager(t, true)
			oldPolicy, oldDomain := m.domainPolicy, m.domains["old.example.com"]
			oldIPs, oldActive := maps.Clone(m.tracker.dynamicIPs), maps.Clone(m.tracker.previousActiveIPs)
			calls := r.calls()
			ctx := context.Background()
			switch kind {
			case "runner":
				r.failNext(&ApplyError{Effect: ApplyUnchanged, Err: errors.New("start failed")})
			case "cancel":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "build":
				m.opts.DoHBlocklistV4 = []string{"bad-prefix"}
			}
			require.Equal(t, ApplyUnchanged, ApplyEffectOf(m.ApplyStatic(ctx, policy.DefaultDenyPolicy())))
			require.False(t, m.isQuiesced())
			require.Same(t, oldPolicy, m.domainPolicy)
			require.Same(t, oldDomain, m.domains["old.example.com"])
			require.Equal(t, oldIPs, m.tracker.dynamicIPs)
			require.Equal(t, oldActive, m.tracker.previousActiveIPs)
			if kind == "runner" {
				require.Equal(t, calls+1, r.calls())
			} else {
				require.Equal(t, calls, r.calls())
			}
			m.opts.DoHBlocklistV4 = nil
			require.NoError(t, m.AddResolvedDomain(context.Background(), "old.example.com", quiescenceIPs()))
			require.NoError(t, m.ApplyStatic(context.Background(), policy.DefaultDenyPolicy()))
		})
	}
}

func TestManagerFallbackEffectsAndDiagnostics(t *testing.T) {
	for _, first := range []ApplyEffect{ApplyUnchanged, ApplyUnknown} {
		for _, second := range []ApplyEffect{ApplyUnchanged, ApplyUnknown} {
			t.Run(fmt.Sprintf("%d-%d", first, second), func(t *testing.T) {
				m, r := seededQuiescenceManager(t, true)
				missing := errors.New("delete table inet opensandbox: No such file or directory")
				retry := errors.New("second start or wait failure")
				r.failNext(&ApplyError{Effect: first, Err: missing}, &ApplyError{Effect: second, Err: retry})
				err := m.ApplyStatic(context.Background(), policy.DefaultDenyPolicy())
				require.ErrorIs(t, err, missing)
				require.ErrorIs(t, err, retry)
				want := ApplyUnknown
				if first == ApplyUnchanged && second == ApplyUnchanged {
					want = ApplyUnchanged
				}
				require.Equal(t, want, ApplyEffectOf(err))
				require.Equal(t, want == ApplyUnknown, m.isQuiesced())
			})
		}
	}
}

func TestManagerUnchangedDoesNotUnfreeze(t *testing.T) {
	m, r := seededQuiescenceManager(t, true)
	m.Quiesce()
	calls := r.calls()
	err := m.ApplyStatic(context.Background(), policy.DefaultDenyPolicy())
	require.ErrorIs(t, err, ErrQuiesced)
	require.Equal(t, ApplyUnchanged, ApplyEffectOf(err))
	require.True(t, m.isQuiesced())
	require.Equal(t, calls, r.calls())
	require.ErrorIs(t, m.AddResolvedIPs(context.Background(), quiescenceIPs()), ErrQuiesced)
}

func TestManagerUnchangedQueuedWriter(t *testing.T) {
	m, r := seededQuiescenceManager(t, true)
	oldPolicy := m.domainPolicy
	entered, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	m.run = func(ctx context.Context, script string) ([]byte, error) {
		out, err := r.run(ctx, script)
		close(entered)
		<-release
		return out, err
	}
	r.failNext(&ApplyError{Effect: ApplyUnchanged, Err: errors.New("start failed")})
	applyDone := make(chan error, 1)
	go func() { applyDone <- m.ApplyStatic(context.Background(), policy.DefaultDenyPolicy()) }()
	<-entered
	writerStarted, writerDone := make(chan struct{}), make(chan error, 1)
	go func() {
		close(writerStarted)
		writerDone <- m.AddResolvedDomain(context.Background(), "queued.example.com", quiescenceIPs())
	}()
	<-writerStarted
	select {
	case err := <-writerDone:
		t.Fatalf("writer passed held lock: %v", err)
	default:
	}
	m.run = r.run
	unblock()
	require.Equal(t, ApplyUnchanged, ApplyEffectOf(<-applyDone))
	require.NoError(t, <-writerDone)
	require.False(t, m.isQuiesced())
	require.Same(t, oldPolicy, m.domainPolicy)
	require.Contains(t, m.domains, "queued.example.com")
}
