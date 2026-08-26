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

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	containerd "github.com/containerd/containerd"

	"github.com/alibaba/OpenSandbox/sandbox-k8s/pkg/imagecommitter"
)

const (
	defaultContainerdSocket    = "/run/containerd/containerd.sock"
	defaultContainerdNamespace = "k8s.io"
)

// Config supplies implementation-specific dependencies while preserving the
// common commit and unpause CLI contract.
type Config struct {
	CredentialProvider       imagecommitter.CredentialProvider
	SourceCredentialProvider imagecommitter.CredentialProvider
	TerminationMessagePath   string
	Output                   io.Writer
	ErrorOutput              io.Writer
}

// Run executes a commit or unpause operation.
func Run(ctx context.Context, args []string, config Config) error {
	if config.Output == nil {
		config.Output = io.Discard
	}
	if config.ErrorOutput == nil {
		config.ErrorOutput = io.Discard
	}
	operation, commitRequest, unpauseRequest, err := parseOperation(args)
	if err != nil {
		return err
	}

	client, err := containerd.New(
		containerdSocket(),
		containerd.WithDefaultNamespace(containerdNamespace()),
	)
	if err != nil {
		return fmt.Errorf("connect to containerd: %w", err)
	}
	defer client.Close()

	runtime := imagecommitter.NewContainerdRuntime(client)
	orchestrator := &imagecommitter.Orchestrator{
		Runtime:     runtime,
		Executor:    runtime,
		Output:      config.Output,
		ErrorOutput: config.ErrorOutput,
	}
	if operation == "unpause" {
		return orchestrator.Unpause(ctx, unpauseRequest)
	}

	// Preserve the existing best-effort preparation behavior. It remains an
	// implementation detail and is not part of the executable contract.
	orchestrator.PreparationCommand = []string{"sync"}
	orchestrator.Builder = imagecommitter.NewContainerdImageBuilder(
		client,
		config.SourceCredentialProvider,
		func(source string) bool { return shouldUseInsecureSourceRegistry(source, config.ErrorOutput) },
	)
	orchestrator.Pusher = imagecommitter.NewContainerdImagePusher(
		client,
		config.CredentialProvider,
		func(target string) bool { return shouldUseInsecureRegistry(target, config.ErrorOutput) },
	)
	result, err := orchestrator.Commit(ctx, commitRequest)
	if err != nil {
		return err
	}
	if err := writeResult(config.TerminationMessagePath, result); err != nil {
		return fmt.Errorf("write snapshot result: %w", err)
	}
	for _, container := range result.Containers {
		name := strings.ToUpper(strings.ReplaceAll(container.Name, "-", "_"))
		fmt.Fprintf(config.Output, "SNAPSHOT_DIGEST_%s=%s\n", name, container.Digest)
	}
	if len(result.Containers) > 0 {
		fmt.Fprintf(config.Output, "SNAPSHOT_DIGEST=%s\n", result.Containers[0].Digest)
	}
	return nil
}

func parseOperation(args []string) (string, imagecommitter.CommitRequest, imagecommitter.UnpauseRequest, error) {
	podUID := strings.TrimSpace(os.Getenv("SOURCE_POD_UID"))
	if len(args) > 0 && args[0] == "unpause" {
		if len(args) < 4 {
			return "", imagecommitter.CommitRequest{}, imagecommitter.UnpauseRequest{}, errors.New("usage: image-committer unpause <pod_name> <namespace> <container_name> [container_name...]")
		}
		return "unpause", imagecommitter.CommitRequest{}, imagecommitter.UnpauseRequest{
			PodName:        args[1],
			Namespace:      args[2],
			PodUID:         podUID,
			ContainerNames: append([]string(nil), args[3:]...),
		}, nil
	}
	if len(args) < 3 {
		return "", imagecommitter.CommitRequest{}, imagecommitter.UnpauseRequest{}, errors.New("usage: image-committer <pod_name> <namespace> <container_name>:<target_image> [<container_name>:<target_image>...]")
	}
	request := imagecommitter.CommitRequest{PodName: args[0], Namespace: args[1], PodUID: podUID}
	for _, raw := range args[2:] {
		spec, err := parseContainerSpec(raw)
		if err != nil {
			return "", imagecommitter.CommitRequest{}, imagecommitter.UnpauseRequest{}, err
		}
		request.Containers = append(request.Containers, spec)
	}
	if request.PodName == "" || request.Namespace == "" {
		return "", imagecommitter.CommitRequest{}, imagecommitter.UnpauseRequest{}, errors.New("pod name and namespace are required")
	}
	return "commit", request, imagecommitter.UnpauseRequest{}, nil
}

func parseContainerSpec(raw string) (imagecommitter.ContainerSpec, error) {
	parts := strings.SplitN(raw, ":", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return imagecommitter.ContainerSpec{}, fmt.Errorf("invalid container spec %q; expected container_name:target_image", raw)
	}
	return imagecommitter.ContainerSpec{Name: parts[0], Target: parts[1]}, nil
}

func writeResult(path string, result imagecommitter.Result) error {
	if path == "" {
		return errors.New("termination message path is required")
	}
	data, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func containerdSocket() string {
	if value := strings.TrimSpace(os.Getenv("CONTAINERD_SOCKET")); value != "" {
		return value
	}
	return defaultContainerdSocket
}

func containerdNamespace() string {
	if value := strings.TrimSpace(os.Getenv("CONTAINERD_NAMESPACE")); value != "" {
		return value
	}
	return defaultContainerdNamespace
}

func shouldUseInsecureRegistry(targetImage string, errorOutput io.Writer) bool {
	return shouldUseInsecureRegistryEnv(targetImage, "SNAPSHOT_REGISTRY_INSECURE", errorOutput)
}

func shouldUseInsecureSourceRegistry(_ string, errorOutput io.Writer) bool {
	raw := strings.TrimSpace(os.Getenv("SOURCE_IMAGE_REGISTRY_INSECURE"))
	if raw == "" {
		return false
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		fmt.Fprintf(errorOutput, "WARNING: invalid SOURCE_IMAGE_REGISTRY_INSECURE=%q; using secure transport\n", raw)
		return false
	}
	return value
}

func shouldUseInsecureRegistryEnv(imageReference, environmentVariable string, errorOutput io.Writer) bool {
	if raw := strings.TrimSpace(os.Getenv(environmentVariable)); raw != "" {
		value, err := strconv.ParseBool(raw)
		if err == nil {
			return value
		}
		fmt.Fprintf(errorOutput, "WARNING: invalid %s=%q; using host compatibility heuristic\n", environmentVariable, raw)
	}
	registryHost := strings.SplitN(imageReference, "/", 2)[0]
	return strings.Contains(registryHost, "local") ||
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
