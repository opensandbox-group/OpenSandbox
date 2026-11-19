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

// Command e2b-shim is the scaffold for the optional OSEP-0027 E2B
// compatibility component. Both subcommands validate configuration and serve
// a liveness check only; no E2B protocol or proxy behavior is implemented.
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

	"github.com/alibaba/opensandbox/e2b-shim/pkg/config"
	"github.com/alibaba/opensandbox/e2b-shim/pkg/service"
	"github.com/alibaba/opensandbox/internal/version"
)

const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

const usage = `Usage: e2b-shim <command> [flags]

Scaffold for the optional OpenSandbox E2B compatibility component (OSEP-0027).
No E2B operation is implemented; both commands serve only GET/HEAD /healthz.

Commands:
  adapter   run the lifecycle adapter skeleton
  router    run the envd router skeleton
  version   print build information
  help      print this help

Flags for adapter and router:
  --config string   JSON configuration file (optional)
  --listen string   listen address; overrides E2B_SHIM_LISTEN and the file

See components/e2b-shim/README.md for configuration.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr, os.LookupEnv)
	stop()
	os.Exit(code)
}

// run executes the CLI and returns the process exit code. Errors are written
// to stderr; they come from config validation, which never echoes values.
func run(ctx context.Context, args []string, stdout, stderr io.Writer, lookupEnv func(string) (string, bool)) int {
	if len(args) == 0 {
		_, _ = io.WriteString(stderr, usage)
		return exitUsage
	}
	switch args[0] {
	case "help", "-h", "-help", "--help":
		_, _ = io.WriteString(stdout, usage)
		return exitOK
	case "version":
		version.EchoVersion("OpenSandbox E2B Shim")
		return exitOK
	case "adapter", "router":
		return serve(ctx, args[0], args[1:], stdout, stderr, lookupEnv)
	default:
		_, _ = fmt.Fprint(stderr, "e2b-shim: unknown command\n\n", usage)
		return exitUsage
	}
}

func serve(ctx context.Context, role string, args []string, stdout, stderr io.Writer, lookupEnv func(string) (string, bool)) int {
	fs := flag.NewFlagSet(role, flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "JSON configuration file")
	listen := fs.String("listen", "", "listen address")
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
		_, _ = fmt.Fprintf(stderr, "e2b-shim %s: unexpected arguments\n", role)
		return exitUsage
	}

	src := config.Source{LookupEnv: lookupEnv}
	configSet := false
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "config":
			configSet = true
			src.Path = *configPath
		case "listen":
			src.Listen = listen
		}
	})
	if configSet && src.Path == "" {
		_, _ = fmt.Fprintf(stderr, "e2b-shim %s: --config must not be empty\n", role)
		return exitUsage
	}
	cfg, err := config.Load(src)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "e2b-shim %s: invalid configuration:\n%v\n", role, err)
		return exitError
	}

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		// Report the cause (for example "bind: address already in use")
		// without the address that net.OpError would include.
		cause := ""
		var opErr *net.OpError
		if errors.As(err, &opErr) && opErr.Err != nil {
			cause = ": " + opErr.Err.Error()
		}
		_, _ = fmt.Fprintf(stderr, "e2b-shim %s: listen failed%s\n", role, cause)
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
