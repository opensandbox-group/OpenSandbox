// Copyright 2025 The OpenSandbox Authors
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

	"github.com/alibaba/opensandbox/ingress/pkg/activity"
	"github.com/alibaba/opensandbox/ingress/pkg/flag"
	"github.com/alibaba/opensandbox/ingress/pkg/proxy"
	"github.com/alibaba/opensandbox/ingress/pkg/proxy/connectivity"
	"github.com/alibaba/opensandbox/ingress/pkg/renewintent"
	"github.com/alibaba/opensandbox/ingress/pkg/routescope"
	"github.com/alibaba/opensandbox/ingress/pkg/sandbox"
	"github.com/alibaba/opensandbox/ingress/pkg/signature"
	"github.com/alibaba/opensandbox/ingress/pkg/telemetry"
	"github.com/alibaba/opensandbox/ingress/pkg/wake"
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
	var fsbProvider *sandbox.FastSandboxProvider
	if providerType == sandbox.ProviderTypeFastSandbox {
		fsbProvider, err = sandbox.NewFastSandboxProvider(
			flag.FastPathEndpoint,
			time.Duration(flag.FastPathWaitTimeoutMillis)*time.Millisecond,
			flag.FastPathAccessMode,
		)
		sandboxProvider = fsbProvider
	} else {
		cfg := injection.ParseAndGetRESTConfigOrDie()
		cfg.ContentType = runtime.ContentTypeProtobuf
		cfg.UserAgent = "opensandbox-ingress/" + version.GitCommit
		providerFactory := sandbox.NewProviderFactory(cfg, time.Second*30)
		sandboxProvider, err = providerFactory.CreateProvider(providerType)
		if err == nil && fastPathEnabled {
			fsbProvider, err = sandbox.NewFastSandboxProvider(
				flag.FastPathEndpoint,
				time.Duration(flag.FastPathWaitTimeoutMillis)*time.Millisecond,
				flag.FastPathAccessMode,
			)
			if err == nil {
				sandboxProvider = sandbox.NewCompositeProvider(sandboxProvider, fsbProvider)
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
	proxyOptions := make([]proxy.Option, 0, 3)
	if err != nil {
		log.Printf("network readiness shadow assessment disabled (invalid configuration): %v", err)
	} else {
		proxyOptions = append(proxyOptions, proxy.WithConnectObserver(connectObserver))
	}
	activityRecorder, activityOptions := newActivityRecorder(ctx)
	proxyOptions = append(proxyOptions, activityOptions...)
	proxyOptions = append(proxyOptions, newWakeOption(fsbProvider, activityRecorder)...)
	if flag.WakeEnabled && !flag.ActivityEnabled {
		log.Printf("--wake-enabled without --activity-enabled: completed resumes are not recorded as activity, " +
			"so the server-side idle sweeper will re-pause freshly resumed sandboxes after the idle threshold; " +
			"enable activity recording for a stable wake loop")
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

// newActivityRecorder wires the OSEP-0024 auto-pause activity recorder so
// the server-side idle sweeper sees live traffic. Fire-and-forget; never
// blocks requests. The returned recorder (Noop when disabled) is also the
// wake flights' activity writer — but a Noop writes nothing, so running
// --wake-enabled without --activity-enabled leaves resumed sandboxes
// unrecorded (warned at startup).
func newActivityRecorder(ctx context.Context) (activity.Recorder, []proxy.Option) {
	if !flag.ActivityEnabled {
		return activity.Noop{}, nil
	}
	activityClient, err := activity.RedisClientFromDSN(flag.ActivityRedisDSN)
	if err != nil {
		log.Panicf("Failed to create Redis client for activity: %v", err)
	}
	recorder, err := activity.NewRedisRecorder(ctx, activityClient, activity.RedisConfig{
		TTL:         time.Duration(flag.ActivityTTLSeconds) * time.Second,
		MinInterval: flag.ActivityMinInterval,
		Logger:      proxy.Logger,
	})
	if err != nil {
		log.Panicf("Invalid activity configuration: %v", err)
	}
	return recorder, []proxy.Option{proxy.WithActivityRecorder(recorder)}
}

// newWakeOption wires the OSEP-0024 wake-on-access orchestrator: requests
// routed to paused fast sandboxes park while a resume flight restores them.
func newWakeOption(fsbProvider *sandbox.FastSandboxProvider, activityWriter wake.ActivityWriter) []proxy.Option {
	if !flag.WakeEnabled {
		return nil
	}
	if fsbProvider == nil {
		log.Panic("Wake-on-access requires a Fast Sandbox provider (set --provider-type=fast-sandbox or --fastpath-endpoint)")
	}
	waker, err := wake.NewWaker(wake.Config{
		ParkBudget:    flag.WakeParkBudget,
		ParkMax:       flag.WakeParkMax,
		RetryInterval: flag.WakeRetryInterval,
		RetryFactor:   flag.WakeRetryFactor,
		RetryJitter:   flag.WakeRetryJitter,
	}, fsbProvider, activityWriter)
	if err != nil {
		log.Panicf("Failed to create waker: %v", err)
	}
	if flag.WakeParkBudget <= time.Duration(flag.FastPathWaitTimeoutMillis)*time.Millisecond {
		log.Printf("wake-park-budget (%v) should exceed the FastPath wait timeout (%v) plus one GetSandbox RPC so at least one probe fits inside the budget",
			flag.WakeParkBudget, time.Duration(flag.FastPathWaitTimeoutMillis)*time.Millisecond)
	}
	return []proxy.Option{proxy.WithWaker(waker)}
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
