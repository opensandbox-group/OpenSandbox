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
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	snapshotcontract "github.com/alibaba/OpenSandbox/sandbox-k8s/internal/snapshot"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/pkg/imagecommitter"
	imagecommittercli "github.com/alibaba/OpenSandbox/sandbox-k8s/pkg/imagecommitter/cli"
)

var terminationMessagePath = "/dev/termination-log"

const registryConfigPath = "/var/run/opensandbox/registry/config.json"

// commandCombinedOutput is a var so tests can replace it.
var commandCombinedOutput = func(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

func main() {
	args := os.Args[1:]

	// QEMU-specific subcommands use the nerdctl-based path, which requires
	// HostPID access and runs qemu-checkpoint-helper inside the container.
	if len(args) > 0 && (args[0] == "snapshot" || args[0] == "recover-qemu") {
		recovery := &snapshotRecovery{}

		// Recover the source workload if the snapshot process is interrupted.
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt, syscall.SIGTERM)
		go func() {
			sig := <-c
			fmt.Fprintf(os.Stderr, "Received signal %v, recovering snapshot source...\n", sig)
			recoverSnapshotSource(recovery)
			os.Exit(1)
		}()

		// Recover the source workload if snapshot orchestration panics.
		defer func() {
			if r := recover(); r != nil {
				fmt.Fprintf(os.Stderr, "Panic occurred: %v\n", r)
				recoverSnapshotSource(recovery)
				panic(r)
			}
		}()

		if args[0] == "recover-qemu" {
			runRecoverQEMU(args[1:])
			return
		}
		if err := runSnapshotRequest(args[1:], recovery); err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: snapshot failed: %v\n", err)
			recoverSnapshotSource(recovery)
			os.Exit(1)
		}
		return
	}

	// Standard rootfs commit and unpause via the containerd API path.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	provider := imagecommitter.DockerConfigCredentialProvider{Path: registryConfigPath, ErrorOutput: os.Stderr}
	if err := imagecommittercli.Run(ctx, args, imagecommittercli.Config{
		CredentialProvider:       provider,
		SourceCredentialProvider: provider,
		TerminationMessagePath:   terminationMessagePath,
		Output:                   os.Stdout,
		ErrorOutput:              os.Stderr,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}
}

// ContainerSpec maps a source container to its target image.
// Used by the legacy rootfs snapshot path and its tests.
type ContainerSpec struct {
	Name string
	URI  string
}

// writeSnapshotResult writes the legacy rootfs snapshot result to the
// Kubernetes termination message path.
func writeSnapshotResult(containerSpecs []ContainerSpec, digests map[string]string) error {
	result := snapshotcontract.Result{
		Containers: make([]snapshotcontract.ContainerResult, 0, len(digests)),
	}
	for _, spec := range containerSpecs {
		digest, ok := digests[spec.Name]
		if !ok {
			continue
		}
		result.Containers = append(result.Containers, snapshotcontract.ContainerResult{
			Name:   spec.Name,
			Image:  spec.URI,
			Digest: digest,
		})
	}
	data, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return os.WriteFile(terminationMessagePath, append(data, '\n'), 0o644)
}

// containerdSocket returns the containerd socket address from env or default.
func containerdSocket() string {
	if v := strings.TrimSpace(os.Getenv("CONTAINERD_SOCKET")); v != "" {
		return v
	}
	return "/run/containerd/containerd.sock"
}

// containerdNamespace returns the containerd namespace from env or default.
func containerdNamespace() string {
	if v := strings.TrimSpace(os.Getenv("CONTAINERD_NAMESPACE")); v != "" {
		return v
	}
	return "k8s.io"
}

// nerdctlBaseArgs returns the base arguments for nerdctl commands.
func nerdctlBaseArgs() []string {
	return []string{"--address", containerdSocket(), "--namespace", containerdNamespace()}
}

// getContainerIDByNerdctl finds a container ID using nerdctl ps with Kubernetes labels.
// This approach directly queries containerd (k8s.io namespace) without going through
// the CRI API, making it compatible with all containerd versions.
// Kubernetes injects standard labels on all containers:
//   - io.kubernetes.pod.name
//   - io.kubernetes.pod.namespace
//   - io.kubernetes.pod.uid
//   - io.kubernetes.container.name
func getContainerIDByNerdctl(podName, podNamespace, podUID, containerName string) (string, error) {
	containerID, err := lookupContainerIDByNerdctl(podName, podNamespace, podUID, containerName, false)
	if err != nil {
		return "", err
	}
	if containerID != "" {
		return containerID, nil
	}

	containerID, err = lookupContainerIDByNerdctl(podName, podNamespace, podUID, containerName, true)
	if err != nil {
		return "", err
	}
	if containerID != "" {
		return containerID, nil
	}

	return "", fmt.Errorf(
		"container '%s' not found in pod %s/%s (nerdctl ps and nerdctl ps -a returned empty)",
		containerName,
		podNamespace,
		podName,
	)
}

func lookupContainerIDByNerdctl(podName, podNamespace, podUID, containerName string, includeStopped bool) (string, error) {
	args := append(nerdctlBaseArgs(), "ps")
	if includeStopped {
		args = append(args, "-a")
	}
	args = append(args,
		"-q",
		"--filter", fmt.Sprintf("label=io.kubernetes.pod.name=%s", podName),
		"--filter", fmt.Sprintf("label=io.kubernetes.pod.namespace=%s", podNamespace),
	)
	if podUID != "" {
		args = append(args, "--filter", fmt.Sprintf("label=io.kubernetes.pod.uid=%s", podUID))
	}
	args = append(args,
		"--filter", fmt.Sprintf("label=io.kubernetes.container.name=%s", containerName),
	)
	output, err := commandCombinedOutput("nerdctl", args...)
	if err != nil {
		mode := "nerdctl ps"
		if includeStopped {
			mode = "nerdctl ps -a"
		}
		return "", fmt.Errorf(
			"%s failed for pod=%s ns=%s container=%s: %v, output: %s",
			mode,
			podName,
			podNamespace,
			containerName,
			err,
			strings.TrimSpace(string(output)),
		)
	}

	containerID := strings.TrimSpace(string(output))
	if containerID == "" {
		return "", nil
	}

	// nerdctl ps -q may return multiple lines; take the first (most recently started)
	lines := strings.Split(containerID, "\n")
	return strings.TrimSpace(lines[0]), nil
}

// pauseContainer uses nerdctl to pause a container.
func pauseContainer(containerID string) error {
	fmt.Printf("Pausing container %s...\n", containerID)
	args := append(nerdctlBaseArgs(), "pause", containerID)
	cmd := exec.Command("nerdctl", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to pause container %s: %v, output: %s", containerID, err, string(output))
	}
	fmt.Printf("Paused successfully: %s\n", containerID)
	return nil
}

// resumeContainer uses nerdctl to resume a container.
func resumeContainer(containerID string) error {
	fmt.Printf("Resuming container %s...\n", containerID)
	args := append(nerdctlBaseArgs(), "unpause", containerID)
	cmd := exec.Command("nerdctl", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to resume container %s: %v, output: %s", containerID, err, string(output))
	}
	fmt.Printf("Resumed successfully: %s\n", containerID)
	return nil
}

// commitContainer uses nerdctl to commit a container to an image.
func commitContainer(containerID, targetImage string) error {
	fmt.Printf("Committing container %s to image %s...\n", containerID, targetImage)
	args := append(nerdctlBaseArgs(), "commit", containerID, targetImage)
	cmd := exec.Command("nerdctl", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to commit container %s to %s: %v, output: %s", containerID, targetImage, err, string(output))
	}
	return nil
}

// pushImage uses nerdctl to push the image to the registry.
// nerdctl push does not support --username/--password flags, so we use
// nerdctl login first, then nerdctl push with --insecure-registry.
func pushImage(targetImage string) error {
	fmt.Printf("Pushing image %s...\n", targetImage)

	// Parse registry host from target image
	imageParts := strings.Split(targetImage, "/")
	if len(imageParts) == 0 {
		return fmt.Errorf("invalid target image: %s", targetImage)
	}
	registryHost := imageParts[0]

	isInsecure := shouldUseInsecureRegistry(registryHost)

	// Try to login using credentials from mounted secret
	credDir := "/var/run/opensandbox/registry"
	configPath := filepath.Join(credDir, "config.json")
	if _, err := os.Stat(configPath); err == nil {
		fmt.Printf("Found registry credentials at %s\n", configPath)
		if err := nerdctlLogin(configPath, registryHost, isInsecure); err != nil {
			fmt.Fprintf(os.Stderr, "WARNING: nerdctl login failed: %v (will attempt push anyway)\n", err)
		}
	} else {
		fmt.Println("No registry credentials found, assuming insecure or pre-authenticated registry")
	}

	// Build push options
	pushOpts := append(nerdctlBaseArgs(), "push")
	if isInsecure {
		pushOpts = append(pushOpts, "--insecure-registry")
	}
	pushOpts = append(pushOpts, targetImage)

	cmd := exec.Command("nerdctl", pushOpts...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to push image %s: %v, output: %s", targetImage, err, string(output))
	}

	return nil
}

// nerdctlLogin extracts credentials from a Docker config.json and runs nerdctl login.
func nerdctlLogin(configPath, registryHost string, insecure bool) error {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("failed to read config: %w", err)
	}

	var creds map[string]interface{}
	if err := json.Unmarshal(data, &creds); err != nil {
		return fmt.Errorf("failed to parse config: %w", err)
	}

	auths, ok := creds["auths"].(map[string]interface{})
	if !ok || auths[registryHost] == nil {
		return fmt.Errorf("no auth entry for registry %s", registryHost)
	}

	authEntry, ok := auths[registryHost].(map[string]interface{})
	if !ok {
		return fmt.Errorf("invalid auth entry for registry %s", registryHost)
	}

	// Try "auth" field first (base64 encoded), then fall back to username/password fields
	var username, password string
	if authVal, ok := authEntry["auth"].(string); ok && authVal != "" {
		decoded, err := base64.StdEncoding.DecodeString(authVal)
		if err != nil {
			return fmt.Errorf("failed to decode auth: %w", err)
		}
		parts := strings.SplitN(string(decoded), ":", 2)
		if len(parts) != 2 {
			return fmt.Errorf("invalid auth format")
		}
		username = parts[0]
		password = parts[1]
	} else {
		if u, ok := authEntry["username"].(string); ok {
			username = u
		}
		if p, ok := authEntry["password"].(string); ok {
			password = p
		}
	}

	if username == "" || password == "" {
		return fmt.Errorf("empty username or password for registry %s", registryHost)
	}

	fmt.Printf("Logging in to registry %s as %s\n", registryHost, username)

	loginOpts := append(nerdctlBaseArgs(), "login", "-u", username, "-p", password)
	if insecure {
		loginOpts = append(loginOpts, "--insecure-registry")
	}
	loginOpts = append(loginOpts, registryHost)

	cmd := exec.Command("nerdctl", loginOpts...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("nerdctl login failed: %v, output: %s", err, string(output))
	}

	fmt.Printf("Login succeeded for %s\n", registryHost)
	return nil
}

func shouldUseInsecureRegistry(registryHost string) bool {
	if raw := strings.TrimSpace(os.Getenv("SNAPSHOT_REGISTRY_INSECURE")); raw != "" {
		value, err := strconv.ParseBool(raw)
		if err == nil {
			return value
		}
		fmt.Fprintf(os.Stderr, "WARNING: invalid SNAPSHOT_REGISTRY_INSECURE=%q, falling back to registry host heuristic\n", raw)
	}

	return strings.Contains(registryHost, "local") ||
		strings.Contains(registryHost, "localhost") ||
		strings.HasPrefix(registryHost, "127.") ||
		strings.HasPrefix(registryHost, "10.") ||
		strings.HasPrefix(registryHost, "192.168.") ||
		isPrivate172Registry(registryHost)
}

func isPrivate172Registry(registryHost string) bool {
	host := strings.SplitN(registryHost, ":", 2)[0]
	parts := strings.Split(host, ".")
	if len(parts) < 2 || parts[0] != "172" {
		return false
	}
	secondOctet, err := strconv.Atoi(parts[1])
	return err == nil && secondOctet >= 16 && secondOctet <= 31
}

type nerdctlNativeImageInspect struct {
	Target struct {
		Digest string `json:"digest"`
	} `json:"Target"`
	Image struct {
		Target struct {
			Digest string `json:"digest"`
		} `json:"Target"`
	} `json:"Image"`
	ManifestDesc struct {
		Digest string `json:"digest"`
	} `json:"ManifestDesc"`
}

// getImageDigest uses nerdctl's native descriptor rather than the Docker
// config ID. Native inspect changed shape between nerdctl releases, so accept
// both the direct Target and the older Image.Target/ManifestDesc layouts.
func getImageDigest(imageRef string) (string, error) {
	args := append(nerdctlBaseArgs(), "image", "inspect", "--mode=native", imageRef)
	output, err := commandCombinedOutput("nerdctl", args...)
	if err != nil {
		return "", fmt.Errorf("nerdctl inspect failed for image %s: %w, output: %s", imageRef, err, strings.TrimSpace(string(output)))
	}
	jsonStart := bytes.IndexByte(output, '[')
	if jsonStart < 0 {
		return "", fmt.Errorf("nerdctl native inspect returned no JSON array for image %s: %s", imageRef, strings.TrimSpace(string(output)))
	}
	var inspected []nerdctlNativeImageInspect
	if err := json.Unmarshal(output[jsonStart:], &inspected); err != nil {
		return "", fmt.Errorf("decode nerdctl native inspect for image %s: %w", imageRef, err)
	}
	for _, item := range inspected {
		for _, digest := range []string{item.Target.Digest, item.Image.Target.Digest, item.ManifestDesc.Digest} {
			if digest == "" {
				continue
			}
			if _, err := snapshotcontract.ParseSHA256Digest(digest); err != nil {
				return "", fmt.Errorf("nerdctl inspect returned invalid manifest digest for image %s: %w", imageRef, err)
			}
			return digest, nil
		}
	}
	return "", fmt.Errorf("nerdctl native inspect returned no manifest digest for image %s", imageRef)
}
