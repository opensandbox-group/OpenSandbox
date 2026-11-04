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

package utils

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"

	. "github.com/onsi/ginkgo/v2" // nolint:revive,staticcheck
)

// Run executes the provided command within this context
func Run(cmd *exec.Cmd) (string, error) {
	dir, _ := GetProjectDir()
	cmd.Dir = dir

	if err := os.Chdir(cmd.Dir); err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "chdir dir: %q\n", err)
	}

	cmd.Env = append(os.Environ(), "GO111MODULE=on")
	command := strings.Join(cmd.Args, " ")
	_, _ = fmt.Fprintf(GinkgoWriter, "running: %q\n", command)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("%q failed with error %q: %w", command, string(output), err)
	}

	return string(output), nil
}

// LoadImageToKindClusterWithName loads a local docker image to the kind cluster.
// When E2E_MODE is not "kind" this is a no-op: the image is expected to live in
// a registry the target cluster can pull from, so there is no Kind node to
// load into.
func LoadImageToKindClusterWithName(name string) error {
	if IsExternal() {
		_, _ = fmt.Fprintf(GinkgoWriter, "skipping kind load for %q (E2E_MODE=%s)\n", name, Mode())
		return nil
	}
	cluster := "kind"
	if v, ok := os.LookupEnv("KIND_CLUSTER"); ok {
		cluster = v
	}
	kindOptions := []string{"load", "docker-image", name, "--name", cluster}
	cmd := exec.Command("kind", kindOptions...)
	_, err := Run(cmd)
	return err
}

// LocalDockerPlatform returns the "linux/<arch>" platform string matching the
// local docker daemon (and therefore the kind node architecture). It falls
// back to the test binary architecture when the daemon cannot be queried.
func LocalDockerPlatform() string {
	out, err := exec.Command("docker", "version", "--format", "{{.Server.Arch}}").Output()
	if err == nil {
		if arch := strings.TrimSpace(string(out)); arch != "" {
			return "linux/" + arch
		}
	}
	return "linux/" + runtime.GOARCH
}

// PullImageWithPlatform pulls <name> pinned to the local daemon platform
// after removing any existing image with the same tag. The removal matters:
// when the tag already exists locally (e.g. pre-cached on GitHub runners),
// "docker pull --platform" is a no-op that keeps the existing multi-arch
// image record, and "docker save" then exports an OCI index referencing
// platforms whose blobs were never pulled (see
// LoadPulledImageToKindClusterWithName).
func PullImageWithPlatform(name string) error {
	_ = exec.Command("docker", "image", "rm", "-f", name).Run()
	cmd := exec.Command("docker", "pull", "--platform", LocalDockerPlatform(), name)
	_, err := Run(cmd)
	return err
}

// LoadPulledImageToKindClusterWithName imports a pulled docker image into the
// kind node via ctr, because "kind load" fails on single-platform pulls
// (kubernetes-sigs/kind#4255).
//
// The image must have been pulled with an explicit --platform pin. Without
// one, a docker daemon using the containerd image store keeps the full
// multi-arch index locally, and "docker save" then exports an OCI index that
// references platforms whose blobs were never pulled. containerd 1.x nodes
// (kind v1.22/v1.28) create image records for every entry and fail unpacking
// them with "content digest ...: not found".
func LoadPulledImageToKindClusterWithName(name string) error {
	if IsExternal() {
		_, _ = fmt.Fprintf(GinkgoWriter, "skipping image import for %q (E2E_MODE=%s)\n", name, Mode())
		return nil
	}
	cluster := "kind"
	if v, ok := os.LookupEnv("KIND_CLUSTER"); ok {
		cluster = v
	}
	node := fmt.Sprintf("%s-control-plane", cluster)

	saveCmd := exec.Command("docker", "save", name)
	importCmd := exec.Command("docker", "exec", "--privileged", "-i", node,
		"ctr", "--namespace=k8s.io", "images", "import", "--digests", "--snapshotter=overlayfs", "-")

	pipeReader, pipeWriter := io.Pipe()
	var saveErrBuf, importErrBuf bytes.Buffer
	saveCmd.Stdout = pipeWriter
	saveCmd.Stderr = &saveErrBuf
	importCmd.Stdin = pipeReader
	importCmd.Stderr = &importErrBuf

	var importErr error
	importDone := make(chan struct{})
	go func() {
		defer close(importDone)
		importErr = importCmd.Run()
	}()

	saveErr := saveCmd.Run()
	_ = pipeWriter.Close()
	<-importDone
	_ = pipeReader.Close()

	if saveErr != nil {
		return fmt.Errorf("%q failed with error %q: %w", strings.Join(saveCmd.Args, " "), strings.TrimSpace(saveErrBuf.String()), saveErr)
	}
	if importErr != nil {
		return fmt.Errorf("%q failed with error %q: %w", strings.Join(importCmd.Args, " "), strings.TrimSpace(importErrBuf.String()), importErr)
	}
	return nil
}

// GetNonEmptyLines converts given command output string into individual objects
// according to line breakers, and ignores the empty elements in it.
func GetNonEmptyLines(output string) []string {
	var res []string
	elements := strings.Split(output, "\n")
	for _, element := range elements {
		if element != "" {
			res = append(res, element)
		}
	}

	return res
}

// GetProjectDir will return the directory where the project is
func GetProjectDir() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return wd, fmt.Errorf("failed to get current working directory: %w", err)
	}
	wd = strings.ReplaceAll(wd, "/test/e2e", "")
	return wd, nil
}
