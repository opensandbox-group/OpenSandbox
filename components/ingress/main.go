// Copyright 2025 Alibaba Group Holding Ltd.
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
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	slogger "github.com/alibaba/opensandbox/internal/logger"
	"github.com/alibaba/opensandbox/internal/version"
	"k8s.io/apimachinery/pkg/runtime"
	"knative.dev/pkg/injection"
	"knative.dev/pkg/signals"

	"github.com/alibaba/opensandbox/ingress/pkg/flag"
	"github.com/alibaba/opensandbox/ingress/pkg/proxy"
	"github.com/alibaba/opensandbox/ingress/pkg/proxy/connectivity"
	"github.com/alibaba/opensandbox/ingress/pkg/renewintent"
	"github.com/alibaba/opensandbox/ingress/pkg/routescope"
	"github.com/alibaba/opensandbox/ingress/pkg/sandbox"
	"github.com/alibaba/opensandbox/ingress/pkg/signature"
	"github.com/alibaba/opensandbox/ingress/pkg/telemetry"
)

func main() {
	version.EchoVersion("OpenSandbox Ingress")

	flag.InitFlags()

	ctx := signals.NewContext()
	ctx = withLogger(ctx, flag.LogLevel)
	providerType := sandbox.ProviderType(flag.ProviderType)

	otelShutdown, err := telemetry.Init(ctx)
	if err != nil {
		log.Printf("OpenTelemetry metrics disabled (continuing without OTLP): %v", err)
		otelShutdown = nil
	}
	if otelShutdown != nil {
		defer func() {
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer shutdownCancel()
			_ = otelShutdown(shutdownCtx)
		}()
	}

	var secure *signature.Verifier
	var scopeVerifier *routescope.Verifier
	fastPathEnabled := strings.TrimSpace(flag.FastPathEndpoint) != ""
	if keyStr := strings.TrimSpace(flag.SecureAccessKeys); keyStr != "" {
		keys, parseErr := signature.ParseKeys(keyStr)
		if parseErr != nil {
			log.Panicf("parse secure-access-keys: %v", parseErr)
		}
		secure = &signature.Verifier{Keys: keys}
		if fastPathEnabled {
			scopeVerifier = &routescope.Verifier{Keys: keys}
		}
	}
	if fastPathEnabled && scopeVerifier == nil {
		log.Panic("FastPath routing requires --secure-access-keys for authenticated route scopes")
	}

	var sandboxProvider sandbox.Provider
	if providerType == sandbox.ProviderTypeFleets {
		sandboxProvider, err = sandbox.NewFleetsProvider(
			flag.FastPathEndpoint,
			time.Duration(flag.FastPathWaitTimeoutMillis)*time.Millisecond,
			flag.FastPathAccessMode,
		)
	} else {
		cfg := injection.ParseAndGetRESTConfigOrDie()
		cfg.ContentType = runtime.ContentTypeProtobuf
		cfg.UserAgent = "opensandbox-ingress/" + version.GitCommit
		providerFactory := sandbox.NewProviderFactory(cfg, time.Second*30)
		sandboxProvider, err = providerFactory.CreateProvider(providerType)
		if err == nil && fastPathEnabled {
			var fleetsProvider *sandbox.FleetsProvider
			fleetsProvider, err = sandbox.NewFleetsProvider(
				flag.FastPathEndpoint,
				time.Duration(flag.FastPathWaitTimeoutMillis)*time.Millisecond,
				flag.FastPathAccessMode,
			)
			if err == nil {
				sandboxProvider = sandbox.NewCompositeProvider(sandboxProvider, fleetsProvider)
			}
		}
	}
	if err != nil {
		log.Panicf("Failed to create sandbox provider: %v", err)
	}

	// Start provider (includes cache sync)
	if err := sandboxProvider.Start(ctx); err != nil {
		log.Panicf("Failed to start sandbox provider: %v", err)
	}

	var renewPublisher renewintent.Publisher
	if flag.RenewIntentEnabled {
		redisClient, err := renewintent.RedisClientFromDSN(flag.RenewIntentRedisDSN)
		if err != nil {
			log.Panicf("Failed to create Redis client for renew-intent: %v", err)
		}
		renewPublisher = renewintent.NewRedisPublisher(ctx, redisClient, renewintent.RedisPublisherConfig{
			QueueKey:    flag.RenewIntentQueueKey,
			QueueMaxLen: flag.RenewIntentQueueMaxLen,
			MinInterval: time.Duration(flag.RenewIntentMinIntervalSec) * time.Second,
			Logger:      proxy.Logger,
		})
	}

	connectObserver, networkReadiness, err := newNetworkReadiness(connectivity.TrackerConfig{
		Window:                   flag.NetworkReadinessShadowWindow,
		MaxDistinctTargets:       flag.NetworkReadinessShadowMaxTargets,
		MinAttempts:              flag.NetworkReadinessShadowMinAttempts,
		MinDistinctTargets:       flag.NetworkReadinessShadowMinTargets,
		MinDistinctSignalTargets: flag.NetworkReadinessShadowMinSignalTargets,
		DegradedFailureRatio:     flag.NetworkReadinessShadowDegradedFailureRatio,
	})
	proxyOptions := make([]proxy.Option, 0, 1)
	if err != nil {
		log.Printf("network readiness shadow assessment disabled (invalid configuration): %v", err)
	} else {
		proxyOptions = append(proxyOptions, proxy.WithConnectObserver(connectObserver))
	}

	// Create reverse proxy with sandbox provider.
	reverseProxy := proxy.NewProxy(
		ctx,
		sandboxProvider,
		proxy.Mode(flag.Mode),
		renewPublisher,
		secure,
		scopeVerifier,
		proxyOptions...,
	)
	mux := newIngressMux(reverseProxy, networkReadiness)

	if err := http.ListenAndServe(fmt.Sprintf(":%v", flag.Port), mux); err != nil {
		log.Panicf("Error starting http server: %v", err)
	}
}

func newNetworkReadiness(config connectivity.TrackerConfig) (connectivity.Observer, http.Handler, error) {
	tracker, err := connectivity.NewTracker(config)
	if err != nil {
		telemetry.SetConnectivitySnapshotProvider(nil)
		return nil, http.NotFoundHandler(), err
	}

	observer := connectivity.ObserverFunc(func(observation connectivity.Observation) {
		tracker.Observe(observation)
		telemetry.RecordUpstreamConnect(
			string(observation.Result),
			observation.Protocol,
			float64(observation.Duration)/float64(time.Millisecond),
		)
	})
	telemetry.SetConnectivitySnapshotProvider(func() telemetry.ConnectivitySnapshot {
		snapshot := tracker.Snapshot(time.Now())
		return telemetry.ConnectivitySnapshot{
			Attempts:              int64(snapshot.Attempts),
			SignalFailures:        int64(snapshot.SignalFailures),
			DistinctTargets:       int64(snapshot.DistinctTargets),
			DistinctSignalTargets: int64(snapshot.DistinctSignalTargets),
			Qualified:             snapshot.Qualified,
			Degraded:              snapshot.Degraded,
		}
	})
	return observer, connectivity.NewReadinessHandler(tracker), nil
}

func newIngressMux(reverseProxy, networkReadiness http.Handler) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/", reverseProxy)
	mux.Handle("/status.ok/network-readiness", networkReadiness)
	mux.HandleFunc("/status.ok", proxy.Healthz)
	return mux
}

func withLogger(ctx context.Context, logLevel string) context.Context {
	logger := slogger.MustNew(slogger.Config{Level: logLevel}).Named("opensandbox.ingress")
	return proxy.WithLogger(ctx, logger)
}
