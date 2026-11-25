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

package fastsandboxnft

import (
	"context"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/egress/pkg/droptelemetry"
	"github.com/alibaba/opensandbox/egress/pkg/nftables"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/alibaba/opensandbox/egress/pkg/subject"
)

func dropFixture(t *testing.T) []string {
	p, err := policy.ParsePolicy(`{"defaultAction":"deny","egress":[{"action":"deny","target":"192.0.2.8","ports":[22]},{"action":"deny","target":"2001:db8::/64"}]}`)
	require.NoError(t, err)
	r := &fakeRunner{}
	a := NewApplier(r.Run, Options{BlockDoH443: true, DoHBlocklistV4: []string{"192.0.2.1"}, DoHBlocklistV6: []string{"2001:db8:1::1"}, MitmRedirectPort: 18081, PortFloorDenyRanges: []policy.PortRange{{Lo: 1, Hi: 21}}, UpstreamProxy: &UpstreamProxyEndpoint{Port: 3128, LiteralIPs: []netip.Addr{netip.MustParseAddr("192.0.2.9")}}})
	ctx := context.Background()
	s := subject.FromSandboxUID("u-1")
	require.NoError(t, a.ApplyReset(ctx))
	require.NoError(t, a.ApplyDenyFirst(ctx, s, testSlot("u-1", "10.0.0.5")))
	require.NoError(t, a.ApplyPolicy(ctx, s, p))
	require.NoError(t, a.ApplyDenyFirst(ctx, s, testSlot("u-1", "10.0.0.6")))
	return r.scripts
}
func TestDropTelemetryOffSnapshot(t *testing.T) {
	got := strings.Join(dropFixture(t), "# next transaction\n")
	path := filepath.Join("testdata", "drop-off.nft")
	want, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, string(want), got)
}

func TestDropTelemetryEveryDrop(t *testing.T) {
	data, err := os.ReadFile("testdata/drop-reasons.tsv")
	require.NoError(t, err)
	expected := map[string]droptelemetry.Reason{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		kv := strings.SplitN(line, "\t", 2)
		expected[kv[1]] = droptelemetry.Reason(kv[0])
	}
	seen := map[string]bool{}
	for _, script := range dropFixture(t) {
		cfg := droptelemetry.DefaultConfig()
		cfg.Mode = droptelemetry.Sample
		got, err := droptelemetry.Instrument(script, cfg, dropReasons(script))
		require.NoError(t, err)
		for _, line := range strings.Split(script, "\n") {
			if !strings.HasPrefix(line, "add rule ") || !strings.HasSuffix(line, " drop") {
				continue
			}
			reason, ok := expected[line]
			require.True(t, ok, "uncovered drop %s", line)
			require.Equal(t, reason, dropReasons(script)(line))
			seen[line] = true
			match := strings.TrimSuffix(line, " drop")
			// Every original drop still exists with only a named counter inserted.
			var verdict string
			for _, out := range strings.Split(got, "\n") {
				if strings.HasPrefix(out, match+" counter name ") {
					verdict = out
				}
			}
			require.NotEmpty(t, verdict)
			require.True(t, strings.HasSuffix(verdict, " drop"))
			require.Contains(t, verdict, string(reason))
			tcp := match + " meta l4proto tcp tcp flags & (syn | ack) == syn limit rate 20/second burst 40 packets log group 3085 snaplen 128 prefix \"osb:" + string(reason) + "\""
			udp := match + " meta l4proto udp limit rate 20/second burst 40 packets log group 3085 snaplen 128 prefix \"osb:" + string(reason) + "\""
			if reason != droptelemetry.EstablishedRevoke && !strings.Contains(match, "udp ") {
				require.Contains(t, got, tcp+"\n")
				require.Less(t, strings.Index(got, tcp), strings.Index(got, verdict))
			} else {
				require.NotContains(t, got, tcp)
			}
			if reason != droptelemetry.EstablishedRevoke && !strings.Contains(match, "tcp ") {
				require.Contains(t, got, udp+"\n")
				require.Less(t, strings.Index(got, udp), strings.Index(got, verdict))
			} else {
				require.NotContains(t, got, udp)
			}
		}
	}
	require.Len(t, seen, len(expected))
}

func TestDropTelemetryAlternateDropForms(t *testing.T) {
	cases := map[string]droptelemetry.Reason{
		"add rule inet opensandbox-fast-sandbox dispatch tcp dport 443 drop":                                                                          "doh",
		"add rule inet opensandbox-fast-sandbox input ct status dnat meta l4proto tcp ct original proto-dst 443 drop":                                 "doh",
		"add rule inet opensandbox-fast-sandbox input ip6 saddr 2001:db8::2 ip6 daddr 2001:db8::1 tcp dport 18081 drop":                               "gateway_mitm_direct",
		"add rule inet opensandbox-fast-sandbox subj_s_other_in ct original ip6 daddr 2001:db8::1 meta l4proto tcp ct original proto-dst { 22 } drop": "subject_input_deny",
	}
	for line, reason := range cases {
		require.Equal(t, reason, dropReason(line))
		cfg := droptelemetry.DefaultConfig()
		cfg.Mode = droptelemetry.Sample
		out, err := droptelemetry.Instrument(line+"\n", cfg, dropReason)
		require.NoError(t, err)
		require.Contains(t, out, "counter name")
		if reason == droptelemetry.EstablishedRevoke {
			require.NotContains(t, out, "log group")
		} else {
			require.Contains(t, out, "prefix \"osb:"+string(reason)+"\"")
		}
	}
}

func TestDropTelemetryUIDEndingInInputSuffix(t *testing.T) {
	script := "add rule inet opensandbox-fast-sandbox subj_s_uid_in ip daddr @subj_s_uid_in_deny_v4 drop\nadd rule inet opensandbox-fast-sandbox subj_s_uid_in drop\nadd rule inet opensandbox-fast-sandbox subj_s_uid_in_in ct original ip daddr @subj_s_uid_in_deny_v4 drop\nadd rule inet opensandbox-fast-sandbox subj_s_uid_in_in drop\n"
	reasonFor := dropReasons(script)
	for _, line := range strings.Split(strings.TrimSpace(script), "\n") {
		if strings.Contains(line, " subj_s_uid_in_in ") {
			require.Equal(t, droptelemetry.SubjectInputDeny, reasonFor(line))
		} else if strings.Contains(line, "daddr") {
			require.Equal(t, droptelemetry.DenySet, reasonFor(line))
		} else {
			require.Equal(t, droptelemetry.DefaultDeny, reasonFor(line))
		}
	}
}

func counterReadMarker(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	marker := filepath.Join(dir, "counter-read")
	script := "#!/bin/sh\nif [ -n \"$OPENSANDBOX_TEST_COUNTER_MARKER\" ]; then : > \"$OPENSANDBOX_TEST_COUNTER_MARKER\"; fi\nprintf '%s\\n' '{\"nftables\":[]}'\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "nft"), []byte(script), 0700))
	t.Setenv("PATH", dir)
	t.Setenv("OPENSANDBOX_TEST_COUNTER_MARKER", "")
	return marker
}

func TestDropTelemetryReadsBeforeFastSandboxLeasePlanning(t *testing.T) {
	for _, mode := range []droptelemetry.Mode{droptelemetry.Counter, droptelemetry.Sample} {
		t.Run(string(mode), func(t *testing.T) {
			marker := counterReadMarker(t)
			base := time.Unix(1000, 0)
			at := base
			clock := func() time.Time {
				if _, err := os.Stat(marker); err == nil {
					return base.Add(121 * time.Second)
				}
				return at
			}
			cfg := droptelemetry.DefaultConfig()
			cfg.Mode = mode
			runner := &fakeRunner{}
			a := NewApplier(runner.Run, Options{DropTelemetry: droptelemetry.New(cfg, TableName)})
			a.now = clock
			pol, err := policy.ParsePolicy(`{"defaultAction":"deny","egress":[{"action":"allow","target":"*.example.com"}]}`)
			require.NoError(t, err)
			ctx := context.Background()
			survivor := subject.FromSandboxUID("lease-owner")
			removed := subject.FromSandboxUID("removed")
			require.NoError(t, a.ApplyDenyFirst(ctx, survivor, testSlot("lease-owner", "10.0.0.5")))
			require.NoError(t, a.ApplyDenyFirst(ctx, removed, testSlot("removed", "10.0.0.6")))
			require.NoError(t, a.ApplyPolicy(ctx, survivor, pol))
			short := netip.MustParseAddr("192.0.2.10")
			long := netip.MustParseAddr("192.0.2.11")
			require.NoError(t, a.AddResolvedIPs(ctx, survivor, "allowed.example.com", []nftables.ResolvedIP{{Addr: short, TTL: time.Minute}, {Addr: long, TTL: 2 * time.Minute}}))
			at = base.Add(119 * time.Second)
			t.Setenv("OPENSANDBOX_TEST_COUNTER_MARKER", marker)
			require.NoError(t, a.Remove(ctx, removed))
			require.FileExists(t, marker, "the counter read must execute")
			require.NotContains(t, runner.last(), short.String(), "a lease expiring during the read must not be restored")
			require.Contains(t, runner.last(), long.String()+" timeout 59s", "the surviving lease uses the post-read clock")
		})
	}
}
