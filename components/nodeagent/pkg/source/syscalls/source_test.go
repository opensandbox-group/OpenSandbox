// Copyright 2026 The OpenSandbox Authors
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

package syscalls

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/internal/logger"
	"github.com/alibaba/opensandbox/nodeagent/pkg/api"
	checkpointstate "github.com/alibaba/opensandbox/nodeagent/pkg/state"
	"github.com/alibaba/opensandbox/nodeagent/pkg/store"
)

type fakeTracer struct {
	lost       map[uint64]uint64
	trackErr   error
	trackCalls int
	flushCalls int
}

func (t *fakeTracer) Track(uint64, uint64) error {
	t.trackCalls++
	return t.trackErr
}
func (*fakeTracer) Untrack(uint64) error               { return nil }
func (t *fakeTracer) Flush() error                     { t.flushCalls++; return nil }
func (*fakeTracer) Forget(uint64) error                { return nil }
func (*fakeTracer) Messages() <-chan kernelMessage     { return nil }
func (*fakeTracer) Errors() <-chan error               { return nil }
func (t *fakeTracer) Lost() (map[uint64]uint64, error) { return t.lost, nil }
func (*fakeTracer) Close() error                       { return nil }

type fakeStoreView struct {
	resources []store.Resource
}

func (v *fakeStoreView) List() []store.Resource               { return v.resources }
func (*fakeStoreView) GetByUID(string) (store.Resource, bool) { return store.Resource{}, false }
func (*fakeStoreView) Forget(string)                          {}
func (*fakeStoreView) Changes() <-chan struct{}               { return nil }

func testSourceState(t *testing.T) *checkpointstate.SourceState {
	t.Helper()
	db, err := checkpointstate.Open(t.TempDir(), "target", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	private, err := db.SourceState(sourceName)
	if err != nil {
		t.Fatal(err)
	}
	return private
}

func testAPIResource(podUID string) api.Resource {
	return api.Resource{SandboxID: "sb-1", ClusterName: "cluster", Namespace: "ns", PodName: "pod", PodUID: podUID, NodeName: "node", Container: "sandbox"}
}

func testStream(podUID string) *streamRuntime {
	return &streamRuntime{
		resource:          store.Resource{Resource: testAPIResource(podUID), ContainerID: "container-1", ContainerRuntime: "containerd"},
		coverageStartedAt: time.Unix(1, 0).UTC(),
		outcome:           api.SourceOutcome{HadSourceGaps: true, LossReasons: []string{lossReasonLateAttach}},
	}
}

func TestCollectLossMarksSourceGapOnce(t *testing.T) {
	tracer := &fakeTracer{lost: map[uint64]uint64{7: 3}}
	source := &source{tracer: tracer, state: testSourceState(t), log: logger.MustNew(logger.Config{Level: "error"})}
	stream := testStream("u1")
	stream.handle = 7
	stream.outcome = api.SourceOutcome{}
	binding := &streamBinding{stream: stream}
	streams := map[uint64]*streamBinding{7: binding}
	if err := source.collectLoss(streams); err != nil {
		t.Fatal(err)
	}
	tracer.lost[7] = 5
	if err := source.collectLoss(streams); err != nil {
		t.Fatal(err)
	}
	if !stream.outcome.HadSourceGaps || len(stream.outcome.LossReasons) != 1 || stream.outcome.LossReasons[0] != lossReasonOverflow || binding.lost != 5 {
		t.Fatalf("outcome=%+v lost=%d", stream.outcome, binding.lost)
	}
}

func TestValidateTokenRejectsForeignIdentity(t *testing.T) {
	ref := api.StreamRef{ID: "syscalls/u1/sandbox", Kind: api.RecordKindSyscall}
	if err := validateToken(sourceName, ref, make([]byte, 8)); err != nil {
		t.Fatal(err)
	}
	if err := validateToken("other", ref, make([]byte, 8)); err == nil {
		t.Fatal("foreign Source token was accepted")
	}
}

func TestReconcileRetriesReplacementAfterTrackFailure(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("cgroup inode resolution requires Linux")
	}
	root := t.TempDir()
	resource := store.Resource{
		Resource:         testAPIResource("u1"),
		ContainerRuntime: "containerd",
		ContainerID:      "new-container",
	}
	if err := os.MkdirAll(filepath.Join(root, "podu1", "cri-containerd-new-container.scope"), 0o755); err != nil {
		t.Fatal(err)
	}
	tracer := &fakeTracer{trackErr: errors.New("temporary track failure")}
	source := &source{cgroupRoot: root, store: &fakeStoreView{resources: []store.Resource{resource}}, state: testSourceState(t), tracer: tracer, log: logger.MustNew(logger.Config{Level: "error"})}
	stream := testStream("u1")
	stream.resource.ContainerID = "old-container"
	stream.handle, stream.cgroupID = 1, 1
	streams := map[string]*streamRuntime{"u1": stream}
	bindings := map[uint64]*streamBinding{1: {stream: stream, cgroupID: 1}}
	nextHandle := uint64(1)
	drains := drainCoordinator{tracer: tracer}

	if err := source.reconcile(streams, bindings, &nextHandle, &drains); err == nil {
		t.Fatal("replacement track failure was not returned")
	}
	if stream.resource.ContainerID != "old-container" {
		t.Fatalf("failed replacement committed container ID %q", stream.resource.ContainerID)
	}

	pending := []api.SourceEvent{}
	source.handleDrain(streams, bindings, &drains, &pending)
	tracer.trackErr = nil
	if err := source.reconcile(streams, bindings, &nextHandle, &drains); err != nil {
		t.Fatal(err)
	}
	if stream.resource.ContainerID != "new-container" || tracer.trackCalls != 2 {
		t.Fatalf("replacement was not retried: resource=%+v trackCalls=%d", stream.resource, tracer.trackCalls)
	}
}

func TestRecoveredMissingStreamFinalizesUntilEndAcknowledged(t *testing.T) {
	private := testSourceState(t)
	active := testStream("u1")
	writer := &source{state: private}
	if err := writer.persistStream(active); err != nil {
		t.Fatal(err)
	}

	recovered := &source{state: private, store: &fakeStoreView{}, tracer: &fakeTracer{}}
	streams, err := recovered.loadStreams()
	if err != nil {
		t.Fatal(err)
	}
	stream := streams["u1"]
	if stream == nil || !contains(stream.outcome.LossReasons, lossReasonRestart) {
		t.Fatalf("recovered stream=%+v", stream)
	}
	drains := drainCoordinator{tracer: recovered.tracer}
	if err := recovered.reconcile(streams, map[uint64]*streamBinding{}, new(uint64), &drains); err != nil {
		t.Fatal(err)
	}
	pending := []api.SourceEvent{}
	if err := recovered.finishTerminated(streams, &pending); err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending events=%d, want 1", len(pending))
	}
	event := pending[0]
	if event.End == nil || !contains(event.End.Outcome.LossReasons, lossReasonRestart) {
		t.Fatalf("end=%+v", event.End)
	}
	replayed, err := recovered.loadStreams()
	if err != nil {
		t.Fatal(err)
	}
	if got := replayed["u1"]; got == nil || !got.endReady || len(got.outcome.LossReasons) != len(event.End.Outcome.LossReasons) {
		t.Fatalf("replayed stream=%+v", got)
	}
	if err := recovered.AcknowledgeEnd(context.Background(), event.End.EndToken); err != nil {
		t.Fatal(err)
	}
	remaining, err := recovered.loadStreams()
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 0 {
		t.Fatalf("streams after end ACK=%v", remaining)
	}
}

func TestTerminatedStreamEmitsQueuedEventBeforeEnd(t *testing.T) {
	private := testSourceState(t)
	resource := store.Resource{Resource: testAPIResource("u1"), Terminated: true, ContainerRuntime: "containerd", ContainerID: "container-1"}
	tracer := &fakeTracer{}
	source := &source{store: &fakeStoreView{resources: []store.Resource{resource}}, state: private, tracer: tracer, runID: "run-1", log: logger.MustNew(logger.Config{Level: "error"})}
	stream := testStream("u1")
	stream.resource = resource
	stream.handle, stream.cgroupID = 1, 11
	if err := source.persistStream(stream); err != nil {
		t.Fatal(err)
	}
	streams := map[string]*streamRuntime{"u1": stream}
	bindings := map[uint64]*streamBinding{1: {stream: stream, cgroupID: 11}}
	drains := drainCoordinator{tracer: tracer}
	if err := source.reconcile(streams, bindings, new(uint64), &drains); err != nil {
		t.Fatal(err)
	}
	if tracer.flushCalls != 1 || stream.endReady {
		t.Fatalf("flushCalls=%d endReady=%t", tracer.flushCalls, stream.endReady)
	}
	full := make([]api.SourceEvent, sourceQueueSize)
	if err := source.enqueueEvent(&full, bindings[1], kernelEvent{CgroupID: 11, Handle: 1}, 0); err != nil {
		t.Fatal(err)
	}
	if len(full) != sourceQueueSize || !contains(stream.outcome.LossReasons, lossReasonSourceBackpressure) {
		t.Fatalf("full queue=%d outcome=%+v", len(full), stream.outcome)
	}
	pending := []api.SourceEvent{}
	if err := source.finishTerminated(streams, &pending); err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatal("stream ended before the drain barrier")
	}
	if err := source.enqueueEvent(&pending, bindings[1], kernelEvent{MonotonicNS: 2, CgroupID: 11, Handle: 1, SyscallNR: 1}, 0); err != nil {
		t.Fatal(err)
	}
	source.handleDrain(streams, bindings, &drains, &pending)
	if len(pending) != 2 {
		t.Fatalf("pending events=%d, want 2", len(pending))
	}
	first, second := pending[0], pending[1]
	if first.Delivery == nil || second.End == nil {
		t.Fatalf("events arrived out of order: first=%+v second=%+v", first, second)
	}
}
