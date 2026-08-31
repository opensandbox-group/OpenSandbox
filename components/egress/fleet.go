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

// Fleet-profile assembly: a single egress control plane serving N
// sandboxes sharing one host/network domain. Activated by
// OPENSANDBOX_EGRESS_PROFILE=fleet; the sidecar profile is unchanged.
//
// Control flow:
//
//	slot store (file) --(poll/watch)--> subject.Controller --(hooks)-->
//	  fleetPolicyServer: deny-first nft + resolv rewrite, pending-push flush
//	proxy route --(UID header)--> fleetPolicyServer:18080 (loopback)
//	  policy/vault pushes routed per subject
//	DNS: one shared proxy, per-query policy via source IP dispatch
package main

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/dnsproxy"
	"github.com/alibaba/opensandbox/egress/pkg/fleetnft"
	"github.com/alibaba/opensandbox/egress/pkg/iptables"
	"github.com/alibaba/opensandbox/egress/pkg/log"
	"github.com/alibaba/opensandbox/egress/pkg/mitmproxy"
	"github.com/alibaba/opensandbox/egress/pkg/nftables"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/alibaba/opensandbox/egress/pkg/sandboxnft"
	"github.com/alibaba/opensandbox/egress/pkg/slotsource"
	"github.com/alibaba/opensandbox/egress/pkg/subject"
	"github.com/alibaba/opensandbox/egress/pkg/telemetry"
	"github.com/alibaba/opensandbox/internal/safego"
)

// runFleetProfile starts the fleet-profile control plane and blocks until ctx is
// canceled or a fatal error occurs.
func runFleetProfile(ctx context.Context) {
	log.Infof("egress profile: fleet (multi-sandbox control plane)")

	// Erase any stale mitmproxy CA left on the shared volume (root and the
	// fleet mount subdir) by a previous egress generation, so a sandbox's
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

	slotDir := envOrDefault(constants.EnvSlotStoreDir, constants.DefaultSlotStoreDir)
	pollSec := constants.EnvIntOrDefault(constants.EnvSlotPollInterval, constants.DefaultSlotPollIntervalSeconds)
	src := slotsource.NewFileSource(slotDir, time.Duration(pollSec)*time.Second)
	log.Infof("slot store source: %s (poll %ds)", src.Dir(), pollSec)

	alwaysDeny, alwaysAllow, err := policy.LoadAlwaysRuleFiles()
	if err != nil {
		log.Fatalf("failed to load always allow/deny rule files: %v", err)
	}

	podNft := fleetnft.NewApplier(nil, fleetDoHOptions())
	sandboxNft := sandboxnft.NewApplier(nil, sandboxDoHOptions())
	nftMgr := &fleetEnforcer{pod: podNft, sandbox: sandboxNft}
	// Recovery: wipe stale rules from a previous egress generation BEFORE
	// rescanning, so no dead subject's policy survives into a new sandbox.
	if err := podNft.ApplyReset(ctx); err != nil {
		log.Fatalf("fleet nftables reset failed: %v", err)
	}
	log.Infof("fleet nftables table reset (stale rules cleared)")
	// The sandbox layer needs the same wipe: sandbox netns can outlive the
	// egress process, and their OUTPUT tables are the ONLY enforcement for
	// host-local traffic (never seen by the Pod forward hook). Reset every
	// netns the previous generation could have installed into — from the
	// slot store and from the shared netns mount dir.
	wipeSandboxTables(ctx, src, sandboxNft)

	reg := subject.NewRegistry(alwaysDeny, alwaysAllow)
	pendingTTL := time.Duration(constants.EnvIntOrDefault(constants.EnvPendingPushTTL, constants.DefaultPendingPushTTL)) * time.Second
	fleetSrv := newFleetPolicyServer(ctx, reg, nftMgr, pendingTTL)
	controller := subject.NewController(reg, fleetSrv)

	// Shared mitmproxy (OSEP-0022 A1): one mitmdump in the Pod netns serving
	// every sandbox. Started BEFORE the controller so subjects can never
	// register against a missing interceptor (fail-closed registration);
	// the per-subject prerouting DNAT is installed by the fleet server on
	// registration. A disabled MITM skips the whole block.
	mitmGate := mitmproxy.NewHealthGate()
	var fleetMitm *mitmTransparent
	if mitm, err := startFleetMitmproxyIfEnabled(); err != nil {
		log.Fatalf("fleet mitmproxy start failed: %v", err)
	} else if mitm != nil {
		fleetMitm = mitm
		dports, err := constants.BuildMitmproxyPorts(os.Getenv(constants.EnvMitmproxyExtraPorts))
		if err != nil {
			log.Fatalf("fleet mitmproxy ports: %v", err)
		}
		fleetSrv.SetMitm(mitmGate, mitm.port, dports)
		mitm.watchMitmproxy(ctx, mitmGate)
		mitmGate.SetReady(true)
		log.Infof("fleet mitmproxy watch started (shared listener, healthz-gated)")
		startFleetActiveSocket(ctx, fleetSrv)
	} else {
		fleetSrv.SetMitm(nil, 0, nil)
		// MITM disabled: clear any interception table a previous generation
		// (running with MITM enabled) may have left — stale rules would keep
		// DNATing 80/443 to a now-unserved mitmproxy port (blackhole).
		if err := iptables.RemoveMitmRedirects(); err != nil {
			log.Warnf("fleet mitmproxy: stale redirect table cleanup failed (ignored): %v", err)
		}
	}

	// DNS: one shared listener. Bound on :15353 (all interfaces — a
	// prerouting REDIRECT retargets sandbox DNS to the interface address,
	// NOT loopback, so a 127.0.0.1 bind would never receive it; :15353 also
	// never collides with a host DNS service on :53). Per-subject gateway
	// REDIRECTs (fleet server's installGatewayDNSRedirect) forward sandbox
	// DNS addressed to slot.Gateway:53 here; per-query policy is dispatched
	// by source IP.
	dnsAddr := ":15353"
	proxy, err := dnsproxy.New(nil, dnsAddr, alwaysDeny, alwaysAllow)
	if err != nil {
		log.Fatalf("failed to init dns proxy: %v", err)
	}
	proxy.SetQueryPolicySelector(func(remote netip.Addr) *dnsproxy.QueryPolicy {
		s, ok := reg.Resolve(subject.SubjectKey{SourceIP: remote})
		if !ok {
			// Unknown source: deny (fail closed), never a default policy.
			log.Warnf("[dns] query from unknown source %s denied (fail closed)", remote)
			return nil
		}
		eff := reg.EffectivePolicy(s)
		if eff == nil {
			log.Warnf("[dns] query from subject %s (source %s) denied: no effective policy", s, remote)
			return nil
		}
		return &dnsproxy.QueryPolicy{
			Policy: eff,
			OnResolved: func(domain string, ips []nftables.ResolvedIP) {
				addCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				defer cancel()
				if err := nftMgr.AddResolvedIPs(addCtx, s, ips); err != nil {
					log.Warnf("[dns] add resolved IPs to fleet nft failed for subject %s domain %q: %v", s, domain, err)
				}
			},
		}
	})
	if err := proxy.Start(ctx); err != nil {
		log.Fatalf("failed to start dns proxy: %v", err)
	}
	log.Infof("fleet dns proxy listening on %s", dnsAddr)

	httpAddr := envOrDefault(constants.EnvEgressHTTPAddr, constants.DefaultFleetServerAddr)
	srv := &http.Server{Addr: httpAddr, Handler: fleetSrv.Handler()}
	safego.Go(func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("fleet policy server error: %v", err)
		}
	})
	log.Infof("fleet policy server listening on %s (loopback, UID-header routed)", httpAddr)

	fleetSrv.StartPendingSweep(ctx)
	controllerErr := controller.StartWatch(ctx, src)

	// Per-subject connection refresh: active TCP connections keep their
	// dynamic leases alive (bucketed by source IP from the Pod netns
	// conntrack table); the sandbox-netns mirror is refreshed in lockstep.
	nftMgr.StartConnectionRefresh(ctx)
	log.Infof("fleet connection refresh started (bucketed per subject, every 30s)")

	// Block until shutdown or a fatal control-plane failure (slot store
	// unreadable = fail closed: the daemon must exit, not run unenforced).
	select {
	case <-ctx.Done():
	case err := <-controllerErr:
		log.Fatalf("subject controller exited: %v", err)
	}
	log.Infof("received shutdown signal; shutting down fleet profile")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Errorf("fleet policy server shutdown error: %v", err)
	}
	if err := proxy.Shutdown(); err != nil {
		log.Errorf("fleet dns proxy shutdown error: %v", err)
	}
	if fleetMitm != nil {
		mitmproxy.GracefulShutdown(fleetMitm.getRunning(), 3*time.Second)
	}
	// Enforcement is intentionally NOT removed: the kernel rules keep denying
	// while the daemon is down (fail closed); the next start wipes them via
	// ApplyReset before rescannining.
	if err := <-controllerErr; err != nil {
		log.Errorf("subject controller error: %v", err)
	}
	log.Infof("fleet profile shutdown complete")
	_ = os.Stderr.Sync()
}

// fleetDoHOptions parses the shared DoH-443 blocking env for the fleet
// profile: OPENSANDBOX_EGRESS_BLOCK_DOH_443 (strict all-443 drop when the
// blocklist is empty) + OPENSANDBOX_EGRESS_DOH_BLOCKLIST (comma-separated
// IP/CIDR list), same semantics as the sidecar profile. MitmRedirectPort
// enables the Pod-netns INPUT enforcement chain for intercepted (DNATed)
// traffic; 0 when MITM is off.
func fleetDoHOptions() fleetnft.Options {
	opts := fleetnft.Options{BlockDoH443: constants.IsTruthy(os.Getenv(constants.EnvBlockDoH443))}
	if raw := strings.TrimSpace(os.Getenv(constants.EnvDoHBlocklist)); raw != "" {
		opts.DoHBlocklistV4, opts.DoHBlocklistV6 = parseDoHBlocklist(raw)
	}
	if constants.IsTruthy(os.Getenv(constants.EnvMitmproxyTransparent)) {
		opts.MitmRedirectPort = constants.EnvIntOrDefault(constants.EnvMitmproxyPort, constants.DefaultMitmproxyPort)
	}
	return opts
}

// sandboxDoHOptions mirrors fleetDoHOptions for the per-sandbox netns layer,
// so both layers carry identical encrypted-DNS blocking.
func sandboxDoHOptions() sandboxnft.Options {
	opts := sandboxnft.Options{BlockDoH443: constants.IsTruthy(os.Getenv(constants.EnvBlockDoH443))}
	if raw := strings.TrimSpace(os.Getenv(constants.EnvDoHBlocklist)); raw != "" {
		opts.DoHBlocklistV4, opts.DoHBlocklistV6 = parseDoHBlocklist(raw)
	}
	return opts
}

// fleetEnforcer composes the two enforcement layers per subject: the
// authoritative Pod-netns forward hook (pkg/fleetnft) plus the per-sandbox
// netns OUTPUT defense in depth (pkg/sandboxnft). It implements the
// fleetNftApplier surface, so the policy server and the DNS callback stay
// layer-agnostic. Pod first, sandbox second: the authoritative layer is
// always in place before the defense-in-depth layer, and a sandbox-layer
// failure fails the operation (the subject stays denying / on the old
// policy) instead of activating with a gap.
type fleetEnforcer struct {
	pod     *fleetnft.Applier
	sandbox *sandboxnft.Applier
}

var _ fleetNftApplier = (*fleetEnforcer)(nil)

func (e *fleetEnforcer) ApplyDenyFirst(ctx context.Context, s subject.Subject, slot slotsource.Slot) error {
	if err := e.pod.ApplyDenyFirst(ctx, s, slot); err != nil {
		return err
	}
	return e.sandbox.ApplyDenyFirst(ctx, s, slot)
}

func (e *fleetEnforcer) ApplyPolicy(ctx context.Context, s subject.Subject, pol *policy.NetworkPolicy) error {
	if err := e.pod.ApplyPolicy(ctx, s, pol); err != nil {
		return err
	}
	return e.sandbox.ApplyPolicy(ctx, s, pol)
}

// ApplyDispatchUpdate is Pod-netns dispatch plus the sandbox-layer
// reconciliation: an unchanged-fencing slot update that moved the netns path
// or gateway must reinstall the subject's sandbox table (with its current
// policy) so the defense-in-depth layer stays aligned.
func (e *fleetEnforcer) ApplyDispatchUpdate(ctx context.Context, s subject.Subject, slot slotsource.Slot) error {
	if err := e.pod.ApplyDispatchUpdate(ctx, s, slot); err != nil {
		return err
	}
	return e.sandbox.ApplySlotUpdate(ctx, s, slot)
}

func (e *fleetEnforcer) Remove(ctx context.Context, s subject.Subject) error {
	podErr := e.pod.Remove(ctx, s)
	// Best effort: the sandbox rules die with the netns; a gone netns is
	// expected and must never fail the unload.
	_ = e.sandbox.Remove(ctx, s)
	return podErr
}

// AddResolvedIPs mirrors DNS-learned leases into both layers.
func (e *fleetEnforcer) AddResolvedIPs(ctx context.Context, s subject.Subject, ips []nftables.ResolvedIP) error {
	if err := e.pod.AddResolvedIPs(ctx, s, ips); err != nil {
		return err
	}
	return e.sandbox.AddResolvedIPs(ctx, s, ips)
}

// StartConnectionRefresh launches the per-subject refresh loop; the sandbox
// layer is refreshed in lockstep through the mirror callback.
func (e *fleetEnforcer) StartConnectionRefresh(ctx context.Context) {
	e.pod.StartConnectionRefresh(ctx, e.sandbox.AddResolvedIPs)
}

// wipeSandboxTables deletes the sandbox-layer table in every netns the
// previous egress generation could have installed into: the slot store is the
// authoritative list, and the shared netns mount dir covers slots whose files
// are already gone. Best effort — a missing netns or table is expected.
func wipeSandboxTables(ctx context.Context, src slotsource.Source, sandboxNft *sandboxnft.Applier) {
	var paths []string
	if slots, err := src.List(ctx); err == nil {
		for _, slot := range slots {
			paths = append(paths, slot.HostNetnsPath)
		}
	} else {
		log.Warnf("slot store unreadable during recovery (slot-driven sandbox wipe skipped): %v", err)
	}
	paths = append(paths, netnsMountEntries()...)
	sandboxNft.Reset(ctx, paths)
	log.Infof("fleet sandbox tables reset (%d netns path(s))", len(paths))
}

// netnsMountEntries lists the shared netns mount dir (OSEP-0022 deployment
// precondition: /var/run/netns or equivalent).
func netnsMountEntries() []string {
	entries, err := os.ReadDir(constants.DefaultNetnsMountDir)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, filepath.Join(constants.DefaultNetnsMountDir, e.Name()))
	}
	return out
}
