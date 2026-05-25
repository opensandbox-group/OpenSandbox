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

package dnsproxy

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"

	"github.com/alibaba/opensandbox/internal/safego"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/log"
)

const defaultUpstreamProbeInterval = 30 * time.Second

func upstreamProbeIntervalFromEnv() time.Duration {
	s := strings.TrimSpace(os.Getenv(constants.EnvDNSUpstreamProbeIntervalSec))
	if s == "" {
		return defaultUpstreamProbeInterval
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return defaultUpstreamProbeInterval
	}
	if n > 3600 {
		n = 3600
	}
	return time.Duration(n) * time.Second
}

// upstreamProbeFromEnv returns the DNS question used for upstream liveness checks.
// Default is root IN NS (primers/recursors answer without resolving a public TLD).
// Set OPENSANDBOX_EGRESS_DNS_UPSTREAM_PROBE to an FQDN that your resolvers always
// answer (e.g. split-horizon internal name) when the default is inappropriate.
func upstreamProbeFromEnv() (name string, qtype uint16) {
	raw := strings.TrimSpace(os.Getenv(constants.EnvDNSUpstreamProbe))
	if raw == "" || raw == "." {
		return ".", dns.TypeNS
	}
	return dns.Fqdn(raw), dns.TypeA
}

func (p *Proxy) runUpstreamProbes(ctx context.Context) {
	p.probeUpstreams()

	t := time.NewTicker(p.upstreamProbeInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.probeUpstreams()
		}
	}
}

// forwardUpstreams returns the ordered list used for DNS forwarding: last healthy probe
// results when non-empty; otherwise the configured chain (e.g. before first probe).
func (p *Proxy) forwardUpstreams() []string {
	p.upstreamMu.RLock()
	active := p.activeUpstreams
	p.upstreamMu.RUnlock()
	if len(active) > 0 {
		return active
	}
	return p.upstreams
}

// probeUpstreams checks each configured resolver in parallel and refreshes activeUpstreams.
// If every probe fails, the full upstream list is kept so forwarding still attempts all resolvers.
func (p *Proxy) probeUpstreams() {
	all := p.upstreams
	if len(all) == 0 {
		return
	}

	timeout := probeExchangeTimeout(p.upstreamExchangeTimeout)
	healthy := make([]bool, len(all))
	var wg sync.WaitGroup
	for i := range all {
		wg.Add(1)
		idx := i
		addr := all[i]
		safego.Go(func() {
			defer wg.Done()
			healthy[idx] = p.probeOneUpstream(addr, timeout)
		})
	}
	wg.Wait()

	var active []string
	for i := range all {
		if healthy[i] {
			active = append(active, all[i])
		}
	}
	if len(active) == 0 {
		log.Warnf("[dns] all upstream probes failed; using full upstream list for forwarding")
		active = append([]string(nil), all...)
	}

	p.upstreamMu.Lock()
	p.activeUpstreams = active
	p.upstreamMu.Unlock()
}

func probeExchangeTimeout(upstreamTimeout time.Duration) time.Duration {
	const maxProbe = 2 * time.Second
	if upstreamTimeout <= 0 {
		return maxProbe
	}
	if upstreamTimeout > maxProbe {
		return maxProbe
	}
	return upstreamTimeout
}

func (p *Proxy) probeOneUpstream(addr string, timeout time.Duration) bool {
	m := new(dns.Msg)
	m.SetQuestion(p.upstreamProbeName, p.upstreamProbeQType)
	m.RecursionDesired = true

	c := &dns.Client{
		Timeout: timeout,
		Dialer:  p.dialerForUpstream(addr),
	}
	resp, _, err := c.Exchange(m, addr)
	if err != nil {
		log.Errorf("[dns] upstream probe %s failed: %v", addr, err)
		return false
	}
	return resp != nil
}
