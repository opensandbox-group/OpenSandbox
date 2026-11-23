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

package main

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/dnsproxy"
	"github.com/alibaba/opensandbox/egress/pkg/forwarder"
	"github.com/alibaba/opensandbox/egress/pkg/iptables"
	"github.com/alibaba/opensandbox/egress/pkg/log"
	"github.com/alibaba/opensandbox/egress/pkg/mitmproxy"
	"github.com/alibaba/opensandbox/egress/pkg/nftables"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/alibaba/opensandbox/egress/pkg/startup"
	"github.com/alibaba/opensandbox/egress/pkg/telemetry"
	"github.com/alibaba/opensandbox/internal/safego"
	inttelemetry "github.com/alibaba/opensandbox/internal/telemetry"
)

// forwardMode is the validated OSEP-0030 forward-mode configuration.
type forwardMode struct {
	gateway      mitmproxy.UpstreamProxySpec
	fwd          *forwarder.Forwarder
	refusedFinal bool
}

// forwardModeFromEnv validates forward mode before any network setup. Every
// missing or conflicting setting is an error: forward mode never degrades to
// dns-only or direct egress.
func forwardModeFromEnv(profile string) (*forwardMode, error) {
	if profile == constants.ProfileFastSandbox {
		return nil, fmt.Errorf("%s=%s is not yet supported in the %s profile", constants.EnvEgressMode, constants.PolicyForward, constants.ProfileFastSandbox)
	}
	for _, key := range []string{constants.EnvMitmproxyTransparent, constants.EnvMitmproxySslInsecure, constants.EnvExperimentalRevisionRuntime} {
		if constants.IsTruthy(os.Getenv(key)) {
			return nil, fmt.Errorf("%s is incompatible with %s=%s", key, constants.EnvEgressMode, constants.PolicyForward)
		}
	}
	for _, key := range []string{constants.EnvMitmproxyPassthroughHosts, constants.EnvNameserverExempt, constants.EnvEgressRules} {
		if _, present := os.LookupEnv(key); present {
			return nil, fmt.Errorf("%s is incompatible with %s=%s", key, constants.EnvEgressMode, constants.PolicyForward)
		}
	}
	spec, err := mitmproxy.ForwardUpstreamProxyFromEnv()
	if err != nil {
		return nil, err
	}
	if spec == nil {
		return nil, fmt.Errorf("%s=%s requires %s", constants.EnvEgressMode, constants.PolicyForward, constants.EnvUpstreamProxy)
	}
	rootCAs, err := gatewayRootCAs(strings.TrimSpace(os.Getenv(constants.EnvMitmproxyUpstreamExtraCA)))
	if err != nil {
		return nil, err
	}
	fwd, err := forwarder.New(forwarder.Config{
		Scheme:        spec.Scheme,
		Host:          spec.Host,
		Port:          spec.Port,
		Authorization: os.Getenv(constants.EnvUpstreamProxyAuth),
		RootCAs:       rootCAs,
		Identity: forwarder.Identity{
			SandboxID:  strings.TrimSpace(os.Getenv(constants.EnvSandboxID)),
			Namespace:  strings.TrimSpace(os.Getenv(constants.EnvSandboxNamespace)),
			TenantName: strings.TrimSpace(os.Getenv(constants.EnvTenantName)),
		},
		Mark: constants.ForwarderMarkValue,
	})
	if err != nil {
		return nil, fmt.Errorf("forwarder: %w", err)
	}
	return &forwardMode{
		gateway: *spec,
		fwd:     fwd,
		// An operator-configured resolver is the policy DNS (R7): its
		// REFUSED is final. Without it, DNS behaves as in dns mode.
		refusedFinal: strings.TrimSpace(os.Getenv(constants.EnvDNSUpstream)) != "",
	}, nil
}

// gatewayRootCAs returns the system pool plus the optional extra CA bundle
// the server mounts for the gateway listener (same env as the MITM chain).
func gatewayRootCAs(extraCA string) (*x509.CertPool, error) {
	if extraCA == "" {
		return nil, nil // nil = system roots
	}
	pem, err := os.ReadFile(extraCA)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", constants.EnvMitmproxyUpstreamExtraCA, err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("%s: no PEM certificates found", constants.EnvMitmproxyUpstreamExtraCA)
	}
	return pool, nil
}

// forwardTelemetryEndpoint resolves the OTLP collector destination (using the
// same env precedence as telemetryAllowRules) into a mark-scoped nftables
// endpoint and, for hostname endpoints, the domain to register as an infra
// DNS exemption. Returns (nil, "") when metrics are disabled or unconfigured.
func forwardTelemetryEndpoint() (*nftables.UpstreamProxyEndpoint, string) {
	if inttelemetry.MetricsDisabled() {
		return nil, ""
	}
	host, portStr, ok := inttelemetry.OTLPEndpointHostPort()
	if !ok {
		if inttelemetry.OTLPEndpointEnvSet() {
			log.Warnf("forward mode: configured OTLP endpoint is not a valid URL; skipping telemetry nft allow")
			return nil, ""
		}
		host, portStr, ok = inttelemetry.OTLPEndpointFallbackHostPort()
	}
	if !ok {
		return nil, ""
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		log.Warnf("forward mode: invalid OTLP endpoint port %q for host %q; skipping telemetry nft allow", portStr, host)
		return nil, ""
	}
	ep := &nftables.UpstreamProxyEndpoint{Port: port, Mark: constants.TelemetryMarkValue}
	if ip, err := netip.ParseAddr(host); err == nil {
		ep.IPs = []netip.Addr{ip.Unmap()}
		return ep, ""
	}
	return ep, host
}

const (
	// Immutable resolver snapshots captured by Kubernetes egress-init before any
	// workload container starts, mounted read-only into egress only. On Docker
	// these paths do not exist and forward mode falls back to /etc/hosts and
	// /etc/resolv.conf (which are not shared with the workload container).
	forwardSnapshotHostsPath  = "/run/opensandbox-egress/hosts"
	forwardSnapshotResolvPath = "/run/opensandbox-egress/resolv.conf"
)

func preferSnapshotFile(snapshot, fallback string) string {
	if info, err := os.Stat(snapshot); err == nil && !info.IsDir() {
		return snapshot
	}
	return fallback
}

// buildForwardEndpoint builds the mark-scoped nftables endpoint for host:port.
// Literal IPs populate IPs directly (isLiteral=true). Hostnames seed any
// static hosts-file addresses (e.g. Kubernetes spec.hostAliases captured in the
// init snapshot) so the forwarder resolves them from memory without reading the
// Pod's workload-writable /etc/hosts, while returning isLiteral=false so
// dnsproxy also registers the hostname for DNS answers.
func buildForwardEndpoint(host string, port int, mark uint32, hostsPath string) (*nftables.UpstreamProxyEndpoint, bool) {
	ep := &nftables.UpstreamProxyEndpoint{Port: port, Mark: mark}
	if ip, err := netip.ParseAddr(host); err == nil {
		ep.IPs = []netip.Addr{ip.Unmap()}
		return ep, true
	}
	ep.IPs = dnsproxy.HostsFileIPs(host, hostsPath)
	return ep, false
}

// registerForwardInfraDomain registers host and every resolvPath
// search-expanded candidate (e.g. Kubernetes ClusterFirst "gateway" →
// "gateway.<ns>.svc.cluster.local") so dnsproxy populates the mark-scoped nft
// allow set regardless of which candidate name CoreDNS answers.
func registerForwardInfraDomain(proxy *dnsproxy.Proxy, host, resolvPath string, onResolved func(string, []nftables.ResolvedIP)) []string {
	names := dnsproxy.ExpandInfraDomainNames(host, resolvPath)
	for _, name := range names {
		proxy.SetInfraDomain(name, onResolved)
	}
	return names
}

// newForwardHostResolver returns a host resolver for forwarder.SetResolveHost
// that never reads /etc/hosts or /etc/resolv.conf at runtime. Static snapshot
// IPs (from Kubernetes hostAliases) are returned directly from memory;
// otherwise each pre-expanded candidate name is queried against the local
// dnsproxy (127.0.0.1:15353), which forwards to the immutable snapshot
// nameservers and updates the mark-scoped nft sets before replying.
func newForwardHostResolver(staticIPs []netip.Addr, dnsAddr string, queryNames []string) func(context.Context, string) ([]netip.Addr, error) {
	if len(staticIPs) > 0 {
		pinned := append([]netip.Addr(nil), staticIPs...)
		return func(context.Context, string) ([]netip.Addr, error) {
			return pinned, nil
		}
	}
	candidates := append([]string(nil), queryNames...)
	return func(ctx context.Context, host string) ([]netip.Addr, error) {
		var lastErr error
		for _, name := range candidates {
			ips, err := queryDNSBothFamilies(ctx, dnsAddr, name)
			if err != nil {
				lastErr = err
			}
			if len(ips) > 0 {
				return ips, nil
			}
			if ctx.Err() != nil {
				break
			}
		}
		if lastErr != nil {
			return nil, fmt.Errorf("resolve %q via %s: %w", host, dnsAddr, lastErr)
		}
		return nil, fmt.Errorf("resolve %q via %s: no addresses found", host, dnsAddr)
	}
}

func queryDNSBothFamilies(ctx context.Context, dnsAddr, name string) ([]netip.Addr, error) {
	qCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()

	var (
		ips4, ips6 []netip.Addr
		err4, err6 error
		wg         sync.WaitGroup
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		ips4, err4 = queryDNSAddr(qCtx, dnsAddr, name, dns.TypeA)
	}()
	go func() {
		defer wg.Done()
		ips6, err6 = queryDNSAddr(qCtx, dnsAddr, name, dns.TypeAAAA)
	}()
	wg.Wait()

	if len(ips4) > 0 || len(ips6) > 0 {
		out := make([]netip.Addr, 0, len(ips4)+len(ips6))
		out = append(out, ips4...)
		out = append(out, ips6...)
		return out, nil
	}
	if err4 != nil {
		return nil, err4
	}
	return nil, err6
}

func queryDNSAddr(ctx context.Context, dnsAddr, name string, qtype uint16) ([]netip.Addr, error) {
	msg := new(dns.Msg)
	msg.SetQuestion(dns.Fqdn(name), qtype)
	msg.RecursionDesired = true

	udpClient := &dns.Client{Net: "udp", Timeout: time.Second}
	resp, _, err := udpClient.ExchangeContext(ctx, msg, dnsAddr)
	if err != nil {
		return nil, err
	}
	if resp.Truncated {
		tcpClient := &dns.Client{Net: "tcp", Timeout: time.Second}
		resp, _, err = tcpClient.ExchangeContext(ctx, msg, dnsAddr)
		if err != nil {
			return nil, err
		}
	}
	var out []netip.Addr
	for _, rr := range resp.Answer {
		switch a := rr.(type) {
		case *dns.A:
			if ip, ok := netip.AddrFromSlice(a.A); ok {
				out = append(out, ip.Unmap())
			}
		case *dns.AAAA:
			if ip, ok := netip.AddrFromSlice(a.AAAA); ok {
				out = append(out, ip.Unmap())
			}
		}
	}
	return out, nil
}

// runForwardSidecar installs the forward-mode data plane in order: default
// deny filter, DNS proxy and redirect, forwarder, TCP capture, and only then
// /healthz, so the server never starts the workload before capture is active
// and the gateway is reachable.
// Enforcement is retained on shutdown (fail closed).
func runForwardSidecar(ctx context.Context, fm *forwardMode) {
	log.Infof("enforcement mode: %s (gateway %s://%s:%d)", constants.PolicyForward, fm.gateway.Scheme, fm.gateway.Host, fm.gateway.Port)

	hostsPath := preferSnapshotFile(forwardSnapshotHostsPath, "/etc/hosts")
	resolvPath := preferSnapshotFile(forwardSnapshotResolvPath, "/etc/resolv.conf")

	telemetryEP, telemetryHost := forwardTelemetryEndpoint()
	if telemetryEP != nil && len(telemetryEP.IPs) == 0 && telemetryHost != "" {
		telemetryEP.IPs = dnsproxy.HostsFileIPs(telemetryHost, hostsPath)
	}
	otelShutdown, err := telemetry.InitWithDialControl(ctx, forwarder.MarkControl(constants.TelemetryMarkValue))
	if err != nil {
		log.Warnf("OpenTelemetry metrics disabled (continuing without OTLP): %v", err)
		otelShutdown = nil
		telemetryEP = nil
		telemetryHost = ""
	}
	if otelShutdown != nil {
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = otelShutdown(shutdownCtx)
		}()
	}

	ep, gatewayIsLiteral := buildForwardEndpoint(fm.gateway.Host, fm.gateway.Port, constants.ForwarderMarkValue, hostsPath)
	nftMgr := nftables.NewManagerWithOptions(nftables.Options{
		UpstreamProxy:       ep,
		Telemetry:           telemetryEP,
		ForwardRedirectPort: constants.DefaultForwarderPort,
	})
	if err := nftMgr.ApplyStatic(ctx, policy.DefaultDenyPolicy()); err != nil {
		log.Fatalf("forward mode: nftables apply failed: %v", err)
	}

	// No local domain policy: every query is forwarded (R7).
	allowAll, err := policy.ParsePolicy(`{"defaultAction":"allow"}`)
	if err != nil {
		log.Fatalf("forward mode: dns policy: %v", err)
	}
	proxy, err := dnsproxy.NewWithResolvPath(allowAll, "", nil, nil, resolvPath)
	if err != nil {
		log.Fatalf("failed to init dns proxy: %v", err)
	}
	proxy.SetRefusedFinal(fm.refusedFinal)
	if !gatewayIsLiteral {
		gatewayNames := registerForwardInfraDomain(proxy, fm.gateway.Host, resolvPath, func(domain string, ips []nftables.ResolvedIP) {
			addCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := nftMgr.AddUpstreamProxyIPs(addCtx, ips); err != nil && !errors.Is(err, nftables.ErrQuiesced) {
				log.Warnf("forward mode: gateway nft update for %q failed: %v", domain, err)
			}
		})
		fm.fwd.SetResolveHost(newForwardHostResolver(ep.IPs, proxy.ListenAddr(), gatewayNames))
	}
	if telemetryEP != nil && telemetryHost != "" {
		registerForwardInfraDomain(proxy, telemetryHost, resolvPath, func(domain string, ips []nftables.ResolvedIP) {
			addCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := nftMgr.AddTelemetryIPs(addCtx, ips); err != nil && !errors.Is(err, nftables.ErrQuiesced) {
				log.Warnf("forward mode: telemetry nft update for %q failed: %v", domain, err)
			}
		})
	}
	if err := proxy.Start(ctx); err != nil {
		log.Fatalf("failed to start dns proxy: %v", err)
	}
	if err := iptables.SetupRedirect(15353, nil); err != nil {
		log.Fatalf("failed to install DNS redirect: %v", err)
	}

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", constants.DefaultForwarderPort))
	if err != nil {
		log.Fatalf("forward mode: listen: %v", err)
	}
	safego.Go(func() {
		if err := fm.fwd.Serve(ln); err != nil {
			log.Errorf("forward mode: forwarder stopped: %v", err)
		}
	})
	if err := iptables.SetupForwardRedirect(constants.DefaultForwarderPort); err != nil {
		log.Fatalf("forward mode: %v", err)
	}

	health := &gatewayHealth{}
	safego.Go(func() { health.run(ctx, fm.fwd.ProbeGateway) })

	httpAddr := envOrDefault(constants.EnvEgressHTTPAddr, constants.DefaultEgressServerAddr)
	srv := &http.Server{Addr: httpAddr, Handler: forwardHTTPHandler(health.err), ReadHeaderTimeout: 10 * time.Second}
	safego.Go(func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("forward mode: http server: %v", err)
		}
	})
	log.Infof("forward mode ready: http on %s, forwarder on 127.0.0.1:%d", httpAddr, constants.DefaultForwarderPort)

	if err := startup.RunPost(ctx); err != nil {
		log.Errorf("startup hooks (post) error: %v", err)
	}

	<-ctx.Done()
	log.Infof("received shutdown signal; retaining forward-mode enforcement")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), defaultPolicyShutdownTimeout)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	_ = ln.Close()
	if err := proxy.Shutdown(); err != nil {
		log.Errorf("dns proxy shutdown error: %v", err)
	}
	_ = os.Stderr.Sync()
}

const (
	gatewayProbeTimeout         = 3 * time.Second
	gatewayProbeUnhealthyPeriod = time.Second
	gatewayProbeHealthyPeriod   = 5 * time.Second
)

var errGatewayNotProbed = errors.New("gateway not probed yet")

// gatewayHealth tracks gateway reachability from a background prober, so
// /healthz never blocks on the network. It starts unhealthy: the server
// must not start a workload whose every connection would fail.
type gatewayHealth struct {
	mu      sync.RWMutex
	lastErr error
	probed  bool
}

func (h *gatewayHealth) err() error {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if !h.probed {
		return errGatewayNotProbed
	}
	return h.lastErr
}

func (h *gatewayHealth) record(err error) {
	h.mu.Lock()
	changed := !h.probed || (h.lastErr == nil) != (err == nil)
	h.lastErr, h.probed = err, true
	h.mu.Unlock()
	if !changed {
		return
	}
	if err != nil {
		log.Warnf("forward mode: gateway unreachable: %v", err)
	} else {
		log.Infof("forward mode: gateway reachable")
	}
}

func (h *gatewayHealth) run(ctx context.Context, probe func(context.Context) error) {
	for {
		probeCtx, cancel := context.WithTimeout(ctx, gatewayProbeTimeout)
		err := probe(probeCtx)
		cancel()
		if ctx.Err() != nil {
			return
		}
		h.record(err)
		period := gatewayProbeHealthyPeriod
		if err != nil {
			period = gatewayProbeUnhealthyPeriod
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(period):
		}
	}
}

// forwardHTTPHandler serves /healthz and refuses the policy API: forward
// mode has no local policy, the gateway is the only policy authority.
// /healthz is 503 while the gateway is unreachable (OSEP-0030 observability).
func forwardHTTPHandler(gatewayErr func() error) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := gatewayErr(); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"unavailable","egressMode":"forward","gateway":"unreachable"}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"ok","egressMode":"forward","gateway":"reachable"}`))
	})
	mux.HandleFunc("/policy", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "policy API is disabled in egress forward mode", http.StatusConflict)
	})
	return mux
}
