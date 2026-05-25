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
	"net"
	"os"
	"time"

	"github.com/alibaba/opensandbox/internal/version"

	_ "github.com/alibaba/opensandbox/internal/safego"
	_ "go.uber.org/automaxprocs/maxprocs"

	"github.com/alibaba/opensandbox/execd/pkg/clone3compat"
	"github.com/alibaba/opensandbox/execd/pkg/flag"
	"github.com/alibaba/opensandbox/execd/pkg/log"
	"github.com/alibaba/opensandbox/execd/pkg/telemetry"
	"github.com/alibaba/opensandbox/execd/pkg/web"
	"github.com/alibaba/opensandbox/execd/pkg/web/controller"
)

func main() {
	clone3Compat := clone3compat.MaybeApply()

	version.EchoVersion("OpenSandbox Execd")

	flag.InitFlags()

	log.Init(flag.ServerLogLevel)
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

	controller.InitCodeRunner()
	engine := web.NewRouter(flag.ServerAccessToken)
	addr := fmt.Sprintf(":%d", flag.ServerPort)
	listener, err := net.Listen("tcp4", addr)
	if err != nil {
		log.Error("failed to listen on %s: %v", addr, err)
		os.Exit(1)
	}
	log.Info("execd listening on %s (IPv4)", addr)
	if err := engine.RunListener(listener); err != nil {
		log.Error("failed to start execd server: %v", err)
		os.Exit(1)
	}
}
