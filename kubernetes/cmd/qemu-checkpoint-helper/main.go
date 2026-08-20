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
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/snapshot/qmp"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "probe":
		runProbe(os.Args[2:])
	case "export":
		runExport(os.Args[2:])
	case "resume":
		runResume(os.Args[2:])
	case "resolve-path":
		runResolvePath(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "Usage: qemu-checkpoint-helper <probe|export|resume> --socket <qmp.sock> [--timeout 5m]")
	fmt.Fprintln(os.Stderr, "       qemu-checkpoint-helper resolve-path <path>")
}

func commandFlags(name string, args []string) (string, time.Duration) {
	flags := flag.NewFlagSet(name, flag.ExitOnError)
	socketPath := flags.String("socket", "", "QMP Unix socket path")
	timeout := flags.Duration("timeout", 5*time.Minute, "command timeout")
	_ = flags.Parse(args)
	if *socketPath == "" {
		fmt.Fprintln(os.Stderr, "ERROR: --socket is required")
		os.Exit(2)
	}
	if *timeout <= 0 {
		fmt.Fprintln(os.Stderr, "ERROR: --timeout must be positive")
		os.Exit(2)
	}
	return *socketPath, *timeout
}

func runProbe(args []string) {
	socketPath, timeout := commandFlags("probe", args)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	client, err := qmp.Dial(ctx, socketPath)
	if err != nil {
		fatal(err)
	}
	defer client.Close()

	version, err := client.Version(ctx)
	if err != nil {
		fatal(err)
	}
	data, err := json.Marshal(struct {
		Version string `json:"version"`
	}{Version: version.String()})
	if err != nil {
		fatal(err)
	}
	fmt.Println(string(data))
}

func runExport(args []string) {
	socketPath, timeout := commandFlags("export", args)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	client, err := qmp.Dial(ctx, socketPath)
	if err != nil {
		fatal(err)
	}
	defer client.Close()

	if err := client.ExportMigration(ctx, os.Stdout, 100*time.Millisecond); err != nil {
		fatal(err)
	}
	fmt.Fprintln(os.Stderr, "QEMU migration stream exported; source VM is in postmigrate")
}

func runResume(args []string) {
	socketPath, timeout := commandFlags("resume", args)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	client, err := qmp.Dial(ctx, socketPath)
	if err != nil {
		fatal(err)
	}
	defer client.Close()
	if err := client.Continue(ctx); err != nil {
		fatal(err)
	}
}

func runResolvePath(args []string) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "ERROR: resolve-path requires exactly one path")
		os.Exit(2)
	}
	resolved, err := filepath.EvalSymlinks(args[0])
	if err != nil {
		fatal(fmt.Errorf("resolve path %q: %w", args[0], err))
	}
	fmt.Println(resolved)
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
	os.Exit(1)
}
