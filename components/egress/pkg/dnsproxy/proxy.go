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
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/events"
	"github.com/alibaba/opensandbox/egress/pkg/log"
	"github.com/alibaba/opensandbox/egress/pkg/nftables"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/alibaba/opensandbox/egress/pkg/telemetry"
	slogger "github.com/alibaba/opensandbox/internal/logger"
	"github.com/alibaba/opensandbox/internal/safego"
)

const defaultListenAddr = "127.0.0.1:15353"

type Proxy struct {
	policyMu                sync.RWMutex
	userPolicy              *policy.NetworkPolicy
	effectivePolicy         *policy.NetworkPolicy
	alwaysDeny              []policy.EgressRule
	alwaysAllow             []policy.EgressRule
	listenAddr              string
	upstreams               []string // ordered resolver chain from discovery (immutable after New)
	upstreamMu              sync.RWMutex
	activeUpstreams         []string // healthy subset; same order as upstreams; used for forwarding
	upstreamProbeName       string   // wire name for probe (FQDN or "." for root)
	upstreamProbeQType      uint16   // dns.TypeA or dns.TypeNS etc.
	upstreamProbeInterval   time.Duration
	upstreamExchangeTimeout time.Duration
	servers                 []*dns.Server
	shutdownOnce            sync.Once

	// When set, called synchronously for allowed A/AAAA answers (dns+nft: program nft before client connects).
	onResolved func(domain string, ips []nftables.ResolvedIP)
	// Optional: async fan-out for denied lookups (e.g. webhook).
	blockedBroadcaster *events.Broadcaster
}

// New constructs the DNS proxy: discovers upstreams, default listen 127.0.0.1:15353 if listenAddr is "".
// alwaysDeny/alwaysAllow are merged via policy.MergeAlwaysOverlay; they are file/operator rules, not persisted by POST /policy.
func New(p *policy.NetworkPolicy, listenAddr string, alwaysDeny, alwaysAllow []policy.EgressRule) (*Proxy, error) {
	if listenAddr == "" {
		listenAddr = defaultListenAddr
	}
	if p == nil {
		p = policy.DefaultDenyPolicy()
	}
	upstreams, err := DiscoverUpstreams()
	if err != nil {
		return nil, err
	}
	probeName, probeQType := upstreamProbeFromEnv()
	proxy := &Proxy{
		listenAddr:              listenAddr,
		upstreams:               upstreams,
		activeUpstreams:         append([]string(nil), upstreams...),
		upstreamProbeName:       probeName,
		upstreamProbeQType:      probeQType,
		upstreamProbeInterval:   upstreamProbeIntervalFromEnv(),
		upstreamExchangeTimeout: upstreamExchangeTimeoutFromEnv(),
		userPolicy:              ensurePolicyDefaults(p),
		alwaysDeny:              append([]policy.EgressRule(nil), alwaysDeny...),
		alwaysAllow:             append([]policy.EgressRule(nil), alwaysAllow...),
	}
	proxy.refreshEffectivePolicy()
	return proxy, nil
}

func (p *Proxy) refreshEffectivePolicy() {
	p.effectivePolicy = policy.MergeAlwaysOverlay(p.userPolicy, p.alwaysDeny, p.alwaysAllow)
}

func upstreamExchangeTimeoutFromEnv() time.Duration {
	s := strings.TrimSpace(os.Getenv(constants.EnvDNSUpstreamTimeout))
	if s == "" {
		return time.Duration(constants.DefaultDNSUpstreamTimeoutSec) * time.Second
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return time.Duration(constants.DefaultDNSUpstreamTimeoutSec) * time.Second
	}
	if n > 120 {
		n = 120
	}
	return time.Duration(n) * time.Second
}

func (p *Proxy) Start(ctx context.Context) error {
	handler := dns.HandlerFunc(p.serveDNS)

	udpServer := &dns.Server{Addr: p.listenAddr, Net: "udp", Handler: handler}
	tcpServer := &dns.Server{Addr: p.listenAddr, Net: "tcp", Handler: handler}
	p.servers = []*dns.Server{udpServer, tcpServer}

	errCh := make(chan error, len(p.servers))
	for _, srv := range p.servers {
		s := srv
		safego.Go(func() {
			if err := s.ListenAndServe(); err != nil {
				errCh <- err
			}
		})
	}

	timer := time.NewTimer(200 * time.Millisecond)
	defer timer.Stop()
	select {
	case err := <-errCh:
		return fmt.Errorf("dns proxy failed: %w", err)
	case <-timer.C:
		// Start upstream probes only after listeners are up, so the first probe does not fail on "not listening".
	}

	safego.Go(func() { p.runUpstreamProbes(ctx) })

	return nil
}

// Shutdown stops UDP and TCP DNS listeners. Safe to call more than once.
func (p *Proxy) Shutdown() error {
	var outErr error
	p.shutdownOnce.Do(func() {
		for _, srv := range p.servers {
			if e := srv.Shutdown(); e != nil && outErr == nil {
				outErr = e
			}
		}
	})
	return outErr
}

func (p *Proxy) serveDNS(w dns.ResponseWriter, r *dns.Msg) {
	if len(r.Question) == 0 {
		_ = w.WriteMsg(new(dns.Msg))
		return
	}
	q := r.Question[0]
	domain := q.Name
	host := normalizeDNSHost(domain)

	p.policyMu.RLock()
	currentPolicy := p.effectivePolicy
	p.policyMu.RUnlock()
	if currentPolicy != nil && currentPolicy.Evaluate(domain) == policy.ActionDeny {
		telemetry.RecordDNSDenied()
		p.publishBlocked(domain)
		resp := new(dns.Msg)
		resp.SetRcode(r, dns.RcodeNameError)
		_ = w.WriteMsg(resp)
		return
	}

	start := time.Now()
	resp, err := p.forward(r)
	elapsed := time.Since(start).Seconds()
	if err != nil {
		telemetry.RecordDNSForward(elapsed)
		logOutboundDNS(host, nil, "", err.Error())
		log.Warnf("[dns] forward error for %s: %v", domain, err)
		fail := new(dns.Msg)
		fail.SetRcode(r, dns.RcodeServerFailure)
		_ = w.WriteMsg(fail)
		return
	}
	telemetry.RecordDNSForward(elapsed)
	logOutboundDNS(host, resolvedIPStrings(resp), "", "")
	p.maybeNotifyResolved(domain, resp)
	_ = w.WriteMsg(resp)
}

// maybeNotifyResolved calls onResolved before w.WriteMsg so dynamic nft allows are installed
// before the client receives the answer and may open a connection.
func (p *Proxy) maybeNotifyResolved(domain string, resp *dns.Msg) {
	if p.onResolved == nil {
		return
	}
	ips := extractResolvedIPs(resp)
	if len(ips) == 0 {
		return
	}
	p.onResolved(domain, ips)
}

func (p *Proxy) forward(r *dns.Msg) (*dns.Msg, error) {
	list := p.forwardUpstreams()
	var lastErr error
	for _, upstream := range list {
		c := &dns.Client{
			Timeout: p.upstreamExchangeTimeout,
			Dialer:  p.dialerForUpstream(upstream),
		}
		resp, _, err := c.Exchange(r, upstream)
		if err != nil {
			lastErr = err
			log.Warnf("[dns] upstream %s exchange error: %v", upstream, err)
			continue
		}
		if resp == nil {
			lastErr = fmt.Errorf("nil response from %s", upstream)
			continue
		}
		if tryNext, reason := p.shouldFailoverAfterResponse(resp); tryNext {
			lastErr = fmt.Errorf("%s from %s", reason, upstream)
			log.Warnf("[dns] upstream %s: %s; trying next", upstream, reason)
			continue
		}
		return resp, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("no upstream resolvers configured")
}

// shouldFailoverAfterResponse: treat NXDOMAIN and NOERROR as final (no retry). Other rcodes may
// move to the next upstream (e.g. SERVFAIL).
func (p *Proxy) shouldFailoverAfterResponse(resp *dns.Msg) (tryNext bool, reason string) {
	if resp == nil {
		return true, "nil response"
	}
	switch resp.Rcode {
	case dns.RcodeNameError:
		return false, ""
	case dns.RcodeSuccess:
		return false, ""
	default:
		rcStr := dns.RcodeToString[resp.Rcode]
		if rcStr == "" {
			rcStr = fmt.Sprintf("rcode %d", resp.Rcode)
		}
		return true, rcStr
	}
}

func (p *Proxy) UpstreamHost() string {
	list := p.forwardUpstreams()
	if len(list) == 0 {
		return ""
	}
	host, _, err := net.SplitHostPort(list[0])
	if err != nil {
		return ""
	}
	return host
}

// UpdatePolicy replaces the user policy from POST/GET /policy (not the always file overlay). Nil → default deny-all.
func (p *Proxy) UpdatePolicy(newPolicy *policy.NetworkPolicy) {
	p.policyMu.Lock()
	defer p.policyMu.Unlock()

	p.userPolicy = ensurePolicyDefaults(newPolicy)
	p.refreshEffectivePolicy()
}

// UpdateAlwaysRules replaces the always-deny/always-allow file overlay used only for evaluation (merged in refreshEffectivePolicy).
func (p *Proxy) UpdateAlwaysRules(alwaysDeny, alwaysAllow []policy.EgressRule) {
	p.policyMu.Lock()
	defer p.policyMu.Unlock()

	p.alwaysDeny = append([]policy.EgressRule(nil), alwaysDeny...)
	p.alwaysAllow = append([]policy.EgressRule(nil), alwaysAllow...)
	p.refreshEffectivePolicy()
}

// CurrentPolicy is the last user policy from the API, without always file overlay in the struct (overlay is in effectivePolicy).
func (p *Proxy) CurrentPolicy() *policy.NetworkPolicy {
	p.policyMu.RLock()
	defer p.policyMu.RUnlock()

	return p.userPolicy
}

// SetOnResolved registers the dns+nft path (nil in dns-only). Invoked on the same goroutine as serveDNS, before WriteMsg.
func (p *Proxy) SetOnResolved(fn func(domain string, ips []nftables.ResolvedIP)) {
	p.onResolved = fn
}

// SetBlockedBroadcaster wires the optional publisher for policy-denied lookups.
func (p *Proxy) SetBlockedBroadcaster(b *events.Broadcaster) {
	p.blockedBroadcaster = b
}

func (p *Proxy) publishBlocked(domain string) {
	if p.blockedBroadcaster == nil {
		return
	}
	normalized := strings.ToLower(strings.TrimSuffix(domain, "."))
	if normalized == "" {
		return
	}

	p.blockedBroadcaster.Publish(events.BlockedEvent{
		Hostname:  normalized,
		Timestamp: time.Now().UTC(),
	})
}

// extractResolvedIPs collects A/AAAA from resp.Answer with TTLs for dynamic nft elements.
func extractResolvedIPs(resp *dns.Msg) []nftables.ResolvedIP {
	if resp == nil || len(resp.Answer) == 0 {
		return nil
	}

	var out []nftables.ResolvedIP
	for _, rr := range resp.Answer {
		switch v := rr.(type) {
		case *dns.A:
			if v.A == nil {
				continue
			}
			addr, err := netip.ParseAddr(v.A.String())
			if err != nil {
				continue
			}
			out = append(out, nftables.ResolvedIP{Addr: addr, TTL: time.Duration(v.Hdr.Ttl) * time.Second})
		case *dns.AAAA:
			if v.AAAA == nil {
				continue
			}
			addr, err := netip.ParseAddr(v.AAAA.String())
			if err != nil {
				continue
			}
			out = append(out, nftables.ResolvedIP{Addr: addr, TTL: time.Duration(v.Hdr.Ttl) * time.Second})
		}
	}
	return out
}

const fallbackUpstream = "8.8.8.8:53"

// DiscoverUpstreams is env OPENSANDBOX_EGRESS_DNS_UPSTREAM if set, else /etc/resolv.conf (with caps/fallbacks).
func DiscoverUpstreams() ([]string, error) {
	raw := strings.TrimSpace(os.Getenv(constants.EnvDNSUpstream))
	if raw != "" {
		return parseEnvDNSUpstreams(raw)
	}
	return discoverUpstreamsFromResolv()
}

// parseEnvDNSUpstreams splits OPENSANDBOX_EGRESS_DNS_UPSTREAM (comma-separated); each entry must pass normalizeEnvUpstreamAddr.
func parseEnvDNSUpstreams(raw string) ([]string, error) {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		addr, err := normalizeEnvUpstreamAddr(part)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", constants.EnvDNSUpstream, err)
		}
		out = append(out, addr)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s must list at least one upstream resolver", constants.EnvDNSUpstream)
	}
	return dedupeUpstreamAddrs(out), nil
}

// normalizeEnvUpstreamAddr requires a literal IP (optional :port). Resolving a hostname to :53 would hit
// OUTPUT REDIRECT to this proxy again and recurse.
func normalizeEnvUpstreamAddr(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("empty upstream address")
	}
	if host, port, err := net.SplitHostPort(s); err == nil {
		if port == "" {
			return "", fmt.Errorf("invalid port in %q", s)
		}
		if _, err := netip.ParseAddr(host); err != nil {
			return "", fmt.Errorf("host %q must be a literal IP address, not a hostname (avoids DNS self-recursion with REDIRECT)", host)
		}
		return net.JoinHostPort(host, port), nil
	}
	if strings.HasPrefix(s, "[") {
		if !strings.HasSuffix(s, "]") {
			return "", fmt.Errorf("invalid bracketed IPv6 %q", s)
		}
		inner := strings.TrimPrefix(strings.TrimSuffix(s, "]"), "[")
		if _, err := netip.ParseAddr(inner); err != nil {
			return "", fmt.Errorf("invalid IP inside brackets %q", s)
		}
		return net.JoinHostPort(inner, "53"), nil
	}
	addr, err := netip.ParseAddr(s)
	if err != nil {
		return "", fmt.Errorf("upstream %q must be a literal IP address, not a hostname: %w", s, err)
	}
	return net.JoinHostPort(addr.String(), "53"), nil
}

func discoverUpstreamsFromResolv() ([]string, error) {
	cfg, err := dns.ClientConfigFromFile("/etc/resolv.conf")
	if err != nil || len(cfg.Servers) == 0 {
		if err != nil {
			log.Warnf("[dns] fallback upstream resolver due to error: %v", err)
		}
		return []string{fallbackUpstream}, nil
	}
	port := cfg.Port
	if port == "" {
		port = "53"
	}
	var nonLoop, loop []string
	for _, s := range cfg.Servers {
		addr := net.JoinHostPort(s, port)
		if ip := net.ParseIP(s); ip != nil && ip.IsLoopback() {
			loop = append(loop, addr)
			continue
		}
		nonLoop = append(nonLoop, addr)
	}
	out := append(nonLoop, loop...)
	if len(out) == 0 {
		out = []string{net.JoinHostPort(cfg.Servers[0], port)}
	}
	if len(out) > constants.ResolvNameserverCap {
		out = out[:constants.ResolvNameserverCap]
	}
	return dedupeUpstreamAddrs(out), nil
}

func dedupeUpstreamAddrs(addrs []string) []string {
	seen := make(map[string]struct{}, len(addrs))
	var out []string
	for _, a := range addrs {
		if _, ok := seen[a]; ok {
			continue
		}
		seen[a] = struct{}{}
		out = append(out, a)
	}
	return out
}

// AllowIPsFromUpstreamAddrs collects literal resolver IPs from the host:port upstream list (nft allow, deduped).
func AllowIPsFromUpstreamAddrs(upstreams []string) []netip.Addr {
	var out []netip.Addr
	seen := make(map[netip.Addr]struct{})
	for _, a := range upstreams {
		host, _, err := net.SplitHostPort(a)
		if err != nil {
			continue
		}
		ip, err := netip.ParseAddr(host)
		if err != nil {
			continue
		}
		if _, ok := seen[ip]; ok {
			continue
		}
		seen[ip] = struct{}{}
		out = append(out, ip)
	}
	return out
}

// ResolvNameserverIPs parses resolvPath nameserver lines; used to allow the system resolver IPs in nft
// and align client vs. proxy forward path.
func ResolvNameserverIPs(resolvPath string) ([]netip.Addr, error) {
	cfg, err := dns.ClientConfigFromFile(resolvPath)
	if err != nil || len(cfg.Servers) == 0 {
		return nil, nil
	}
	var out []netip.Addr
	for _, s := range cfg.Servers {
		ip, err := netip.ParseAddr(s)
		if err != nil {
			continue
		}
		out = append(out, ip)
	}
	return out, nil
}

func normalizeDNSHost(domain string) string {
	return strings.ToLower(strings.TrimSuffix(domain, "."))
}

func resolvedIPStrings(resp *dns.Msg) []string {
	ri := extractResolvedIPs(resp)
	if len(ri) == 0 {
		return nil
	}
	out := make([]string, 0, len(ri))
	for _, x := range ri {
		out = append(out, x.Addr.String())
	}
	return out
}

func logOutboundDNS(host string, ips []string, peer string, errStr string) {
	fields := []slogger.Field{
		{Key: "opensandbox.event", Value: "egress.outbound"},
	}
	if host != "" {
		fields = append(fields, slogger.Field{Key: "target.host", Value: host})
	}
	if peer != "" {
		fields = append(fields, slogger.Field{Key: "peer", Value: peer})
	}
	if len(ips) > 0 {
		fields = append(fields, slogger.Field{Key: "target.ips", Value: ips})
	}
	if errStr != "" {
		fields = append(fields, slogger.Field{Key: "error", Value: errStr})
	}
	log.Logger.With(fields...).Infof("egress outbound")
}

func ensurePolicyDefaults(p *policy.NetworkPolicy) *policy.NetworkPolicy {
	if p == nil {
		return policy.DefaultDenyPolicy()
	}
	if p.DefaultAction == "" {
		p.DefaultAction = policy.ActionDeny
	}
	return p
}
