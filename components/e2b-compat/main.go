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

// Command e2b-compat is the scaffold for the optional OSEP-0027 E2B
// compatibility component. Both subcommands serve a liveness check only;
// no E2B protocol or proxy behavior is implemented.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/alibaba/opensandbox/e2b-compat/pkg/config"
	"github.com/alibaba/opensandbox/e2b-compat/pkg/service"
	"github.com/alibaba/opensandbox/internal/version"
)

const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

const usage = `Usage: e2b-compat <command> [flags]

Scaffold for the optional OpenSandbox E2B compatibility component (OSEP-0027).
No E2B operation is implemented; both commands serve only GET/HEAD /healthz.

Commands:
  adapter   run the lifecycle adapter skeleton
  router    run the envd router skeleton
  version   print build information
  help      print this help

Flags for adapter and router:
  --listen string   listen address (default 127.0.0.1:8080)
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run executes the CLI and returns the process exit code.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = io.WriteString(stderr, usage)
		return exitUsage
	}
	switch args[0] {
	case "help", "-h", "-help", "--help":
		_, _ = io.WriteString(stdout, usage)
		return exitOK
	case "version":
		version.EchoVersion("OpenSandbox E2B Compat")
		return exitOK
	case "adapter", "router":
		return serve(ctx, args[0], args[1:], stdout, stderr)
	default:
		_, _ = fmt.Fprint(stderr, "e2b-compat: unknown command\n\n", usage)
		return exitUsage
	}
}

func serve(ctx context.Context, role string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(role, flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", config.DefaultListen, "listen address")
	fs.Usage = func() {}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, _ = io.WriteString(stdout, usage)
			return exitOK
		}
		_, _ = io.WriteString(stderr, usage)
		return exitUsage
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "e2b-compat %s: unexpected arguments\n", role)
		return exitUsage
	}

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "e2b-compat %s: listen failed: %v\n", role, err)
		return exitError
	}
	logger := slog.New(slog.NewTextHandler(stderr, nil))
	logger.Info("listening", "role", role, "addr", ln.Addr().String(), "health_path", config.HealthPath)

	if err := service.Serve(ctx, ln, service.NewHandler(), service.ShutdownTimeout); err != nil {
		logger.Error("server stopped with error", "role", role, "error", err.Error())
		return exitError
	}
	logger.Info("stopped", "role", role)
	return exitOK
}
