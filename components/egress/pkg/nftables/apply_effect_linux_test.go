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

//go:build linux

package nftables

import (
	"context"
	"maps"
	"os"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/stretchr/testify/require"
)

func TestNftStartFailurePreservesKernelAndRetry(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if !isolatedNftTest(t, ctx) {
		return
	}
	m := NewManagerWithOptions(Options{QuiesceOnApplyFailure: true, UpstreamProxy: &UpstreamProxyEndpoint{Port: 3128, UID: 10042}})
	old := quiescencePolicy(t)
	require.NoError(t, m.ApplyStatic(ctx, old))
	require.NoError(t, m.AddResolvedDomain(ctx, "old.example.com", quiescenceIPs()))
	require.NoError(t, m.AddUpstreamProxyIPs(ctx, quiescenceIPs()))
	before := readQuiescenceKernelSets(t, ctx)
	oldPolicy, oldDomain := m.domainPolicy, m.domains["old.example.com"]
	oldIPs := maps.Clone(m.tracker.dynamicIPs)
	path := os.Getenv("PATH")
	t.Setenv("PATH", t.TempDir()) // Real production exec.LookPath/Start failure.
	err := m.ApplyStatic(ctx, policy.DefaultDenyPolicy())
	require.Error(t, err)
	require.Equal(t, ApplyUnchanged, ApplyEffectOf(err))
	require.False(t, m.isQuiesced())
	t.Setenv("PATH", path)
	require.Equal(t, before, readQuiescenceKernelSets(t, ctx))
	require.Same(t, oldPolicy, m.domainPolicy)
	require.Same(t, oldDomain, m.domains["old.example.com"])
	require.Equal(t, oldIPs, m.tracker.dynamicIPs)
	require.NoError(t, m.ApplyStatic(ctx, policy.DefaultDenyPolicy()))
	after := readQuiescenceKernelSets(t, ctx)
	require.Empty(t, after[allowV4Set])
	require.Empty(t, after[dynAllowV4Set])
	require.Empty(t, after[upstreamProxyV4Set])
	require.NoError(t, m.AddResolvedIPs(ctx, quiescenceIPs()))
	require.NotEmpty(t, readQuiescenceKernelSets(t, ctx)[dynAllowV4Set])
}

func TestNftRealMissingTableFallback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if !isolatedNftTest(t, ctx) {
		return
	}
	var scripts []string
	var effects []ApplyEffect
	m := NewManagerWithRunnerAndOptions(func(ctx context.Context, script string) ([]byte, error) {
		scripts = append(scripts, script)
		out, err := defaultRunner(ctx, script)
		effects = append(effects, ApplyEffectOf(err))
		return out, err
	}, Options{QuiesceOnApplyFailure: true, UpstreamProxy: &UpstreamProxyEndpoint{Port: 3128, UID: 10042}})
	require.NoError(t, m.ApplyStatic(ctx, quiescencePolicy(t)))
	require.Len(t, scripts, 2)
	require.Equal(t, []ApplyEffect{ApplyUnknown, ApplyCommitted}, effects)
	require.Contains(t, scripts[0], "delete table inet opensandbox")
	require.NotContains(t, scripts[1], "delete table inet opensandbox")
	require.False(t, m.isQuiesced())
	require.NoError(t, m.AddResolvedDomain(ctx, "old.example.com", quiescenceIPs()))
	require.NotEmpty(t, readQuiescenceKernelSets(t, ctx)[dynAllowV4Set])
}
