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

package dnsproxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/egress/pkg/log"
	"github.com/alibaba/opensandbox/egress/pkg/nftables"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	slogger "github.com/alibaba/opensandbox/internal/logger"
)

func TestServeDNSDecisionLogs(t *testing.T) {
	upstream := startUpstream(t)
	allow, err := policy.ParsePolicy(`{"defaultAction":"allow"}`)
	require.NoError(t, err)
	for _, tc := range []struct {
		name     string
		host     string
		setup    func(*Proxy)
		decision string
		stage    string
		rcode    int
	}{
		{"allowed", "example.com", nil, "allow", "answer", dns.RcodeSuccess},
		{"denied", "example.com", func(p *Proxy) { p.UpdatePolicy(policy.DefaultDenyPolicy()) }, "deny", "deny", dns.RcodeNameError},
		{"root allowed", ".", nil, "allow", "answer", dns.RcodeSuccess},
		{"root denied", ".", func(p *Proxy) { p.UpdatePolicy(policy.DefaultDenyPolicy()) }, "deny", "deny", dns.RcodeNameError},
		{"no applicable policy", "example.com", func(p *Proxy) {
			p.SetQueryPolicySelector(func(netip.Addr) (*QueryPolicy, string) { return nil, "subject has no effective policy" })
		}, "deny", "no_applicable_policy", dns.RcodeNameError},
		{"upstream failure", "example.com", func(p *Proxy) { p.upstreams = nil }, "allow", "upstream_error", dns.RcodeServerFailure},
		{"installation failure on log-skipped host", "example.com", func(p *Proxy) {
			p.SetLogSkip([]string{"example.com"})
			p.SetOnResolved(func(string, []nftables.ResolvedIP) error { return errors.New("cannot install") })
		}, "allow", "policy_error", dns.RcodeServerFailure},
		{"infrastructure bypass", "example.com", func(p *Proxy) {
			p.UpdatePolicy(policy.DefaultDenyPolicy())
			p.SetInfraDomain("example.com", nil)
		}, "", "", dns.RcodeSuccess},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &Proxy{upstreams: []string{upstream}, effectivePolicy: allow, upstreamExchangeTimeout: time.Second}
			if tc.setup != nil {
				tc.setup(p)
			}
			path := filepath.Join(t.TempDir(), "dns.log")
			logger, err := slogger.New(slogger.Config{OutputPaths: []string{path}})
			require.NoError(t, err)
			previous := log.Logger
			log.Logger = logger
			t.Cleanup(func() { log.Logger = previous })

			w := &fakeRespWriter{remote: addrFromIP("192.0.2.1")}
			p.serveDNS(w, new(dns.Msg).SetQuestion(dns.Fqdn(strings.ToUpper(tc.host)), dns.TypeA))
			require.Len(t, w.msgs, 1)
			require.Equal(t, tc.rcode, w.msgs[0].Rcode)
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			var records []map[string]any
			for line := range bytes.SplitSeq(bytes.TrimSpace(data), []byte("\n")) {
				var entry map[string]any
				require.NoError(t, json.Unmarshal(line, &entry))
				// Ignore unrelated info diagnostics such as nameserver initialization.
				if entry["level"] == "warn" || entry["opensandbox.event"] == "egress.outbound" || entry["dns.policy.decision"] != nil {
					records = append(records, entry)
				}
			}
			require.Len(t, records, 1, "there must not be a second decision record")
			record := records[0]
			require.Equal(t, tc.host, record["target.host"])
			if tc.decision == "deny" {
				require.Equal(t, "warn", record["level"])
				require.NotContains(t, record, "opensandbox.event")
			} else {
				require.Equal(t, "info", record["level"])
				require.Equal(t, "egress.outbound", record["opensandbox.event"])
			}
			if tc.rcode == dns.RcodeServerFailure {
				require.NotEmpty(t, record["error"])
				require.NotContains(t, record, "target.ips")
			} else if tc.rcode == dns.RcodeSuccess {
				require.Equal(t, []any{"1.2.3.4"}, record["target.ips"])
			}
			if tc.decision == "" {
				require.NotContains(t, record, "dns.policy.decision")
				require.NotContains(t, record, "dns.stage")
				require.NotContains(t, record, "dns.response_code")
				return
			}
			require.Equal(t, tc.decision, record["dns.policy.decision"])
			require.Equal(t, tc.stage, record["dns.stage"])
			require.Equal(t, float64(tc.rcode), record["dns.response_code"])
			if tc.stage == "answer" {
				p.SetLogSkip([]string{"example.com"})
				p.serveDNS(w, new(dns.Msg).SetQuestion("example.com.", dns.TypeA))
				afterSkip, err := os.ReadFile(path)
				require.NoError(t, err)
				require.Equal(t, data, afterSkip, "successful skipped hosts must not emit decision records")
			}
		})
	}
}
