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

// Fast Sandbox-profile assembly: a single egress control plane serving N
// sandboxes sharing one host/network domain. Activated by
// OPENSANDBOX_EGRESS_PROFILE=fast-sandbox; the sidecar profile is unchanged.
//
// Control flow:
//
//	fastlet action protocol --(SET_BINDING / LIFECYCLE_HOOK / REMOVE_BINDING)-->
//	  fastSandboxPolicyServer:18080 (loopback): subject lifecycle + deny-first nft,
//	  policy activation on sandbox.data-plane-ready
//	proxy route --(UID header)--> fastSandboxPolicyServer:18080 (loopback)
//	  policy/credential pushes routed per subject (vault memory-only)
//	DNS: one shared proxy, per-query policy via source IP dispatch
//
// Subject lifecycle is driven entirely by the Fastlet (Sandbox Actions
// Handler protocol); there is no local observation source. On egress restart
// the Fastlet detects the new handler instanceId and replays every live
// binding (SET_BINDING + reached Hooks) — no rescan needed.
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/dnsproxy"
	"github.com/alibaba/opensandbox/egress/pkg/fastsandboxnft"
	"github.com/alibaba/opensandbox/egress/pkg/iptables"
	"github.com/alibaba/opensandbox/egress/pkg/log"
	"github.com/alibaba/opensandbox/egress/pkg/mitmproxy"
	"github.com/alibaba/opensandbox/egress/pkg/nftables"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/alibaba/opensandbox/egress/pkg/subject"
	"github.com/alibaba/opensandbox/egress/pkg/telemetry"
	"github.com/alibaba/opensandbox/internal/safego"
)

// runFastSandboxProfile starts the fast-sandbox-profile control plane and blocks until ctx is
// canceled or a fatal error occurs. upstreamSpec is the validated chained
// upstream proxy (nil when disabled): containment is enforced profile-wide
// in the nft table and the proxy hostname is registered as an infra domain
// on the shared dnsproxy.
func runFastSandboxProfile(ctx context.Context, upstreamSpec *mitmproxy.UpstreamProxySpec) {
	log.Infof("egress profile: fast-sandbox (multi-sandbox control plane)")

	// Erase any stale mitmproxy CA left on the shared volume (root and the
	// fast-sandbox mount subdir) by a previous egress generation, so a sandbox's
	// bootstrap can never install a CA this generation no longer signs with
	// (upstream issue #1370, fast-sandbox issue #19).
	mitmproxy.PurgeStaleExportedCA()

	otelShutdown, err := telemetry.Init(ctx)
	if err != nil {
		log.Warnf("OpenTelemetry metrics disabled (continuing without OTLP): %v", err)
		otelShutdown = nil
	}
	if otelShutdown != nil {
		defer func() {
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer shutdownCancel()
			_ = otelShutdown(shutdownCtx)
		}()
	}

	alwaysDeny, alwaysAllow, err := policy.LoadAlwaysRuleFiles()
	if err != nil {
		log.Fatalf("failed to load always allow/deny rule files: %v", err)
	}

	podNft := fastsandboxnft.NewApplier(nil, fastSandboxNftOptions(upstreamSpec))
	// Recovery: wipe stale rules from a previous egress generation BEFORE
	// serving action requests, so no dead subject's policy survives into a
	// new sandbox. The Fastlet then detects the new handler instanceId and
	// replays every live binding through the same registration path.
	if err := podNft.ApplyReset(ctx); err != nil {
		log.Fatalf("fast-sandbox nftables reset failed: %v", err)
	}
	log.Infof("fast-sandbox nftables table reset (stale rules cleared)")

	reg := subject.NewRegistry(alwaysDeny, alwaysAllow)
	pendingTTL := time.Duration(constants.EnvIntOrDefault(constants.EnvPendingPushTTL, constants.DefaultPendingPushTTL)) * time.Second
	fastSandboxSrv := newFastSandboxPolicyServer(ctx, reg, podNft, pendingTTL)

	// Shared mitmproxy (OSEP-0022 A1): one mitmdump in the Pod netns serving
	// every sandbox. Started BEFORE the HTTP listener so subjects can never
	// register against a missing interceptor (fail-closed registration); the
	// per-subject prerouting DNAT is installed by the fast-sandbox server on
	// registration. A disabled MITM skips the whole block.
	mitmGate := mitmproxy.NewHealthGate()
	var fastSandboxMitm *mitmTransparent
	if mitm, err := startFastSandboxMitmproxyIfEnabled(); err != nil {
		log.Fatalf("fast-sandbox mitmproxy start failed: %v", err)
	} else if mitm != nil {
		fastSandboxMitm = mitm
		dports, err := constants.BuildMitmproxyPorts(os.Getenv(constants.EnvMitmproxyExtraPorts))
		if err != nil {
			log.Fatalf("fast-sandbox mitmproxy ports: %v", err)
		}
		fastSandboxSrv.SetMitm(mitmGate, mitm.port, dports)
		mitm.watchMitmproxy(ctx, mitmGate)
		mitmGate.SetReady(true)
		log.Infof("fast-sandbox mitmproxy watch started (shared listener, healthz-gated)")
		startFastSandboxActiveSocket(ctx, fastSandboxSrv)
	} else {
		fastSandboxSrv.SetMitm(nil, 0, nil)
		// MITM disabled: clear any interception table a previous generation
		// (running with MITM enabled) may have left — stale rules would keep
		// DNATing 80/443 to a now-unserved mitmproxy port (blackhole).
		if err := iptables.RemoveMitmRedirects(); err != nil {
			log.Warnf("fast-sandbox mitmproxy: stale redirect table cleanup failed (ignored): %v", err)
		}
	}

	// DNS: one shared listener. Bound on :15353 (all interfaces — a
	// prerouting REDIRECT retargets sandbox DNS to the interface address,
	// NOT loopback, so a 127.0.0.1 bind would never receive it; :15353 also
	// never collides with a host DNS service on :53). Per-subject gateway
	// REDIRECTs (fast-sandbox server's installGatewayDNSRedirect) forward sandbox
	// DNS addressed to gateway:53 here; per-query policy is dispatched by
	// source IP.
	dnsAddr := ":15353"
	proxy, err := dnsproxy.New(nil, dnsAddr, alwaysDeny, alwaysAllow)
	if err != nil {
		log.Fatalf("failed to init dns proxy: %v", err)
	}
	if upstreamSpec != nil {
		if _, parseErr := netip.ParseAddr(upstreamSpec.Host); parseErr != nil {
			// Hostname endpoint: register it as an infrastructure domain so
			// sandbox lookups resolve without per-subject policy and NEVER
			// feed the dyn allow sets (the open-relay bypass), while the
			// answers keep the profile-wide drop sets fresh. The drop-set
			// refresh resolves through BOTH resolver authorities (see
			// resolveUpstreamProxyHost): the dnsproxy's forward upstreams and
			// the Pod's own resolver — the authority the shared mitmdump
			// dials through, since the profile deliberately installs no
			// Pod-OUTPUT DNS redirect.
			host := upstreamSpec.Host
			proxy.SetInfraDomain(host, func(domain string, ips []nftables.ResolvedIP) {
				addCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				defer cancel()
				if err := podNft.AddUpstreamProxyIPs(addCtx, ips); err != nil {
					log.Warnf("upstream proxy: nft update for %q failed: %v", domain, err)
				}
			})
			if err := podNft.StartUpstreamProxyRefresh(ctx, host, func(ctx context.Context, domain string) ([]nftables.ResolvedIP, error) {
				return resolveUpstreamProxyHost(ctx, domain, proxy.ResolveDomain)
			}); err != nil {
				// Fail closed like every other startup step: serving sandbox
				// actions with an unseeded drop set would let any subject
				// that learns the proxy IP CONNECT it directly.
				log.Fatalf("fast-sandbox upstream proxy %q: %v", host, err)
			}
			log.Infof("upstream proxy: registered infra DNS domain %q (profile-wide sandbox drop, no allow-set feed, dual-resolver refresh)", host)
		} else {
			log.Infof("upstream proxy: literal endpoint %s:%d (profile-wide sandbox drop)", upstreamSpec.Host, upstreamSpec.Port)
		}
	}
	proxy.SetQueryPolicySelector(func(remote netip.Addr) (*dnsproxy.QueryPolicy, string) {
		s, ok := reg.Resolve(subject.SubjectKey{SourceIP: remote})
		if !ok {
			// Unknown source: deny (fail closed), never a default policy.
			return nil, "unknown source"
		}
		eff := reg.EffectivePolicy(s)
		if eff == nil {
			return nil, "no effective policy for subject " + string(s)
		}
		return &dnsproxy.QueryPolicy{
			Policy: eff,
			OnResolved: func(domain string, ips []nftables.ResolvedIP) {
				addCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				defer cancel()
				if err := podNft.AddResolvedIPs(addCtx, s, ips); err != nil {
					log.Warnf("[dns] add resolved IPs to fast-sandbox nft failed for subject %s domain %q: %v", s, domain, err)
				}
			},
		}, ""
	})
	if err := proxy.Start(ctx); err != nil {
		log.Fatalf("failed to start dns proxy: %v", err)
	}
	log.Infof("fast-sandbox dns proxy listening on %s", dnsAddr)

	httpAddr := envOrDefault(constants.EnvEgressHTTPAddr, constants.DefaultFastSandboxServerAddr)
	srv := &http.Server{Addr: httpAddr, Handler: fastSandboxSrv.Handler()}
	safego.Go(func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("fast-sandbox policy server error: %v", err)
		}
	})
	log.Infof("fast-sandbox policy server listening on %s (actions + UID-header routed)", httpAddr)

	fastSandboxSrv.StartPendingSweep(ctx)

	// Per-subject connection refresh: active TCP connections keep their
	// dynamic leases alive (bucketed by source IP from the Pod netns
	// conntrack table).
	podNft.StartConnectionRefresh(ctx, nil)
	log.Infof("fast-sandbox connection refresh started (bucketed per subject, every 30s)")

	// Block until shutdown.
	<-ctx.Done()
	log.Infof("received shutdown signal; shutting down fast-sandbox profile")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Errorf("fast-sandbox policy server shutdown error: %v", err)
	}
	if err := proxy.Shutdown(); err != nil {
		log.Errorf("fast-sandbox dns proxy shutdown error: %v", err)
	}
	if fastSandboxMitm != nil {
		fastSandboxMitm.shutdown(3 * time.Second)
	}
	// Enforcement is intentionally NOT removed: the kernel rules keep denying
	// while the daemon is down (fail closed); the next start wipes them via
	// ApplyReset before serving.
	log.Infof("fast-sandbox profile shutdown complete")
	_ = os.Stderr.Sync()
}

// fastSandboxNftOptions assembles the fast-sandbox nft options: the shared
// DoH-443 blocking env (OPENSANDBOX_EGRESS_BLOCK_DOH_443 strict all-443 drop
// when the blocklist is empty + OPENSANDBOX_EGRESS_DOH_BLOCKLIST
// comma-separated IP/CIDR list), the mitm redirect port (Pod-netns INPUT
// enforcement chain for intercepted (DNATed) traffic; 0 when MITM is off),
// and — when a chained upstream proxy is configured — the profile-wide
// endpoint drop. Same semantics as the sidecar profile for the shared parts.
func fastSandboxNftOptions(upstreamSpec *mitmproxy.UpstreamProxySpec) fastsandboxnft.Options {
	opts := fastSandboxDoHOptions()
	opts.UpstreamProxy = fastSandboxUpstreamEndpoint(upstreamSpec)
	return opts
}

// fastSandboxUpstreamEndpoint translates the validated chained upstream
// proxy spec into the fast-sandbox containment endpoint. A literal IP seeds
// the drop sets permanently; a hostname starts empty (fed by the infra
// domain DNS path and the self-refresh loop). Port is always non-zero: the
// spec parser fills the scheme default.
func fastSandboxUpstreamEndpoint(spec *mitmproxy.UpstreamProxySpec) *fastsandboxnft.UpstreamProxyEndpoint {
	if spec == nil {
		return nil
	}
	ep := &fastsandboxnft.UpstreamProxyEndpoint{Port: spec.Port}
	if ip, err := netip.ParseAddr(spec.Host); err == nil {
		ep.LiteralIPs = []netip.Addr{ip.Unmap()}
	}
	return ep
}

// fastSandboxDoHOptions parses the shared DoH-443 blocking env for the fast-sandbox
// profile: OPENSANDBOX_EGRESS_BLOCK_DOH_443 (strict all-443 drop when the
// blocklist is empty) + OPENSANDBOX_EGRESS_DOH_BLOCKLIST (comma-separated
// IP/CIDR list), same semantics as the sidecar profile. MitmRedirectPort
// enables the Pod-netns INPUT enforcement chain for intercepted (DNATed)
// traffic; 0 when MITM is off.
func fastSandboxDoHOptions() fastsandboxnft.Options {
	opts := fastsandboxnft.Options{BlockDoH443: constants.IsTruthy(os.Getenv(constants.EnvBlockDoH443))}
	if raw := strings.TrimSpace(os.Getenv(constants.EnvDoHBlocklist)); raw != "" {
		opts.DoHBlocklistV4, opts.DoHBlocklistV6 = parseDoHBlocklist(raw)
	}
	if constants.IsTruthy(os.Getenv(constants.EnvMitmproxyTransparent)) {
		opts.MitmRedirectPort = constants.EnvIntOrDefault(constants.EnvMitmproxyPort, constants.DefaultMitmproxyPort)
	}
	return opts
}

// resolveUpstreamProxyHost resolves the upstream proxy hostname through BOTH
// resolver authorities that can map it and returns the union. The drop sets
// would otherwise be seeded only from the dnsproxy's forward upstreams
// (OPENSANDBOX_EGRESS_DNS_UPSTREAM or /etc/resolv.conf — the answers
// sandboxes can observe), while the shared mitmdump dials through the fastlet
// Pod's own resolver: split-horizon or operator-configured DNS can make the
// two return different address sets, and an address only the Pod resolver
// returns is exactly one a sandbox could CONNECT directly (the open-relay
// bypass). dnsLookup is injected for testing; in production it is the shared
// dnsproxy's ResolveDomain. Pod-resolver answers carry no TTL (the
// AddUpstreamProxyIPs TTL floor bounds them); an error is returned only when
// both authorities fail.
func resolveUpstreamProxyHost(ctx context.Context, domain string, dnsLookup func(context.Context, string) ([]nftables.ResolvedIP, error)) ([]nftables.ResolvedIP, error) {
	ips, err := unionResolver(ctx, domain, dnsLookup, resolveViaPodResolver)
	if err != nil {
		return nil, fmt.Errorf("both resolver authorities failed for %q: %w", domain, err)
	}
	return ips, nil
}

// resolveViaPodResolver resolves through the Go default resolver —
// /etc/resolv.conf and /etc/hosts, the same sources mitmdump's glibc resolver
// uses for the chained dial. No TTL is knowable here (the AddUpstreamProxyIPs
// TTL floor bounds the elements).
func resolveViaPodResolver(ctx context.Context, domain string) ([]nftables.ResolvedIP, error) {
	addrs, err := net.DefaultResolver.LookupIP(ctx, "ip", domain)
	if err != nil {
		return nil, err
	}
	var ips []nftables.ResolvedIP
	for _, a := range addrs {
		if addr, ok := netip.AddrFromSlice(a); ok {
			ips = append(ips, nftables.ResolvedIP{Addr: addr.Unmap()})
		}
	}
	return ips, nil
}

// unionResolver runs every lookup and unions the answers, tolerating partial
// failure: an error is returned only when every authority failed. A
// consistent-but-empty union (e.g. NXDOMAIN everywhere) is not an error —
// the refresh loop logs it as "no addresses".
func unionResolver(ctx context.Context, domain string, lookups ...func(context.Context, string) ([]nftables.ResolvedIP, error)) ([]nftables.ResolvedIP, error) {
	var (
		ips  []nftables.ResolvedIP
		errs []error
	)
	for _, lookup := range lookups {
		resolved, err := lookup(ctx, domain)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		ips = append(ips, resolved...)
	}
	if len(errs) == len(lookups) {
		return nil, errors.Join(errs...)
	}
	return unionResolvedIPs(ips), nil
}

// unionResolvedIPs dedupes per address, preferring the TTL-bearing entry:
// the pod-resolver path reports TTL 0 and must not shorten a TTL the dnsproxy
// authority reported for the same address.
func unionResolvedIPs(ips []nftables.ResolvedIP) []nftables.ResolvedIP {
	out := make([]nftables.ResolvedIP, 0, len(ips))
	index := make(map[netip.Addr]int, len(ips))
	for _, r := range ips {
		if i, ok := index[r.Addr]; ok {
			if out[i].TTL == 0 && r.TTL > 0 {
				out[i] = r
			}
			continue
		}
		index[r.Addr] = len(out)
		out = append(out, r)
	}
	return out
}
