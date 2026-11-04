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
//	fastlet action protocol --(SET_BINDING / LIFECYCLE_HOOK / REMOVE_BINDING /
//	                         VAULT_EXPORT / VAULT_IMPORT /
//	                         POLICY_EXPORT / POLICY_IMPORT)-->
//	  fastSandboxPolicyServer:18080 (loopback): subject lifecycle + deny-first nft,
//	  policy activation on sandbox.data-plane-ready. The binding input seeds the
//	  policy at create; runtime mutations go through the handler-relay surfaces
//	  (/policy/{uid} for the recorded policy, /vault/{uid} for the credential
//	  vault, both egress-held per OSEP-0022) and the persisted files supersede
//	  replayed binding inputs after a restart.
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
	"path/filepath"
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
	actionsAPIVersion, err := constants.ParseFastSandboxActionsAPIVersion(os.Getenv(constants.EnvFastSandboxActionsAPIVersion))
	if err != nil {
		log.Fatalf("fast-sandbox Actions protocol: %v", err)
	}
	if err := validateOwnerDomain(os.Getenv(constants.EnvEgressPolicyDir), actionsAPIVersion); err != nil {
		log.Fatalf("fast-sandbox Actions domain: %v", err)
	}

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
	// Construct the resolver without starting its listener. Seed hostname
	// containment BEFORE touching the previous generation's kernel table.
	dnsAddr := ":15353"
	proxy, err := dnsproxy.New(nil, dnsAddr, alwaysDeny, alwaysAllow)
	if err != nil {
		log.Fatalf("failed to init dns proxy: %v", err)
	}
	upstreamLookup := func(ctx context.Context, domain string) ([]nftables.ResolvedIP, error) {
		return resolveUpstreamProxyHost(ctx, domain, proxy.ResolveDomain)
	}
	if upstreamSpec != nil {
		if _, parseErr := netip.ParseAddr(upstreamSpec.Host); parseErr != nil {
			if err := podNft.SeedUpstreamProxyIPs(ctx, upstreamSpec.Host, upstreamLookup); err != nil {
				log.Fatalf("fast-sandbox upstream proxy %q: %v", upstreamSpec.Host, err)
			}
		}
	}
	// Recovery: wipe stale rules from a previous egress generation BEFORE
	// serving action requests, so no dead subject's policy survives into a
	// new sandbox. The Fastlet then detects the new handler instanceId and
	// replays every live binding through the same registration path.
	if err := podNft.ApplyReset(ctx); err != nil {
		log.Fatalf("fast-sandbox nftables reset failed: %v", err)
	}
	log.Infof("fast-sandbox nftables table reset (stale rules cleared)")

	reg := subject.NewRegistry(alwaysDeny, alwaysAllow)
	fastSandboxSrv := newFastSandboxPolicyServer(ctx, reg, podNft)
	fastSandboxSrv.SetActionsAPIVersion(actionsAPIVersion)

	// Register the configured state-directory basenames BEFORE either
	// constructor runs: the volume-root restriction must recognize the
	// configured sibling as ours even on custom-named deployments.
	vaultDir := strings.TrimSpace(os.Getenv(constants.EnvEgressVaultDir))
	policyDir := strings.TrimSpace(os.Getenv(constants.EnvEgressPolicyDir))
	if policyDir == "" {
		log.Fatalf("fast-sandbox policy persistence: OPENSANDBOX_EGRESS_POLICY_DIR is required (the recorded policy is egress-held state)")
	}
	registerStateDirName(vaultDir)
	registerStateDirName(policyDir)
	// The two state directories are siblings holding same-named per-subject
	// files (<uid>.json) and seal markers: aliasing them would make each
	// persister clobber the other's state (a restart-time rehydrate of one
	// kind quarantines the other kind's file). Reject before any state is
	// written.
	if vaultDir != "" && policyDir != "" && sameStateDir(vaultDir, policyDir) {
		log.Fatalf("fast-sandbox policy persistence: OPENSANDBOX_EGRESS_POLICY_DIR must differ from OPENSANDBOX_EGRESS_VAULT_DIR (both resolve to %s)", policyDir)
	}

	// Vault persistence (OSEP-0022 stage 2): per-subject vault files under
	// OPENSANDBOX_EGRESS_VAULT_DIR so an egress restart recovers locally at
	// SET_BINDING replay. Empty/unset keeps the vault memory-only. Fail fast
	// on an unusable directory: silently serving memory-only would break the
	// acked ⇒ durable contract.
	if vaultDir != "" {
		vp, err := newVaultPersistence(vaultDir)
		if err != nil {
			log.Fatalf("fast-sandbox vault persistence: %v", err)
		}
		fastSandboxSrv.SetVaultFiles(vp)
		log.Infof("fast-sandbox vault persistence enabled at %s (one file per subject, atomic commit protocol)", vaultDir)
	}

	pp, err := newPolicyPersistence(policyDir)
	if err != nil {
		log.Fatalf("fast-sandbox policy persistence: %v", err)
	}
	fastSandboxSrv.SetPolicyFiles(pp)
	log.Infof("fast-sandbox policy persistence enabled at %s (one file per subject, atomic commit protocol)", policyDir)

	// Shared mitmproxy (OSEP-0022 A1): one mitmdump in the Pod netns serving
	// every sandbox. Started BEFORE the HTTP listener so subjects can never
	// register against a missing interceptor (fail-closed registration); the
	// per-subject prerouting DNAT is installed by the fast-sandbox server on
	// registration. A disabled MITM skips the whole block. A pinned-CA failure
	// is fail-closed scoped to the addon: mitmdump refuses to start, the down
	// state is surfaced on /healthz and the actions status payload, and the
	// rest of egress keeps serving policy actions.
	mitmGate := mitmproxy.NewHealthGate()
	var fastSandboxMitm *mitmTransparent
	if mitm, err := startFastSandboxMitmproxyIfEnabled(mitmGate); err != nil {
		if _, down := mitmGate.DownReason(); !down {
			// Infrastructure failure (mitmdump binary, user, listen port):
			// fatal, as before. Only pinned-CA failures are scoped to the
			// addon-down path below (OSEP-0022).
			log.Fatalf("fast-sandbox mitmproxy start failed: %v", err)
		}
		log.Errorf("fast-sandbox mitmproxy addon down (policy actions keep serving): %v", err)
		fastSandboxSrv.SetMitm(mitmGate, 0, nil)
		// MITM down: clear any interception table a previous generation
		// (running with MITM up) may have left — stale rules would keep
		// DNATing 80/443 to a now-unserved mitmproxy port (blackhole).
		if err := iptables.RemoveMitmRedirects(); err != nil {
			log.Warnf("fast-sandbox mitmproxy: stale redirect table cleanup failed (ignored): %v", err)
		}
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
			podNft.StartUpstreamProxyRefresh(ctx, host, upstreamLookup)
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
	// Explicit read timeouts bound a stalled client; the vault relay parses
	// bodies before taking the server lock, so these are the second line of
	// defense (all bodies are bounded and small, handlers are quick).
	srv := &http.Server{
		Addr:              httpAddr,
		Handler:           fastSandboxSrv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	safego.Go(func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("fast-sandbox policy server error: %v", err)
		}
	})
	log.Infof("fast-sandbox policy server listening on %s (Sandbox Actions Handler)", httpAddr)

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

// sameStateDir reports whether two state-directory paths resolve to the
// same directory (lexical cleaning plus best-effort symlink resolution).
func sameStateDir(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if a == b {
		return true
	}
	ra, erra := filepath.EvalSymlinks(a)
	rb, errb := filepath.EvalSymlinks(b)
	return erra == nil && errb == nil && ra == rb
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
	opts.PortFloorDenyRanges = fastSandboxPortFloor()
	return opts
}

// fastSandboxPortFloor reads reachable.ports.cap and returns its complement
// as the profile-wide drop intervals. Load errors fail closed (the floor is
// a platform control, not an optimization); a missing or empty file means
// no floor.
func fastSandboxPortFloor() []policy.PortRange {
	allowed, err := policy.LoadReachablePortsCap()
	if err != nil {
		log.Fatalf("failed to load %s: %v", "reachable.ports.cap", err)
	}
	if len(allowed) == 0 {
		return nil
	}
	return policy.DenyRangesForAllowedPorts(allowed)
}

// fastSandboxUpstreamEndpoint translates the validated chained upstream
// proxy spec into the fast-sandbox containment endpoint. A literal IP seeds
// the drop sets permanently; a hostname starts empty (fed by the infra
// domain DNS path and the self-refresh loop). Port is always non-zero: the
// spec parser fills the scheme default. Hostnames are seeded before ApplyReset.
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
// dnsproxy's ResolveDomain. Pod-resolver answers carry no TTL; drop elements
// are permanent. Partial answers accompany an error so refreshes can add
// them without pruning, while startup requires a complete union.
func resolveUpstreamProxyHost(ctx context.Context, domain string, dnsLookup func(context.Context, string) ([]nftables.ResolvedIP, error)) ([]nftables.ResolvedIP, error) {
	ips, err := unionResolver(ctx, domain, dnsLookup, resolveViaPodResolver)
	if err != nil {
		return ips, fmt.Errorf("resolver union for %q (authority 1=dnsproxy, 2=pod): %w", domain, err)
	}
	return ips, nil
}

// resolveViaPodResolver resolves through the Go default resolver —
// /etc/resolv.conf and /etc/hosts, the same sources mitmdump's glibc resolver
// uses for the chained dial. No TTL is knowable here; the drop elements
// are permanent and the refresh loop owns their retention.
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

// unionResolver runs authorities concurrently so a slow lookup cannot spend
// another authority's deadline before it starts. Errors identify every failed
// authority and accompany any successful answers; callers must not prune
// using an incomplete union. Successful empty answers (NXDOMAIN) are allowed.
func unionResolver(ctx context.Context, domain string, lookups ...func(context.Context, string) ([]nftables.ResolvedIP, error)) ([]nftables.ResolvedIP, error) {
	type result struct {
		index int
		ips   []nftables.ResolvedIP
		err   error
	}
	results := make(chan result, len(lookups))
	for i, lookup := range lookups {
		safego.Go(func() {
			ips, err := lookup(ctx, domain)
			results <- result{index: i, ips: ips, err: err}
		})
	}
	// Keep the authority order stable for deduplication and diagnostics.
	ordered := make([]result, len(lookups))
	for range lookups {
		r := <-results
		ordered[r.index] = r
	}
	var (
		ips  []nftables.ResolvedIP
		errs []error
	)
	for _, r := range ordered {
		if r.err != nil {
			errs = append(errs, fmt.Errorf("authority %d: %w", r.index+1, r.err))
			continue
		}
		ips = append(ips, r.ips...)
	}
	return unionResolvedIPs(ips), errors.Join(errs...)
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
