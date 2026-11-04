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

// PullImageWithPlatform pulls <name> pinned to the local daemon platform so
// only the architecture the kind node runs on is downloaded.
func PullImageWithPlatform(name string) error {
	cmd := exec.Command("docker", "pull", "--platform", LocalDockerPlatform(), name)
	_, err := Run(cmd)
	return err
}

// LoadPulledImageToKindClusterWithName imports a pulled docker image into the
// kind node via ctr, because "kind load" fails on single-platform pulls
// (kubernetes-sigs/kind#4255).
//
// The archive must be saved with "docker save --platform": a plain
// "docker save" of an image pulled from a multi-arch tag exports an OCI
// archive whose index nests the full manifest list, referencing platforms
// whose blobs were never pulled. containerd 1.x kind nodes (v1.22/v1.28)
// fetch every manifest reachable from that nested index while unpacking and
// fail with "content digest ...: not found"; containerd 2.x nodes filter
// platforms correctly, which is why only some K8s matrix jobs failed.
// --digests is intentionally omitted: docker's attestation manifests have no
// name annotation and unpacking them fails on containerd 1.x with "no
// processor for media-type application/vnd.in-toto+json", so without
// --digests ctr simply skips creating records for them.
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

	// runImport streams the saved archive into the node's containerd and
	// returns an error describing which side failed.
	runImport := func(saveArgs []string) error {
		importCmd := exec.Command("docker", "exec", "--privileged", "-i", node,
			"ctr", "--namespace=k8s.io", "images", "import", "--snapshotter=overlayfs", "-")
		importErrBuf := &bytes.Buffer{}
		importCmd.Stderr = importErrBuf

		saveCmd := exec.Command("docker", saveArgs...)
		saveErrBuf := &bytes.Buffer{}
		saveCmd.Stderr = saveErrBuf

		pipeReader, pipeWriter := io.Pipe()
		saveCmd.Stdout = pipeWriter
		importCmd.Stdin = pipeReader

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
			return fmt.Errorf("%q failed with error %q: %w", strings.Join(saveArgs, " "), strings.TrimSpace(saveErrBuf.String()), saveErr)
		}
		if importErr != nil {
			return fmt.Errorf("%q failed with error %q: %w", strings.Join(importCmd.Args, " "), strings.TrimSpace(importErrBuf.String()), importErr)
		}
		return nil
	}

	// Flatten the archive to a single platform so containerd 1.x nodes can
	// import it. Fall back to a full save on docker versions without
	// "save --platform" (< 27.5).
	err := runImport([]string{"save", "--platform", LocalDockerPlatform(), name})
	if err != nil && strings.Contains(err.Error(), "unknown flag") {
		err = runImport([]string{"save", name})
	}
	return err
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
