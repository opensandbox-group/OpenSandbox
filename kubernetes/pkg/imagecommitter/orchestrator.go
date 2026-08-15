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
	"errors"
	"fmt"
	"io"
	"time"
)

// Orchestrator applies the stable commit and unpause operation ordering.
type Orchestrator struct {
	Runtime            ContainerRuntime
	Executor           ContainerExecutor
	Builder            ImageBuilder
	Pusher             ImagePusher
	PreparationCommand []string
	Output             io.Writer
	ErrorOutput        io.Writer
}

func (o *Orchestrator) Commit(ctx context.Context, request CommitRequest) (Result, error) {
	if err := o.validate(); err != nil {
		return Result{}, err
	}
	if request.PodName == "" || request.Namespace == "" || len(request.Containers) == 0 {
		return Result{}, errors.New("pod name, namespace, and at least one container are required")
	}

	containers := make([]ResolvedContainer, 0, len(request.Containers))
	for _, spec := range request.Containers {
		container, err := o.Runtime.Resolve(ctx, ContainerSelector{
			PodName:       request.PodName,
			PodNamespace:  request.Namespace,
			PodUID:        request.PodUID,
			ContainerName: spec.Name,
		})
		if err != nil {
			return Result{}, fmt.Errorf("resolve container %q: %w", spec.Name, err)
		}
		containers = append(containers, container)
		o.logf("Resolved container %s as %s (%s)\n", spec.Name, container.ID, container.State)
	}

	if o.Executor != nil && len(o.PreparationCommand) > 0 {
		for _, container := range containers {
			if container.State != TaskStateRunning {
				continue
			}
			result, err := o.Executor.Exec(ctx, container, ExecRequest{Args: o.PreparationCommand})
			if err != nil {
				o.errorf("WARNING: preparation command failed for container %s: %v\n", container.Name, err)
				continue
			}
			if result.ExitCode != 0 {
				o.errorf("WARNING: preparation command exited with code %d for container %s\n", result.ExitCode, container.Name)
			}
		}
	}

	pauseHandles := make([]PauseHandle, 0, len(containers))
	cleanupComplete := false
	defer func() {
		if !cleanupComplete {
			_ = o.resumeOwned(pauseHandles)
		}
	}()
	for _, container := range containers {
		if container.State == TaskStateStopped {
			continue
		}
		handle, err := o.Runtime.Pause(ctx, container)
		if err != nil {
			// Preserve the current best-effort pause behavior. The image build can
			// still succeed for a task that stopped between resolve and pause.
			o.errorf("WARNING: could not pause container %s: %v\n", container.Name, err)
			continue
		}
		if handle.PausedByUs {
			pauseHandles = append(pauseHandles, handle)
		}
	}

	localImages := make([]LocalImage, len(containers))
	var buildErrors []error
	for i, container := range containers {
		image, err := o.Builder.Commit(ctx, container, request.Containers[i].Target)
		if err != nil {
			buildErrors = append(buildErrors, fmt.Errorf("commit container %q: %w", container.Name, err))
			continue
		}
		localImages[i] = image
		o.logf("Committed container %s to %s\n", container.Name, image.Reference)
	}

	resumeErr := o.resumeOwned(pauseHandles)
	cleanupComplete = true
	if len(buildErrors) > 0 || resumeErr != nil {
		return Result{}, errors.Join(append(buildErrors, resumeErr)...)
	}

	result := Result{Containers: make([]ContainerResult, 0, len(localImages))}
	var pushErrors []error
	for i, image := range localImages {
		if image.Config.Digest == "" {
			pushErrors = append(pushErrors, fmt.Errorf("push container %q: committed image has no config digest", request.Containers[i].Name))
			continue
		}
		descriptor, err := o.Pusher.Push(ctx, image)
		if err != nil {
			pushErrors = append(pushErrors, fmt.Errorf("push container %q: %w", request.Containers[i].Name, err))
			continue
		}
		result.Containers = append(result.Containers, ContainerResult{
			Name:   request.Containers[i].Name,
			Image:  image.Reference,
			Digest: image.Config.Digest.String(),
		})
		o.logf("Pushed image %s (manifest %s, config %s)\n", image.Reference, descriptor.Digest, image.Config.Digest)
	}
	if len(pushErrors) > 0 {
		return Result{}, errors.Join(pushErrors...)
	}
	return result, nil
}

func (o *Orchestrator) Unpause(ctx context.Context, request UnpauseRequest) error {
	if o.Runtime == nil {
		return errors.New("container runtime is required")
	}
	if request.PodName == "" || request.Namespace == "" || len(request.ContainerNames) == 0 {
		return errors.New("pod name, namespace, and at least one container are required")
	}

	var errs []error
	for _, name := range request.ContainerNames {
		container, err := o.Runtime.Resolve(ctx, ContainerSelector{
			PodName:       request.PodName,
			PodNamespace:  request.Namespace,
			PodUID:        request.PodUID,
			ContainerName: name,
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("resolve container %q: %w", name, err))
			continue
		}
		state, err := o.Runtime.Status(ctx, container)
		if err != nil {
			errs = append(errs, fmt.Errorf("inspect container %q: %w", name, err))
			continue
		}
		switch state {
		case TaskStateRunning:
			o.logf("Container %s is already running\n", name)
		case TaskStateStopped:
			o.logf("Container %s is stopped; no unpause is required\n", name)
		case TaskStatePaused:
			if err := o.Runtime.Resume(ctx, container); err != nil {
				errs = append(errs, fmt.Errorf("unpause container %q: %w", name, err))
				continue
			}
			o.logf("Unpaused container %s\n", name)
		default:
			errs = append(errs, fmt.Errorf("container %q has unsupported state %s", name, state))
		}
	}
	return errors.Join(errs...)
}

func (o *Orchestrator) resumeOwned(handles []PauseHandle) error {
	var errs []error
	for i := len(handles) - 1; i >= 0; i-- {
		if !handles[i].PausedByUs {
			continue
		}
		// Give every container an independent cleanup window so one stuck task
		// cannot prevent attempts to resume the remaining tasks.
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := o.Runtime.Resume(cleanupCtx, handles[i].Container)
		cancel()
		if err != nil {
			errs = append(errs, fmt.Errorf("resume container %s: %w", handles[i].Container.ID, err))
		}
	}
	return errors.Join(errs...)
}

func (o *Orchestrator) validate() error {
	if o.Runtime == nil || o.Builder == nil || o.Pusher == nil {
		return errors.New("runtime, image builder, and image pusher are required")
	}
	return nil
}

func (o *Orchestrator) logf(format string, args ...any) {
	if o.Output != nil {
		fmt.Fprintf(o.Output, format, args...)
	}
}

func (o *Orchestrator) errorf(format string, args ...any) {
	if o.ErrorOutput != nil {
		fmt.Fprintf(o.ErrorOutput, format, args...)
	}
}
