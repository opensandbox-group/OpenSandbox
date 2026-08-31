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
	"errors"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/alibaba/opensandbox/internal/logger"
	"github.com/alibaba/opensandbox/internal/safego"
	sharedtelemetry "github.com/alibaba/opensandbox/internal/telemetry"
	"github.com/alibaba/opensandbox/internal/version"
	"github.com/alibaba/opensandbox/nodeagent/pkg/api"
	"github.com/alibaba/opensandbox/nodeagent/pkg/config"
	"github.com/alibaba/opensandbox/nodeagent/pkg/pipeline"
	"github.com/alibaba/opensandbox/nodeagent/pkg/registry"
	"github.com/alibaba/opensandbox/nodeagent/pkg/resourcecheck"
	healthserver "github.com/alibaba/opensandbox/nodeagent/pkg/server"
	_ "github.com/alibaba/opensandbox/nodeagent/pkg/sink/file"
	_ "github.com/alibaba/opensandbox/nodeagent/pkg/sink/oss"
	sourcegroup "github.com/alibaba/opensandbox/nodeagent/pkg/source"
	_ "github.com/alibaba/opensandbox/nodeagent/pkg/source/containerlogs"
	"github.com/alibaba/opensandbox/nodeagent/pkg/state"
	"github.com/alibaba/opensandbox/nodeagent/pkg/store"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func main() {
	os.Exit(run())
}

func run() (exitCode int) {
	version.EchoVersion("OpenSandbox Node Agent")
	log := logger.MustNew(logger.Config{Level: os.Getenv("NODEAGENT_LOG_LEVEL")}).Named("opensandbox.nodeagent")
	defer func() { _ = log.Sync() }()
	safego.InitPanicLogger(context.Background(), log)

	cfg, cfgErr := config.Load()
	readiness := healthserver.NewReadiness()
	healthAddr, pprofAddr := serverAddresses(cfg, cfgErr)
	servers := healthserver.Start(healthAddr, pprofAddr, readiness, func(err error) {
		readiness.Set("server-unavailable", true)
		log.Errorf("server unavailable: %v", err)
	})
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = servers.Shutdown(ctx)
	}()
	if cfgErr != nil {
		readiness.Replace("invalid-config")
		log.Errorf("configuration invalid: %v", cfgErr)
		waitForSignal()
		return
	}

	signalCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()
	runCtx, cancelRun := context.WithCancel(signalCtx)
	defer cancelRun()
	telemetryShutdown, err := sharedtelemetry.Init(runCtx, sharedtelemetry.Config{ServiceName: "opensandbox-nodeagent", DisableEndpointFallback: true})
	if err != nil {
		log.Warnf("OpenTelemetry metrics disabled: %v", err)
	} else {
		defer func() {
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer shutdownCancel()
			_ = telemetryShutdown(shutdownCtx)
		}()
	}

	targetID, err := registry.TargetID(cfg.Sink, cfg)
	if err != nil {
		readiness.Replace("invalid-target")
		log.Errorf("target identity invalid: %v", err)
		<-signalCtx.Done()
		return
	}
	checkpoint, err := state.Open(cfg.StateDir, targetID, cfg.StateMaxBytes)
	if err != nil {
		readiness.Replace("state-unavailable")
		log.Errorf("state unavailable: %v", err)
		<-signalCtx.Done()
		return
	}
	defer func() {
		if err := checkpoint.Close(); err != nil {
			log.Errorf("close checkpoint: %v", err)
			exitCode = 1
		}
	}()
	if err := checkpoint.ValidateEnabledSources(cfg.Sources); err != nil {
		readiness.Replace("source-state-conflict")
		log.Errorf("Source configuration conflicts with recovery state: %v", err)
		<-signalCtx.Done()
		return
	}

	sink, err := registry.BuildSink(cfg.Sink, registry.SinkDependencies{Config: cfg, State: checkpoint})
	if err != nil {
		readiness.Replace("sink-unavailable")
		log.Errorf("sink unavailable: %v", err)
		<-signalCtx.Done()
		return
	}
	if err := resourcecheck.Validate(cfg); err != nil {
		readiness.Replace("host-reserve-unavailable")
		log.Errorf("host resource reserve unavailable: %v", err)
		<-signalCtx.Done()
		return
	}

	restConfig, err := rest.InClusterConfig()
	if err != nil {
		readiness.Replace("kubernetes-config-unavailable")
		log.Errorf("Kubernetes configuration unavailable: %v", err)
		<-signalCtx.Done()
		return
	}
	restConfig.UserAgent = "opensandbox-nodeagent/" + version.GitCommit
	client, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		readiness.Replace("kubernetes-client-unavailable")
		log.Errorf("Kubernetes client unavailable: %v", err)
		<-signalCtx.Done()
		return
	}
	identityStore := store.New(client, cfg.NodeName, cfg.ClusterID)
	if err := identityStore.Start(runCtx); err != nil {
		readiness.Replace("store-not-synced")
		log.Errorf("Sandbox Store failed to sync: %v", err)
		<-signalCtx.Done()
		return
	}
	staleThreshold := max(3*config.InternalReconcileInterval, time.Minute)
	safego.Go(func() {
		ticker := time.NewTicker(min(config.InternalReconcileInterval, time.Minute))
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case now := <-ticker.C:
				readiness.Set("store-stale", identityStore.Stale(now, staleThreshold))
			}
		}
	})

	onRuntimeError := func(err error) {
		readiness.Set("runtime-error", true)
		log.Errorf("runtime error: %v", err)
	}
	sourceChildren := make([]sourcegroup.Child, 0, len(cfg.Sources))
	for _, sourceName := range cfg.Sources {
		sourceStore, viewErr := identityStore.ForSource(sourceName)
		if viewErr != nil {
			readiness.Replace("source-unavailable")
			log.Errorf("source Store view failed: %v", viewErr)
			<-signalCtx.Done()
			return
		}
		sourceState, stateErr := checkpoint.SourceState(sourceName)
		if stateErr != nil {
			readiness.Replace("source-unavailable")
			log.Errorf("source state namespace invalid: %v", stateErr)
			<-signalCtx.Done()
			return
		}
		implementation, buildErr := registry.BuildSource(sourceName, registry.SourceDependencies{Config: cfg, Store: sourceStore, State: sourceState, Logger: log, OnError: onRuntimeError})
		if buildErr != nil {
			readiness.Replace("source-unavailable")
			log.Errorf("source factory failed: %v", buildErr)
			<-signalCtx.Done()
			return
		}
		sourceChildren = append(sourceChildren, sourcegroup.Child{Name: sourceName, Source: implementation})
	}
	sources, err := sourcegroup.NewMux(sourceChildren, onRuntimeError)
	if err != nil {
		readiness.Replace("source-unavailable")
		log.Errorf("source group invalid: %v", err)
		<-signalCtx.Done()
		return
	}
	p, err := pipeline.New(pipeline.Config{BatchMaxItems: config.InternalBatchMaxItems, FlushInterval: config.InternalBatchFlushInterval, SinkTimeout: cfg.SinkTimeout, RetryMaxInterval: cfg.RetryMaxInterval, OnRetryStateChange: func(active bool) { readiness.Set("operation-retrying", active) }, MemoryBudgetBytes: cfg.MemoryBudgetBytes, PerSandboxQueueBytes: cfg.PerSandboxQueueBytes, PerSandboxRateLimit: cfg.PerSandboxRateLimit, DropPolicy: cfg.DropPolicy}, sources, sink, checkpoint, targetID, log, onRuntimeError)
	if err != nil {
		readiness.Replace("pipeline-invalid")
		log.Errorf("pipeline invalid: %v", err)
		<-signalCtx.Done()
		return
	}
	events := make(chan api.SourceEvent)
	if err := sources.Start(runCtx, events); err != nil {
		readiness.Replace("source-unavailable")
		log.Errorf("source failed to start: %v", err)
		<-signalCtx.Done()
		return
	}
	if collector, ok := sink.(interface {
		CollectExpired(context.Context, time.Time) error
	}); ok {
		safego.Go(func() {
			ticker := time.NewTicker(min(config.InternalReconcileInterval, 5*time.Minute))
			defer ticker.Stop()
			for {
				select {
				case <-runCtx.Done():
					return
				case now := <-ticker.C:
					if err := collector.CollectExpired(runCtx, now); err != nil {
						readiness.Set("cleanup-error", true)
						log.Errorf("durable cleanup failed: %v", err)
					} else {
						readiness.Set("cleanup-error", false)
					}
				}
			}
		})
	}
	readiness.Set("starting", false)

	pipelineDone := make(chan error, 1)
	safego.Go(func() {
		runPipeline(pipelineDone, func() error { return p.Run(runCtx, events) })
	})
	var runErr error
	pipelineFinished := false
	runtimeFailure := false
	select {
	case runErr = <-pipelineDone:
		pipelineFinished = true
		runtimeFailure = runErr != nil && signalCtx.Err() == nil
		if runtimeFailure {
			readiness.Replace("runtime-error")
			log.Errorf("collection stopped after an unrecoverable runtime error: %v", runErr)
		}
	case <-signalCtx.Done():
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), cfg.SinkTimeout)
	defer shutdownCancel()
	cancelRun()
	stopErr := sources.Stop(shutdownCtx)
	if !pipelineFinished {
		select {
		case runErr = <-pipelineDone:
			pipelineFinished = true
		case <-shutdownCtx.Done():
			runErr = shutdownCtx.Err()
		}
	}
	closeErr := p.Close(shutdownCtx)
	shutdownErr := errors.Join(stopErr, closeErr)
	if !runtimeFailure {
		shutdownErr = errors.Join(ignoreCanceled(runErr), shutdownErr)
	}
	if shutdownErr != nil {
		log.Errorf("shutdown incomplete: %v", shutdownErr)
		if runtimeFailure {
			<-signalCtx.Done()
		}
		return 1
	}
	if runtimeFailure {
		<-signalCtx.Done()
	}
	return 0
}

var errPipelinePanicked = errors.New("pipeline goroutine panicked")

func runPipeline(done chan<- error, run func() error) {
	runErr := errPipelinePanicked
	defer func() { done <- runErr }()
	runErr = run()
}

func waitForSignal() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	<-ctx.Done()
}

func serverAddresses(cfg config.Config, cfgErr error) (healthAddr, pprofAddr string) {
	if cfgErr == nil {
		return cfg.ServerAddr, cfg.PprofAddr
	}
	if host, port, err := net.SplitHostPort(cfg.ServerAddr); err == nil {
		validHost := host == "" || strings.EqualFold(host, "localhost") || net.ParseIP(host) != nil
		if portNumber, err := strconv.Atoi(port); err == nil && portNumber >= 1 && portNumber <= 65535 {
			if validHost {
				return cfg.ServerAddr, ""
			}
			return net.JoinHostPort("", port), ""
		}
	}
	return ":8080", ""
}

func ignoreCanceled(err error) error {
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
