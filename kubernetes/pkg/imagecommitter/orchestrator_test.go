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
	"reflect"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

type fakeRuntime struct {
	states       map[string]TaskState
	resolveError map[string]error
	operations   *[]string
}

func (f *fakeRuntime) Resolve(_ context.Context, selector ContainerSelector) (ResolvedContainer, error) {
	if err := f.resolveError[selector.ContainerName]; err != nil {
		return ResolvedContainer{}, err
	}
	state, ok := f.states[selector.ContainerName]
	if !ok {
		return ResolvedContainer{}, errors.New("not found")
	}
	*f.operations = append(*f.operations, "resolve:"+selector.ContainerName)
	return ResolvedContainer{ID: selector.ContainerName + "-id", Name: selector.ContainerName, State: state}, nil
}

func (f *fakeRuntime) Status(_ context.Context, container ResolvedContainer) (TaskState, error) {
	return f.states[container.Name], nil
}

func (f *fakeRuntime) Pause(_ context.Context, container ResolvedContainer) (PauseHandle, error) {
	*f.operations = append(*f.operations, "pause:"+container.Name)
	if f.states[container.Name] == TaskStateRunning {
		f.states[container.Name] = TaskStatePaused
		return PauseHandle{Container: container, PausedByUs: true}, nil
	}
	return PauseHandle{Container: container}, nil
}

func (f *fakeRuntime) Resume(_ context.Context, container ResolvedContainer) error {
	*f.operations = append(*f.operations, "resume:"+container.Name)
	f.states[container.Name] = TaskStateRunning
	return nil
}

type fakeBuilder struct {
	operations *[]string
	fail       string
	panicFor   string
}

func (f fakeBuilder) Commit(_ context.Context, container ResolvedContainer, target string) (LocalImage, error) {
	*f.operations = append(*f.operations, "commit:"+container.Name)
	if container.Name == f.panicFor {
		panic("commit panic")
	}
	if container.Name == f.fail {
		return LocalImage{}, errors.New("commit failed")
	}
	return LocalImage{
		Reference: target,
		Target:    ocispec.Descriptor{Digest: digest.FromString("manifest:" + target)},
		Config:    ocispec.Descriptor{Digest: digest.FromString("config:" + target)},
	}, nil
}

type fakePusher struct {
	operations *[]string
	runtime    *fakeRuntime
}

func (f fakePusher) Push(_ context.Context, image LocalImage) (ocispec.Descriptor, error) {
	for name, state := range f.runtime.states {
		if state == TaskStatePaused {
			return ocispec.Descriptor{}, fmt.Errorf("container %s was still paused during push", name)
		}
	}
	*f.operations = append(*f.operations, "push:"+image.Reference)
	return image.Target, nil
}

func TestCommitResolvesBeforePauseAndResumesBeforePush(t *testing.T) {
	var operations []string
	runtime := &fakeRuntime{
		states:       map[string]TaskState{"main": TaskStateRunning, "stopped": TaskStateStopped},
		resolveError: map[string]error{},
		operations:   &operations,
	}
	orchestrator := &Orchestrator{
		Runtime: runtime,
		Builder: fakeBuilder{operations: &operations},
		Pusher:  fakePusher{operations: &operations, runtime: runtime},
	}
	result, err := orchestrator.Commit(context.Background(), CommitRequest{
		PodName: "pod", Namespace: "default",
		Containers: []ContainerSpec{
			{Name: "main", Target: "registry.example.com/main:snap"},
			{Name: "stopped", Target: "registry.example.com/stopped:snap"},
		},
	})
	if err != nil {
		t.Fatalf("Commit failed: %v", err)
	}
	wantOperations := []string{
		"resolve:main", "resolve:stopped",
		"pause:main",
		"commit:main", "commit:stopped",
		"resume:main",
		"push:registry.example.com/main:snap", "push:registry.example.com/stopped:snap",
	}
	if !reflect.DeepEqual(operations, wantOperations) {
		t.Fatalf("operations = %v, want %v", operations, wantOperations)
	}
	if len(result.Containers) != 2 || result.Containers[0].Name != "main" || result.Containers[1].Name != "stopped" {
		t.Fatalf("unexpected result: %#v", result)
	}
	wantDigest := digest.FromString("config:registry.example.com/main:snap").String()
	if result.Containers[0].Digest != wantDigest {
		t.Fatalf("reported digest = %q, want config digest %q", result.Containers[0].Digest, wantDigest)
	}
	manifestDigest := digest.FromString("manifest:registry.example.com/main:snap").String()
	if result.Containers[0].Digest == manifestDigest {
		t.Fatalf("reported digest unexpectedly used manifest digest %q", manifestDigest)
	}
}

func TestCommitResumesOwnedContainersAfterBuildFailure(t *testing.T) {
	var operations []string
	runtime := &fakeRuntime{
		states:       map[string]TaskState{"main": TaskStateRunning},
		resolveError: map[string]error{},
		operations:   &operations,
	}
	orchestrator := &Orchestrator{
		Runtime: runtime,
		Builder: fakeBuilder{operations: &operations, fail: "main"},
		Pusher:  fakePusher{operations: &operations, runtime: runtime},
	}
	_, err := orchestrator.Commit(context.Background(), CommitRequest{
		PodName: "pod", Namespace: "default",
		Containers: []ContainerSpec{{Name: "main", Target: "registry.example.com/main:snap"}},
	})
	if err == nil {
		t.Fatal("expected build failure")
	}
	if runtime.states["main"] != TaskStateRunning {
		t.Fatalf("container was not resumed: %s", runtime.states["main"])
	}
}

func TestCommitResumesOwnedContainersAfterProviderPanic(t *testing.T) {
	var operations []string
	runtime := &fakeRuntime{
		states:       map[string]TaskState{"main": TaskStateRunning},
		resolveError: map[string]error{},
		operations:   &operations,
	}
	orchestrator := &Orchestrator{
		Runtime: runtime,
		Builder: fakeBuilder{operations: &operations, panicFor: "main"},
		Pusher:  fakePusher{operations: &operations, runtime: runtime},
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected provider panic")
			}
		}()
		_, _ = orchestrator.Commit(context.Background(), CommitRequest{
			PodName: "pod", Namespace: "default",
			Containers: []ContainerSpec{{Name: "main", Target: "registry.example.com/main:snap"}},
		})
	}()
	if runtime.states["main"] != TaskStateRunning {
		t.Fatalf("container was not resumed after panic: %s", runtime.states["main"])
	}
}

func TestUnpauseIsIdempotentAndContinuesAfterErrors(t *testing.T) {
	var operations []string
	runtime := &fakeRuntime{
		states: map[string]TaskState{
			"paused":  TaskStatePaused,
			"running": TaskStateRunning,
			"stopped": TaskStateStopped,
		},
		resolveError: map[string]error{"missing": errors.New("not found")},
		operations:   &operations,
	}
	orchestrator := &Orchestrator{Runtime: runtime}
	err := orchestrator.Unpause(context.Background(), UnpauseRequest{
		PodName: "pod", Namespace: "default",
		ContainerNames: []string{"missing", "paused", "running", "stopped"},
	})
	if err == nil {
		t.Fatal("expected aggregated missing-container error")
	}
	if runtime.states["paused"] != TaskStateRunning {
		t.Fatal("paused container was not resumed")
	}
	if !reflect.DeepEqual(operations, []string{"resolve:paused", "resume:paused", "resolve:running", "resolve:stopped"}) {
		t.Fatalf("unexpected operations: %v", operations)
	}
}
