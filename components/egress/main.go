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

package main

import (
	"context"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	_ "github.com/alibaba/opensandbox/egress/hooks"
	_ "github.com/alibaba/opensandbox/internal/safego"
	_ "go.uber.org/automaxprocs/maxprocs"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/dnsproxy"
	"github.com/alibaba/opensandbox/egress/pkg/events"
	"github.com/alibaba/opensandbox/egress/pkg/iptables"
	"github.com/alibaba/opensandbox/egress/pkg/log"
	"github.com/alibaba/opensandbox/egress/pkg/mitmproxy"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/alibaba/opensandbox/egress/pkg/startup"
	"github.com/alibaba/opensandbox/egress/pkg/telemetry"
	slogger "github.com/alibaba/opensandbox/internal/logger"
	"github.com/alibaba/opensandbox/internal/safego"
	"github.com/alibaba/opensandbox/internal/version"
)

func main() {
	version.EchoVersion("OpenSandbox Egress")

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	ctx = withLogger(ctx)
	defer log.Logger.Sync()

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

	initialRules, _, err := policy.LoadInitialPolicyDetailed(os.Getenv(constants.EnvEgressPolicyFile), constants.EnvEgressRules)
	if err != nil {
		log.Fatalf("failed to load initial egress policy: %v", err)
	}
	logEgressLoaded(initialRules)

	alwaysDeny, alwaysAllow, err := policy.LoadAlwaysRuleFiles()
	if err != nil {
		log.Fatalf("failed to load always allow/deny rule files: %v", err)
	}

	allowIPs := allowIps()
	mode := parseMode()
	log.Infof("enforcement mode: %s", mode)
	nftMgr := createNftManager(mode)
	proxy, err := dnsproxy.New(initialRules, "", alwaysDeny, alwaysAllow)
	if err != nil {
		log.Fatalf("failed to init dns proxy: %v", err)
	}
	if err := proxy.Start(ctx); err != nil {
		log.Fatalf("failed to start dns proxy: %v", err)
	}
	log.Infof("dns proxy started on 127.0.0.1:15353")

	if blockWebhookURL := strings.TrimSpace(os.Getenv(constants.EnvBlockedWebhook)); blockWebhookURL != "" {
		blockedBroadcaster := events.NewBroadcaster(ctx, events.BroadcasterConfig{QueueSize: 256})
		blockedBroadcaster.AddSubscriber(events.NewWebhookSubscriber(blockWebhookURL))
		proxy.SetBlockedBroadcaster(blockedBroadcaster)
		defer blockedBroadcaster.Close()
		log.Infof("denied hostname webhook enabled")
	}

	exemptDst := dnsproxy.ParseNameserverExemptList()
	if len(exemptDst) > 0 {
		log.Infof("nameserver exempt list: %v (proxy upstream in this list will not set SO_MARK)", exemptDst)
	}
	if err := iptables.SetupRedirect(15353, exemptDst); err != nil {
		log.Fatalf("failed to install iptables redirect: %v", err)
	}
	log.Infof("iptables redirect configured (OUTPUT 53 -> 15353) with SO_MARK bypass for proxy upstream traffic")

	setupNft(ctx, nftMgr, initialRules, proxy, allowIPs, alwaysDeny, alwaysAllow)

	httpAddr := envOrDefault(constants.EnvEgressHTTPAddr, constants.DefaultEgressServerAddr)
	mitmGate := mitmproxy.NewHealthGate()
	policySrv, err := startPolicyServer(proxy, nftMgr, mode, httpAddr, os.Getenv(constants.EnvEgressToken), allowIPs, os.Getenv(constants.EnvEgressPolicyFile), alwaysDeny, alwaysAllow, mitmGate)
	if err != nil {
		log.Fatalf("failed to start policy server: %v", err)
	}
	log.Infof("policy server listening on %s (POST /policy)", httpAddr)

	mitm, err := startMitmproxyTransparentIfEnabled()
	if err != nil {
		log.Fatalf("mitmproxy transparent: %v", err)
	}
	mitmGate.MarkStackReady()
	if mitm != nil {
		mitm.watchMitmproxy(ctx, mitmGate)
	}

	if err := startup.RunPost(ctx); err != nil {
		log.Errorf("startup hooks (post) error: %v", err)
	}

	waitForShutdown(ctx, proxy, policySrv, exemptDst, nftMgr, mitm)
}

func withLogger(ctx context.Context) context.Context {
	level := envOrDefault(constants.EnvEgressLogLevel, "info")
	cfg := slogger.Config{Level: level}
	base := slogger.MustNew(cfg)
	// Baseline log fields (e.g. sandbox_id, OPENSANDBOX_EGRESS_METRICS_EXTRA_ATTRS) for every line.
	if extra := telemetry.EgressLogFields(); len(extra) > 0 {
		base = base.With(extra...)
	}
	logger := base.Named("opensandbox.egress")
	safego.InitPanicLogger(ctx, logger)
	return log.WithLogger(ctx, logger)
}

func envOrDefault(key, defaultVal string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return defaultVal
}

func containsAddr(addrs []netip.Addr, a netip.Addr) bool {
	for _, x := range addrs {
		if x == a {
			return true
		}
	}
	return false
}

func parseMode() string {
	raw := os.Getenv(constants.EnvEgressMode)
	normalized, err := constants.ParseEgressMode(raw)
	if err != nil {
		log.Warnf("invalid %s=%q: %v; falling back to %s", constants.EnvEgressMode, raw, err, constants.PolicyDnsOnly)
		return constants.PolicyDnsOnly
	}
	return normalized
}
