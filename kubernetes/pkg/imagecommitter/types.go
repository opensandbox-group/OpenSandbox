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

package imagecommitter

import (
	"context"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	PodNameLabel       = "io.kubernetes.pod.name"
	PodNamespaceLabel  = "io.kubernetes.pod.namespace"
	PodUIDLabel        = "io.kubernetes.pod.uid"
	ContainerNameLabel = "io.kubernetes.container.name"
)

// ContainerSpec maps a source container to its target image.
type ContainerSpec struct {
	Name   string
	Target string
}

// ContainerSelector identifies a Kubernetes container in containerd metadata.
type ContainerSelector struct {
	PodName       string
	PodNamespace  string
	PodUID        string
	ContainerName string
}

// TaskState is the runtime state relevant to commit and unpause operations.
type TaskState string

const (
	TaskStateUnknown TaskState = "unknown"
	TaskStateRunning TaskState = "running"
	TaskStatePaused  TaskState = "paused"
	TaskStateStopped TaskState = "stopped"
)

// ResolvedContainer contains runtime metadata needed by providers.
type ResolvedContainer struct {
	ID          string
	Name        string
	State       TaskState
	Snapshotter string
	SnapshotKey string
	SourceImage string
}

// PauseHandle records whether this invocation owns a pause transition.
type PauseHandle struct {
	Container  ResolvedContainer
	PausedByUs bool
}

// ExecRequest describes an optional command executed in a source container.
type ExecRequest struct {
	Args []string
}

// ExecResult is the result of a successfully created exec process.
type ExecResult struct {
	ExitCode uint32
}

// LocalImage identifies image content assembled in containerd.
type LocalImage struct {
	Reference string
	// Target is the manifest descriptor pushed to the registry.
	Target ocispec.Descriptor
	// Config is the image config descriptor reported as the snapshot image
	// digest for compatibility with the previous image-committer behavior.
	Config ocispec.Descriptor
}

// RegistryCredential supports standard OCI Distribution authentication forms.
type RegistryCredential struct {
	Username     string
	Password     string
	AccessToken  string
	RefreshToken string
}

// ContainerResult is written to the Kubernetes termination message.
type ContainerResult struct {
	Name  string `json:"name"`
	Image string `json:"image"`
	// Digest is the image config digest, preserving the image ID semantics of
	// the previous image-committer implementation.
	Digest string `json:"digest"`
}

// Result is the stable commit output contract.
type Result struct {
	Containers []ContainerResult `json:"containers"`
}

// CommitRequest is the parsed commit operation input.
type CommitRequest struct {
	PodName    string
	Namespace  string
	PodUID     string
	Containers []ContainerSpec
}

// UnpauseRequest is the parsed unpause operation input.
type UnpauseRequest struct {
	PodName        string
	Namespace      string
	PodUID         string
	ContainerNames []string
}

// ContainerRuntime abstracts container lookup and task lifecycle operations.
type ContainerRuntime interface {
	Resolve(context.Context, ContainerSelector) (ResolvedContainer, error)
	Status(context.Context, ResolvedContainer) (TaskState, error)
	Pause(context.Context, ResolvedContainer) (PauseHandle, error)
	Resume(context.Context, ResolvedContainer) error
}

// ContainerExecutor optionally runs preparation commands in source containers.
type ContainerExecutor interface {
	Exec(context.Context, ResolvedContainer, ExecRequest) (ExecResult, error)
}

// ImageBuilder creates local image content without contacting a registry.
type ImageBuilder interface {
	Commit(context.Context, ResolvedContainer, string) (LocalImage, error)
}

// CredentialProvider resolves credentials only for the requested registry host.
type CredentialProvider interface {
	Credential(context.Context, string) (RegistryCredential, error)
}

// ImagePusher uploads local image content and returns its pushed descriptor.
type ImagePusher interface {
	Push(context.Context, LocalImage) (ocispec.Descriptor, error)
}
