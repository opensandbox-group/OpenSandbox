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
)

var commandCombinedOutput = func(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

var terminationMessagePath = "/dev/termination-log"

// containerdSocket returns the containerd socket address from env or default
func containerdSocket() string {
	if v := os.Getenv("CONTAINERD_SOCKET"); v != "" {
		return v
	}
	return "/run/containerd/containerd.sock"
}

// containerdNamespace returns the containerd namespace from env or default
func containerdNamespace() string {
	if v := os.Getenv("CONTAINERD_NAMESPACE"); v != "" {
		return v
	}
	return "k8s.io"
}

// nerdctlBaseArgs returns the base arguments for nerdctl commands
func nerdctlBaseArgs() []string {
	return []string{"--address", containerdSocket(), "--namespace", containerdNamespace()}
}

type ContainerSpec struct {
	Name string
	URI  string
}

type snapshotResult struct {
	Containers []snapshotContainerResult `json:"containers"`
}

type snapshotContainerResult struct {
	Name   string `json:"name"`
	Image  string `json:"image"`
	Digest string `json:"digest"`
}

// Global tracking of paused containers for cleanup
var pausedContainerIds []string

func main() {
	args := os.Args[1:]

	// Set up signal handler to ensure all paused containers are resumed on exit
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-c
		fmt.Fprintf(os.Stderr, "Received signal %v, cleaning up paused containers...\n", sig)
		resumeAllPausedContainers()
		os.Exit(1)
	}()

	// Defer cleanup in case of panic or early termination
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "Panic occurred: %v\n", r)
			resumeAllPausedContainers()
			panic(r)
		}
	}()

	if len(args) > 0 && args[0] == "unpause" {
		runUnpause(args[1:])
		return
	}

	// Parse arguments using unified format:
	// <pod_name> <namespace> <container1:uri1> [container2:uri2...]
	var podName, namespace string
	var containerSpecs []ContainerSpec

	if len(args) < 3 {
		fmt.Fprintln(os.Stderr, "ERROR: Missing required parameters")
		fmt.Fprintln(os.Stderr, "Usage: commit-snapshot <pod_name> <namespace> <container1:uri1> [container2:uri2...]")
		os.Exit(1)
	}

	podName = args[0]
	namespace = args[1]

	for i := 2; i < len(args); i++ {
		spec, err := parseContainerSpec(args[i])
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
			os.Exit(1)
		}
		containerSpecs = append(containerSpecs, spec)
	}

	// Validate required inputs
	if len(podName) == 0 {
		fmt.Fprintln(os.Stderr, "ERROR: Pod name is required")
		os.Exit(1)
	}

	if len(namespace) == 0 {
		fmt.Fprintln(os.Stderr, "ERROR: Namespace is required")
		os.Exit(1)
	}

	if len(containerSpecs) == 0 {
		fmt.Fprintln(os.Stderr, "ERROR: At least one container specification is required")
		fmt.Fprintln(os.Stderr, "Usage: commit-snapshot <pod_name> <namespace> <container1:uri1> [container2:uri2...]")
		os.Exit(1)
	}

	fmt.Println("=== Commit Snapshot Go Program ===")
	fmt.Printf("Pod: %s\n", podName)
	fmt.Printf("Namespace: %s\n", namespace)
	for _, spec := range containerSpecs {
		fmt.Printf("Container spec: %s -> %s\n", spec.Name, spec.URI)
	}

	// Step 1: Find container IDs via nerdctl (direct containerd API, no CRI dependency)
	fmt.Println("\n=== Step 1: Find container IDs via nerdctl ===")
	containerMap := make(map[string]string) // Maps container name to container ID
	for _, spec := range containerSpecs {
		containerID, err := getContainerIDByNerdctl(podName, namespace, spec.Name)
		if err != nil {
			resumeAllPausedContainers()
			fmt.Fprintf(os.Stderr, "ERROR: Failed to find container '%s': %v\n", spec.Name, err)
			os.Exit(1)
		}

		fmt.Printf("Container '%s' -> ID: %s\n", spec.Name, containerID)
		containerMap[spec.Name] = containerID
	}

	// Step 2: Pause all containers
	fmt.Println("\n=== Step 2: Pause all containers ===")
	pauseErrors := 0
	for _, spec := range containerSpecs {
		containerID := containerMap[spec.Name]
		if err := pauseContainer(containerID); err != nil {
			// On pause failure, we still try to continue since commit might work anyway (as in shell script)
			fmt.Fprintf(os.Stderr, "WARNING: Could not pause '%s'. Will attempt commit anyway (container may be stopped).\n", spec.Name)
			pauseErrors++
		} else {
			// Track successfully paused containers for cleanup
			pausedContainerIds = append(pausedContainerIds, containerID)
		}
	}

	// Step 3: Commit all containers
	fmt.Println("\n=== Step 3: Commit all containers ===")
	committedImages := make(map[string]string) // Maps container name to committed image URI
	commitErrors := 0
	for _, spec := range containerSpecs {
		containerID := containerMap[spec.Name]
		if err := commitContainer(containerID, spec.URI); err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: Failed to commit container '%s': %v\n", spec.Name, err)
			commitErrors++
		} else {
			committedImages[spec.Name] = spec.URI
			fmt.Printf("Successfully committed: %s -> %s\n", containerID, spec.URI)
		}
	}

	// Step 4: Resume all paused containers (regardless of commit success/failure)
	fmt.Println("\n=== Step 4: Resume all paused containers ===")
	resumeAllPausedContainers()

	// If there were commit errors, exit with failure after cleanup
	if commitErrors > 0 {
		fmt.Fprintf(os.Stderr, "ERROR: %d container(s) failed to commit. All containers have been resumed.\n", commitErrors)
		os.Exit(1)
	}

	// Step 5: Push all committed images
	fmt.Println("\n=== Step 5: Push all images ===")
	pushErrors := 0
	for _, spec := range containerSpecs {
		if _, ok := committedImages[spec.Name]; ok {
			if err := pushImage(spec.URI); err != nil {
				fmt.Fprintf(os.Stderr, "ERROR: Failed to push image for container '%s': %v\n", spec.Name, err)
				pushErrors++
			} else {
				fmt.Printf("Successfully pushed: %s\n", spec.URI)
			}
		}
	}

	if pushErrors > 0 {
		fmt.Fprintf(os.Stderr, "ERROR: %d image(s) failed to push.\n", pushErrors)
		os.Exit(1)
	}

	// Step 6: Extract digests and output results
	fmt.Println("\n=== Step 6: Extract digests ===")
	digests := make(map[string]string) // Maps container name to digest
	firstDigest := ""

	for _, spec := range containerSpecs {
		if _, ok := committedImages[spec.Name]; ok {
			digest, err := getImageDigest(spec.URI)
			if err != nil {
				fmt.Fprintf(os.Stderr, "ERROR: Failed to extract digest for %s: %v\n", spec.URI, err)
				os.Exit(1)
			}

			digests[spec.Name] = digest
			fmt.Printf("Container '%s' digest: %s\n", spec.Name, digest)

			// Capture first digest for legacy output
			if firstDigest == "" {
				firstDigest = digest
			}
		}
	}

	// Final output - SNAPSHOT_DIGEST_ variables for each container
	fmt.Println("\n=== Snapshot completed successfully ===")
	for _, spec := range containerSpecs {
		if digest, ok := digests[spec.Name]; ok {
			upperName := strings.ToUpper(strings.ReplaceAll(spec.Name, "-", "_"))
			fmt.Printf("SNAPSHOT_DIGEST_%s=%s\n", upperName, digest)
			fmt.Printf("  Image: %s\n", spec.URI)
			fmt.Printf("  Digest: %s\n", digest)
		}
	}

	if err := writeSnapshotResult(containerSpecs, digests); err != nil {
		fmt.Fprintf(os.Stderr, "WARNING: Failed to write snapshot result to termination message: %v\n", err)
	}

	// Legacy single-digest output for backward compatibility
	fmt.Printf("SNAPSHOT_DIGEST=%s\n", firstDigest)
}

func writeSnapshotResult(containerSpecs []ContainerSpec, digests map[string]string) error {
	result := snapshotResult{
		Containers: make([]snapshotContainerResult, 0, len(digests)),
	}
	for _, spec := range containerSpecs {
		digest, ok := digests[spec.Name]
		if !ok {
			continue
		}
		result.Containers = append(result.Containers, snapshotContainerResult{
			Name:   spec.Name,
			Image:  spec.URI,
			Digest: digest,
		})
	}
	data, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return os.WriteFile(terminationMessagePath, append(data, '\n'), 0644)
}

func runUnpause(args []string) {
	if len(args) < 3 {
		fmt.Fprintln(os.Stderr, "ERROR: Missing required parameters")
		fmt.Fprintln(os.Stderr, "Usage: image-committer unpause <pod_name> <namespace> <container_name> [container_name...]")
		os.Exit(1)
	}

	podName := args[0]
	namespace := args[1]
	containerNames := args[2:]
	errors := 0

	for _, containerName := range containerNames {
		containerID, err := getContainerIDByNerdctl(podName, namespace, containerName)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: Failed to find container '%s': %v\n", containerName, err)
			errors++
			continue
		}
		if err := resumeContainer(containerID); err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: Failed to unpause container '%s': %v\n", containerName, err)
			errors++
		}
	}

	if errors > 0 {
		os.Exit(1)
	}
}

// parseContainerSpec parses a "container:uri" string into ContainerSpec
func parseContainerSpec(specStr string) (ContainerSpec, error) {
	parts := strings.SplitN(specStr, ":", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return ContainerSpec{}, fmt.Errorf("invalid container spec '%s'. Expected format: container_name:uri", specStr)
	}

	return ContainerSpec{
		Name: parts[0],
		URI:  parts[1],
	}, nil
}

// getContainerIDByNerdctl finds a container ID using nerdctl ps with Kubernetes labels.
// This approach directly queries containerd (k8s.io namespace) without going through
// the CRI API, making it compatible with all containerd versions.
// Kubernetes injects standard labels on all containers:
//   - io.kubernetes.pod.name
//   - io.kubernetes.pod.namespace
//   - io.kubernetes.container.name
func getContainerIDByNerdctl(podName, podNamespace, containerName string) (string, error) {
	// Use nerdctl ps with label filters to find the container directly
	args := append(nerdctlBaseArgs(),
		"ps", "-q",
		"--filter", fmt.Sprintf("label=io.kubernetes.pod.name=%s", podName),
		"--filter", fmt.Sprintf("label=io.kubernetes.pod.namespace=%s", podNamespace),
		"--filter", fmt.Sprintf("label=io.kubernetes.container.name=%s", containerName),
	)
	cmd := exec.Command("nerdctl", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("nerdctl ps failed for pod=%s ns=%s container=%s: %v, output: %s",
			podName, podNamespace, containerName, err, strings.TrimSpace(string(output)))
	}

	containerID := strings.TrimSpace(string(output))
	if containerID == "" {
		return "", fmt.Errorf("container '%s' not found in pod %s/%s (nerdctl ps returned empty)",
			containerName, podNamespace, podName)
	}

	// nerdctl ps -q may return multiple lines; take the first (most recently started)
	lines := strings.Split(containerID, "\n")
	return strings.TrimSpace(lines[0]), nil
}

// pauseContainer uses nerdctl to pause a container
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

// resumeContainer uses nerdctl to resume a container
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

// resumeAllPausedContainers resumes all paused containers that were tracked
func resumeAllPausedContainers() {
	if len(pausedContainerIds) == 0 {
		return
	}

	fmt.Println("\n=== Cleanup: Resuming all paused containers ===")

	// Process in reverse order to match pause order
	for i := len(pausedContainerIds) - 1; i >= 0; i-- {
		containerID := pausedContainerIds[i]
		err := resumeContainer(containerID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "WARNING: Could not resume container %s: %v\n", containerID, err)
		}
	}

	// Clear the paused containers list after cleanup
	pausedContainerIds = []string{}
}

// commitContainer uses nerdctl to commit a container to an image
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
	host := strings.Split(registryHost, ":")[0]
	parts := strings.Split(host, ".")
	if len(parts) < 2 || parts[0] != "172" {
		return false
	}
	secondOctet, err := strconv.Atoi(parts[1])
	if err != nil {
		return false
	}
	return secondOctet >= 16 && secondOctet <= 31
}

// getImageDigest uses nerdctl to get the digest of the image
func getImageDigest(imageRef string) (string, error) {
	args := append(nerdctlBaseArgs(), "inspect", "--format", "{{.Id}}", imageRef)
	output, err := commandCombinedOutput("nerdctl", args...)
	if err != nil {
		return "", fmt.Errorf("nerdctl inspect failed for image %s: %w, output: %s", imageRef, err, strings.TrimSpace(string(output)))
	}
	digest := strings.TrimSpace(string(output))
	if digest == "" {
		return "", fmt.Errorf("nerdctl inspect returned empty digest for image %s", imageRef)
	}
	return digest, nil
}
