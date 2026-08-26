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
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"syscall"
	"time"

	containerd "github.com/containerd/containerd"
	"github.com/containerd/containerd/cio"
	"github.com/containerd/errdefs"
)

// ContainerdRuntime implements runtime and exec operations through containerd.
type ContainerdRuntime struct {
	client *containerd.Client
}

func NewContainerdRuntime(client *containerd.Client) *ContainerdRuntime {
	return &ContainerdRuntime{client: client}
}

func (r *ContainerdRuntime) Resolve(ctx context.Context, selector ContainerSelector) (ResolvedContainer, error) {
	containers, err := r.client.Containers(ctx)
	if err != nil {
		return ResolvedContainer{}, fmt.Errorf("list containerd containers: %w", err)
	}

	type candidate struct {
		container ResolvedContainer
		createdAt time.Time
	}
	var candidates []candidate
	for _, c := range containers {
		info, err := c.Info(ctx)
		if err != nil {
			if errdefs.IsNotFound(err) {
				// A container can be removed between list and inspect.
				continue
			}
			return ResolvedContainer{}, fmt.Errorf("inspect container %s: %w", c.ID(), err)
		}
		labels := info.Labels
		if labels[PodNameLabel] != selector.PodName ||
			labels[PodNamespaceLabel] != selector.PodNamespace ||
			labels[ContainerNameLabel] != selector.ContainerName {
			continue
		}
		if selector.PodUID != "" && labels[PodUIDLabel] != selector.PodUID {
			continue
		}

		resolved := ResolvedContainer{
			ID:          c.ID(),
			Name:        selector.ContainerName,
			Snapshotter: info.Snapshotter,
			SnapshotKey: info.SnapshotKey,
			SourceImage: info.Image,
		}
		resolved.State, err = r.Status(ctx, resolved)
		if err != nil {
			return ResolvedContainer{}, err
		}
		candidates = append(candidates, candidate{container: resolved, createdAt: info.CreatedAt})
	}

	if len(candidates) == 0 {
		return ResolvedContainer{}, fmt.Errorf("container %q not found in pod %s/%s", selector.ContainerName, selector.PodNamespace, selector.PodName)
	}

	var active []candidate
	for _, candidate := range candidates {
		if candidate.container.State == TaskStateRunning || candidate.container.State == TaskStatePaused {
			active = append(active, candidate)
		}
	}
	if len(active) == 1 {
		return active[0].container, nil
	}
	if len(active) > 1 {
		return ResolvedContainer{}, fmt.Errorf("container %q in pod %s/%s is ambiguous: %d active matches", selector.ContainerName, selector.PodNamespace, selector.PodName, len(active))
	}
	if len(candidates) > 1 {
		sort.Slice(candidates, func(i, j int) bool { return candidates[i].createdAt.After(candidates[j].createdAt) })
		if candidates[0].createdAt.Equal(candidates[1].createdAt) {
			return ResolvedContainer{}, fmt.Errorf("container %q in pod %s/%s is ambiguous: %d stopped matches", selector.ContainerName, selector.PodNamespace, selector.PodName, len(candidates))
		}
	}
	return candidates[0].container, nil
}

func (r *ContainerdRuntime) Status(ctx context.Context, container ResolvedContainer) (TaskState, error) {
	c, err := r.client.LoadContainer(ctx, container.ID)
	if err != nil {
		return TaskStateUnknown, fmt.Errorf("load container %s: %w", container.ID, err)
	}
	task, err := c.Task(ctx, nil)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return TaskStateStopped, nil
		}
		return TaskStateUnknown, fmt.Errorf("load task for container %s: %w", container.ID, err)
	}
	status, err := task.Status(ctx)
	if err != nil {
		return TaskStateUnknown, fmt.Errorf("get task status for container %s: %w", container.ID, err)
	}
	switch status.Status {
	case containerd.Running, containerd.Created:
		return TaskStateRunning, nil
	case containerd.Paused, containerd.Pausing:
		return TaskStatePaused, nil
	case containerd.Stopped:
		return TaskStateStopped, nil
	default:
		return TaskStateUnknown, nil
	}
}

func (r *ContainerdRuntime) Pause(ctx context.Context, container ResolvedContainer) (PauseHandle, error) {
	state, err := r.Status(ctx, container)
	if err != nil {
		return PauseHandle{}, err
	}
	handle := PauseHandle{Container: container}
	switch state {
	case TaskStatePaused, TaskStateStopped:
		return handle, nil
	case TaskStateRunning:
	default:
		return PauseHandle{}, fmt.Errorf("cannot pause container %s in state %s", container.ID, state)
	}

	c, err := r.client.LoadContainer(ctx, container.ID)
	if err != nil {
		return PauseHandle{}, fmt.Errorf("load container %s: %w", container.ID, err)
	}
	task, err := c.Task(ctx, nil)
	if err != nil {
		return PauseHandle{}, fmt.Errorf("load task for container %s: %w", container.ID, err)
	}
	if err := task.Pause(ctx); err != nil {
		return PauseHandle{}, fmt.Errorf("pause container %s: %w", container.ID, err)
	}
	handle.PausedByUs = true
	return handle, nil
}

func (r *ContainerdRuntime) Resume(ctx context.Context, container ResolvedContainer) error {
	state, err := r.Status(ctx, container)
	if err != nil {
		return err
	}
	if state == TaskStateRunning || state == TaskStateStopped {
		return nil
	}
	if state != TaskStatePaused {
		return fmt.Errorf("cannot resume container %s in state %s", container.ID, state)
	}

	c, err := r.client.LoadContainer(ctx, container.ID)
	if err != nil {
		return fmt.Errorf("load container %s: %w", container.ID, err)
	}
	task, err := c.Task(ctx, nil)
	if err != nil {
		return fmt.Errorf("load task for container %s: %w", container.ID, err)
	}
	if err := task.Resume(ctx); err != nil {
		return fmt.Errorf("resume container %s: %w", container.ID, err)
	}
	state, err = r.Status(ctx, container)
	if err != nil {
		return err
	}
	if state != TaskStateRunning {
		return fmt.Errorf("container %s remained in state %s after resume", container.ID, state)
	}
	return nil
}

func (r *ContainerdRuntime) Exec(ctx context.Context, container ResolvedContainer, request ExecRequest) (result ExecResult, retErr error) {
	if len(request.Args) == 0 {
		return ExecResult{}, errors.New("exec arguments are required")
	}
	state, err := r.Status(ctx, container)
	if err != nil {
		return ExecResult{}, err
	}
	if state != TaskStateRunning {
		return ExecResult{}, fmt.Errorf("cannot exec in container %s in state %s", container.ID, state)
	}

	c, err := r.client.LoadContainer(ctx, container.ID)
	if err != nil {
		return ExecResult{}, fmt.Errorf("load container %s: %w", container.ID, err)
	}
	spec, err := c.Spec(ctx)
	if err != nil {
		return ExecResult{}, fmt.Errorf("load container spec %s: %w", container.ID, err)
	}
	if spec.Process == nil {
		return ExecResult{}, fmt.Errorf("container %s has no process spec", container.ID)
	}
	processSpec := *spec.Process
	processSpec.Args = append([]string(nil), request.Args...)
	processSpec.CommandLine = ""
	processSpec.Terminal = false

	task, err := c.Task(ctx, nil)
	if err != nil {
		return ExecResult{}, fmt.Errorf("load task for container %s: %w", container.ID, err)
	}
	execID, err := randomID()
	if err != nil {
		return ExecResult{}, err
	}
	process, err := task.Exec(ctx, execID, &processSpec, cio.NullIO)
	if err != nil {
		return ExecResult{}, fmt.Errorf("create exec process for container %s: %w", container.ID, err)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := process.Delete(cleanupCtx, containerd.WithProcessKill)
		if retErr == nil && err != nil && !errdefs.IsNotFound(err) {
			retErr = fmt.Errorf("delete exec process %s: %w", execID, err)
		}
	}()

	exitCh, err := process.Wait(ctx)
	if err != nil {
		return ExecResult{}, fmt.Errorf("wait for exec process %s: %w", execID, err)
	}
	if err := process.Start(ctx); err != nil {
		return ExecResult{}, fmt.Errorf("start exec process %s: %w", execID, err)
	}

	select {
	case status := <-exitCh:
		code, _, err := status.Result()
		if err != nil {
			return ExecResult{}, fmt.Errorf("wait result for exec process %s: %w", execID, err)
		}
		return ExecResult{ExitCode: code}, nil
	case <-ctx.Done():
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = process.Kill(cleanupCtx, syscall.SIGKILL)
		return ExecResult{}, ctx.Err()
	}
}

func randomID() (string, error) {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", fmt.Errorf("generate exec ID: %w", err)
	}
	return "opensandbox-" + hex.EncodeToString(data[:]), nil
}
