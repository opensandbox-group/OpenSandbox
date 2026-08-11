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
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/alibaba/opensandbox/internal/version"

	_ "github.com/alibaba/opensandbox/internal/safego"
	_ "go.uber.org/automaxprocs/maxprocs"

	"github.com/alibaba/opensandbox/execd/pkg/clone3compat"
	"github.com/alibaba/opensandbox/execd/pkg/flag"
	"github.com/alibaba/opensandbox/execd/pkg/isolation"
	"github.com/alibaba/opensandbox/execd/pkg/log"
	"github.com/alibaba/opensandbox/execd/pkg/runtime"
	"github.com/alibaba/opensandbox/execd/pkg/telemetry"
	"github.com/alibaba/opensandbox/execd/pkg/web"
	"github.com/alibaba/opensandbox/execd/pkg/web/controller"
)

const (
	// Only retry fast, retained namespace cleanup. Process teardown is
	// synchronous and may already consume its own bounded wait.
	isolatedRunnerCloseRetryTimeout  = 5 * time.Second
	isolatedRunnerCloseRetryInterval = 100 * time.Millisecond
)

type isolatedRunnerCloser interface {
	Close() error
}

func main() {
	os.Exit(run())
}

func run() int {
	clone3Compat := clone3compat.MaybeApply()

	version.EchoVersion("OpenSandbox Execd")

	flag.InitFlags()

	// Load isolation config.
	isoCfg, err := isolation.LoadConfig(flag.IsolationConfigPath)
	if err != nil {
		log.Error("isolation: config: %v", err)
		return 1
	}

	// Probe isolation runtime capabilities.
	isolationProbe := isolation.Probe(isolation.ProbeConfig{
		UpperRoot:     isoCfg.UpperRoot,
		UpperMaxBytes: isoCfg.UpperMaxBytes,
	})
	log.Info("isolation: available=%v isolator=%s version=%s",
		isolationProbe.Available, isolationProbe.Isolator, isolationProbe.Version)

	log.Init(flag.ServerLogLevel)

	ctrl := controller.InitCodeRunner()

	// Always store probe result for capabilities endpoint.
	controller.InitIsolatedProbe(&isolationProbe)

	// Init isolation runner if probe succeeded.
	if isolationProbe.Available {
		iso := isolation.NewBwrapWithProbe(isoCfg, isolationProbe)
		runner, err := runtime.NewIsolatedRunner(ctrl, iso, isoCfg)
		if err != nil {
			log.Error("isolation: runner init failed (continuing without isolation): %v", err)
		} else {
			controller.InitIsolatedRunner(runner)
			defer func() {
				if err := closeIsolatedRunnerWithRetry(
					runner,
					isolatedRunnerCloseRetryTimeout,
					isolatedRunnerCloseRetryInterval,
					func(err error) {
						log.Warn(
							"isolation: runner shutdown cleanup failed; retrying: %v",
							err,
						)
					},
				); err != nil {
					log.Error("isolation: runner shutdown failed: %v", err)
				}
			}()
			log.Info("isolation: runner ready, upper_root=%s", isoCfg.UpperRoot)
		}
	}
	if clone3Compat {
		log.Warn("execd running with clone3 compatibility (seccomp returns ENOSYS for clone3)")
	}
	otelShutdown, err := telemetry.Init(context.Background())
	if err != nil {
		log.Warn("OpenTelemetry metrics disabled (continuing without OTLP): %v", err)
		otelShutdown = nil
	}
	if otelShutdown != nil {
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = otelShutdown(shutdownCtx)
		}()
	}

	engine := web.NewRouter(flag.ServerAccessToken)
	addr := fmt.Sprintf(":%d", flag.ServerPort)
	listener, err := net.Listen("tcp4", addr)
	if err != nil {
		log.Error("failed to listen on %s: %v", addr, err)
		return 1
	}
	log.Info("execd listening on %s (IPv4)", addr)
	serverCtx, stopSignals := signal.NotifyContext(
		context.Background(),
		os.Interrupt,
		syscall.SIGTERM,
	)
	defer stopSignals()
	if err := serveHTTPUntilShutdown(serverCtx, listener, engine); err != nil {
		log.Error("execd server stopped with error: %v", err)
		return 1
	}
	return 0
}

func closeIsolatedRunnerWithRetry(
	runner isolatedRunnerCloser,
	retryTimeout time.Duration,
	retryInterval time.Duration,
	reportRetry func(error),
) error {
	err := runner.Close()
	if !isRetryableIsolatedRunnerCloseError(err) {
		return err
	}

	retryCtx, cancelRetry := context.WithTimeout(
		context.Background(),
		retryTimeout,
	)
	defer cancelRetry()
	for {
		if reportRetry != nil {
			reportRetry(err)
		}
		timer := time.NewTimer(retryInterval)
		select {
		case <-retryCtx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return errors.Join(
				err,
				fmt.Errorf(
					"retry isolated runner cleanup: %w",
					retryCtx.Err(),
				),
			)
		case <-timer.C:
		}

		err = runner.Close()
		if !isRetryableIsolatedRunnerCloseError(err) {
			return err
		}
	}
}

func isRetryableIsolatedRunnerCloseError(err error) bool {
	return errors.Is(err, runtime.ErrSessionNamespaceCleanup)
}

func serveHTTPUntilShutdown(
	ctx context.Context,
	listener net.Listener,
	handler http.Handler,
) error {
	server := &http.Server{Handler: handler}
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- server.Serve(listener)
	}()

	select {
	case err := <-serveDone:
		if err == nil || errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(
		context.Background(),
		5*time.Second,
	)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		closeErr := server.Close()
		serveErr := <-serveDone
		return errors.Join(
			fmt.Errorf("gracefully shut down execd server: %w", err),
			closeErr,
			serveErr,
		)
	}
	if err := <-serveDone; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
