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

package iptables

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"

	egresslog "github.com/alibaba/opensandbox/egress/pkg/log"
	internallogger "github.com/alibaba/opensandbox/internal/logger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGatewayDNSRedirectScriptExactIPv4IPv6Set(t *testing.T) {
	script, uniqueGatewayCount, err := gatewayDNSRedirectScript([]netip.Addr{
		netip.MustParseAddr("10.0.0.10"),
		netip.MustParseAddr("10.0.0.1"),
		netip.MustParseAddr("::ffff:10.0.0.1"),
		netip.MustParseAddr("fd00::1"),
		netip.MustParseAddr("10.0.0.10"),
	}, 15353)
	require.NoError(t, err)
	require.Contains(t, script, "delete table inet "+gatewayDNSNftTable+"\nadd table inet "+gatewayDNSNftTable)
	require.Contains(t, script, "add chain inet "+gatewayDNSNftTable+" gw { type nat hook prerouting priority dstnat; }")
	assert.Equal(t, 3, uniqueGatewayCount, "five inputs normalize to three unique gateways")
	assert.Equal(t, 6, strings.Count(script, "add rule "), "three unique gateways each need UDP and TCP rules")
	assert.Contains(t, script, "ip daddr 10.0.0.1 udp dport 53 redirect to :15353")
	assert.Contains(t, script, "ip daddr 10.0.0.1 tcp dport 53 redirect to :15353")
	assert.Contains(t, script, "ip daddr 10.0.0.10 udp dport 53 redirect to :15353")
	assert.Contains(t, script, "ip daddr 10.0.0.10 tcp dport 53 redirect to :15353")
	assert.Contains(t, script, "ip6 daddr fd00::1 udp dport 53 redirect to :15353")
	assert.Contains(t, script, "ip6 daddr fd00::1 tcp dport 53 redirect to :15353")
	assert.NotContains(t, script, "ip6 daddr ::ffff:10.0.0.1", "mapped IPv4 must normalize to one IPv4 gateway")
}

func TestGatewayDNSRedirectScriptEmptySetAndValidation(t *testing.T) {
	empty, uniqueGatewayCount, err := gatewayDNSRedirectScript(nil, 15353)
	require.NoError(t, err)
	assert.Equal(t, 0, uniqueGatewayCount)
	assert.Equal(t, "delete table inet "+gatewayDNSNftTable+"\n", empty)
	assert.Error(t, func() error {
		_, _, err := gatewayDNSRedirectScript([]netip.Addr{netip.Addr{}}, 15353)
		return err
	}())
	assert.Error(t, func() error {
		_, _, err := gatewayDNSRedirectScript(nil, 0)
		return err
	}())
}

func TestReplaceGatewayDNSRedirectsRetriesMissingTableWithoutDelete(t *testing.T) {
	var scripts []string
	r := redirectRunner{runNft: func(_ context.Context, script string) ([]byte, error) {
		scripts = append(scripts, script)
		if len(scripts) == 1 {
			return []byte("Error: No such file or directory\n" + script), errors.New("exit status 1")
		}
		return nil, nil
	}}

	err := replaceGatewayDNSRedirects(context.Background(), r, []netip.Addr{netip.MustParseAddr("10.0.0.1")}, 15353)
	require.NoError(t, err)
	require.Len(t, scripts, 2)
	assert.Contains(t, scripts[0], "delete table inet "+gatewayDNSNftTable)
	assert.NotContains(t, scripts[1], "delete table inet "+gatewayDNSNftTable)
	assert.Contains(t, scripts[1], "add table inet "+gatewayDNSNftTable)
	assert.Contains(t, scripts[1], "ip daddr 10.0.0.1")
}

func TestReplaceGatewayDNSRedirectsPreservesCallerDeadlineAcrossRetry(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var seen []context.Context
	r := redirectRunner{runNft: func(got context.Context, script string) ([]byte, error) {
		seen = append(seen, got)
		if len(seen) == 1 {
			return []byte("Error: No such file or directory\n" + script), errors.New("exit status 1")
		}
		return nil, nil
	}}

	err := replaceGatewayDNSRedirects(ctx, r, []netip.Addr{netip.MustParseAddr("10.0.0.1")}, 15353)
	require.NoError(t, err)
	require.Len(t, seen, 2)
	wantDeadline, wantOK := ctx.Deadline()
	for _, got := range seen {
		gotDeadline, gotOK := got.Deadline()
		assert.True(t, gotOK)
		assert.Equal(t, wantOK, gotOK)
		assert.Equal(t, wantDeadline, gotDeadline)
		assert.Equal(t, ctx.Done(), got.Done(), "the original operation context is reused")
	}
}

func TestReplaceGatewayDNSRedirectsDoesNotRetryAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	r := redirectRunner{runNft: func(_ context.Context, script string) ([]byte, error) {
		calls++
		cancel()
		return []byte("Error: No such file or directory\n" + script), errors.New("exit status 1")
	}}

	err := replaceGatewayDNSRedirects(ctx, r, []netip.Addr{netip.MustParseAddr("10.0.0.1")}, 15353)
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, calls, "cancellation must not trigger the missing-table retry")
}

type captureGatewayDNSLogger struct {
	internallogger.Logger
	messages []string
}

func (l *captureGatewayDNSLogger) Infof(template string, args ...any) {
	l.messages = append(l.messages, fmt.Sprintf(template, args...))
}

func TestReplaceGatewayDNSRedirectsLogsUniqueGatewayCount(t *testing.T) {
	previous := egresslog.Logger
	capture := &captureGatewayDNSLogger{}
	egresslog.Logger = capture
	t.Cleanup(func() { egresslog.Logger = previous })

	r := redirectRunner{runNft: func(context.Context, string) ([]byte, error) { return nil, nil }}
	inputs := []netip.Addr{
		netip.MustParseAddr("10.0.0.10"),
		netip.MustParseAddr("10.0.0.1"),
		netip.MustParseAddr("::ffff:10.0.0.1"),
		netip.MustParseAddr("fd00::1"),
		netip.MustParseAddr("10.0.0.10"),
	}
	require.NoError(t, replaceGatewayDNSRedirects(context.Background(), r, inputs, 15353))
	require.Equal(t, []string{"gateway DNS redirects replaced (3 unique gateway(s) -> :15353)"}, capture.messages)
}

func TestReplaceGatewayDNSRedirectsTreatsMissingEmptyTableAsSuccess(t *testing.T) {
	calls := 0
	r := redirectRunner{runNft: func(_ context.Context, script string) ([]byte, error) {
		calls++
		assert.Equal(t, "delete table inet "+gatewayDNSNftTable+"\n", script)
		return []byte("Error: No such file or directory\n" + script), errors.New("exit status 1")
	}}

	require.NoError(t, replaceGatewayDNSRedirects(context.Background(), r, nil, 15353))
	assert.Equal(t, 1, calls, "no retry is needed when the desired set is empty")
}

func TestReplaceGatewayDNSRedirectsPropagatesFailures(t *testing.T) {
	t.Run("non-missing error", func(t *testing.T) {
		calls := 0
		r := redirectRunner{runNft: func(_ context.Context, _ string) ([]byte, error) {
			calls++
			return []byte("syntax error"), errors.New("exit status 1")
		}}
		assert.Error(t, replaceGatewayDNSRedirects(context.Background(), r, []netip.Addr{netip.MustParseAddr("10.0.0.1")}, 15353))
		assert.Equal(t, 1, calls)
	})
	t.Run("failed missing-table retry", func(t *testing.T) {
		calls := 0
		r := redirectRunner{runNft: func(_ context.Context, script string) ([]byte, error) {
			calls++
			if calls == 1 {
				return []byte("Error: No such file or directory\n" + script), errors.New("exit status 1")
			}
			return []byte("nft busy"), errors.New("exit status 1")
		}}
		err := replaceGatewayDNSRedirects(context.Background(), r, []netip.Addr{netip.MustParseAddr("10.0.0.1")}, 15353)
		assert.ErrorContains(t, err, "retry")
		assert.Equal(t, 2, calls)
	})
}
