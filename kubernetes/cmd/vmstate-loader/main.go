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
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/snapshot"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/snapshot/vmstate"
)

const checkpointDir = "/opensandbox/checkpoint"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "restore":
		runRestore(os.Args[2:])
	case "verify":
		runVerify(os.Args[2:])
	case "stream":
		runStream(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "Usage: vmstate-loader <restore|verify|stream> [options]")
}

func runRestore(args []string) {
	flags := flag.NewFlagSet("restore", flag.ExitOnError)
	source := flags.String("source", checkpointDir, "checkpoint image directory")
	target := flags.String("target", "", "shared restore directory")
	manifestDigest := flags.String("manifest-digest", "", "expected sha256 manifest digest")
	_ = flags.Parse(args)
	if *target == "" || *manifestDigest == "" {
		fatal("--target and --manifest-digest are required")
	}
	executablePath, err := os.Executable()
	if err != nil {
		fatal(err.Error())
	}
	if err := vmstate.Restore(*source, *target, *manifestDigest, executablePath); err != nil {
		fatal(err.Error())
	}
}

func runVerify(args []string) {
	flags := flag.NewFlagSet("verify", flag.ExitOnError)
	directory := flags.String("dir", checkpointDir, "checkpoint directory")
	manifestDigest := flags.String("manifest-digest", "", "optional expected sha256 manifest digest")
	_ = flags.Parse(args)
	manifest, _, err := vmstate.ReadManifest(filepath.Join(*directory, snapshot.VMStateManifestFilename), *manifestDigest)
	if err != nil {
		fatal(err.Error())
	}
	if err := vmstate.VerifyPayload(filepath.Join(*directory, snapshot.VMStatePayloadFilename), manifest); err != nil {
		fatal(err.Error())
	}
}

func runStream(args []string) {
	flags := flag.NewFlagSet("stream", flag.ExitOnError)
	directory := flags.String("dir", "", "restored checkpoint directory")
	_ = flags.Parse(args)
	if *directory == "" {
		fatal("--dir is required")
	}
	if err := vmstate.Stream(
		filepath.Join(*directory, snapshot.VMStateManifestFilename),
		filepath.Join(*directory, snapshot.VMStatePayloadFilename),
		os.Stdout,
	); err != nil {
		fatal(err.Error())
	}
}

func fatal(message string) {
	fmt.Fprintf(os.Stderr, "ERROR: %s\n", message)
	os.Exit(1)
}
