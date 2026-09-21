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
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alibaba/opensandbox/internal/logger"
	"github.com/alibaba/opensandbox/nodeagent/pkg/api"
	"github.com/alibaba/opensandbox/nodeagent/pkg/registry"
	"github.com/alibaba/opensandbox/nodeagent/pkg/store"
	"github.com/alibaba/opensandbox/nodeagent/pkg/streamformat"
	"github.com/google/uuid"
)

const (
	sourceName                   = api.SourceNameSyscalls
	reconcileInterval            = 5 * time.Second
	sourceQueueSize              = 1024
	lossReasonOverflow           = "ring-buffer-overflow"
	lossReasonSourceBackpressure = "syscall-source-backpressure"
	lossReasonLateAttach         = "syscall-attach-after-container-start"
	lossReasonRestart            = "syscall-agent-restart"
)

type kernelEvent struct {
	MonotonicNS uint64
	CgroupID    uint64
	Handle      uint64
	HostPID     uint32
	HostTID     uint32
	SyscallNR   int64
	Comm        string
}

type kernelMessage struct {
	Event   kernelEvent
	Drained bool
}

type kernelTracer interface {
	Track(cgroupID, handle uint64) error
	Untrack(cgroupID uint64) error
	Flush() error
	Forget(handle uint64) error
	Messages() <-chan kernelMessage
	Errors() <-chan error
	Lost() (map[uint64]uint64, error)
	Close() error
}

type syscallRecord struct {
	SchemaVersion         int       `json:"schema_version"`
	Timestamp             time.Time `json:"timestamp"`
	MonotonicNS           uint64    `json:"monotonic_ns"`
	SyscallNR             int64     `json:"syscall_nr"`
	NodeArch              string    `json:"node_arch"`
	HostPID               uint32    `json:"host_pid"`
	HostTID               uint32    `json:"host_tid"`
	Comm                  string    `json:"comm"`
	ContainerRestartCount int32     `json:"container_restart_count"`
}

type streamRuntime struct {
	resource          store.Resource
	handle            uint64
	cgroupID          uint64
	sequence          uint64
	coverageStartedAt time.Time
	outcome           api.SourceOutcome
	endDrain          uint64
	endReady          bool
}

type streamBinding struct {
	stream       *streamRuntime
	cgroupID     uint64
	restartCount int32
	lost         uint64
	drainAfter   uint64
}

type source struct {
	cgroupRoot string
	store      store.View
	state      registry.SourceState
	log        logger.Logger
	onError    func(error)
	runID      string

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
	out    chan<- api.SourceEvent
	tracer kernelTracer
}

func init() {
	registry.RegisterSource(sourceName, func(dependencies registry.SourceDependencies) (api.Source, error) {
		return &source{cgroupRoot: dependencies.Config.SyscallCgroupRoot, store: dependencies.Store, state: dependencies.State, log: dependencies.Logger.Named(sourceName), onError: dependencies.OnError, runID: uuid.NewString(), done: make(chan struct{})}, nil
	})
}

func (s *source) Capabilities() api.Capabilities {
	return api.Capabilities{RecordKinds: []api.RecordKind{api.RecordKindSyscall}}
}

func (s *source) Start(ctx context.Context, out chan<- api.SourceEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		return errors.New("syscalls source already started")
	}
	if _, err := os.Stat(filepath.Join(s.cgroupRoot, "cgroup.controllers")); err != nil {
		return fmt.Errorf("syscalls source requires a cgroup v2 root at %s: %w", s.cgroupRoot, err)
	}
	streams, err := s.loadStreams()
	if err != nil {
		return fmt.Errorf("restore syscall streams: %w", err)
	}
	tracer, err := newKernelTracer()
	if err != nil {
		return fmt.Errorf("start syscall tracer: %w", err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.out = out
	s.tracer = tracer
	go s.run(runCtx, streams)
	return nil
}

func (s *source) Stop(ctx context.Context) error {
	s.mu.Lock()
	cancel := s.cancel
	s.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *source) Acknowledge(_ context.Context, results []api.AckResult) error {
	for _, result := range results {
		if err := validateToken(result.Token.Source, result.Token.StreamRef, result.Token.Value); err != nil {
			return api.Permanent(err)
		}
	}
	return nil
}

func (s *source) AcknowledgeEnd(_ context.Context, token api.EndToken) error {
	if err := validateToken(token.Source, token.StreamRef, token.Value); err != nil {
		return api.Permanent(err)
	}
	if err := s.deleteStream(token.StreamRef); err != nil {
		return fmt.Errorf("delete finalized syscall stream state: %w", err)
	}
	return nil
}

func validateToken(source string, streamRef api.StreamRef, value []byte) error {
	if source != sourceName || streamRef.Kind != api.RecordKindSyscall || !strings.HasPrefix(streamRef.ID, sourceName+"/") {
		return errors.New("syscalls token identity is invalid")
	}
	if len(value) != 8 {
		return errors.New("syscalls token value is invalid")
	}
	return nil
}

func (s *source) run(ctx context.Context, streams map[string]*streamRuntime) {
	defer close(s.done)
	defer close(s.out)
	defer func() {
		if err := s.tracer.Close(); err != nil && s.onError != nil {
			s.onError(fmt.Errorf("close syscall tracer: %w", err))
		}
	}()

	byHandle := make(map[uint64]*streamBinding)
	drains := drainCoordinator{tracer: s.tracer}
	pending := make([]api.SourceEvent, 0, sourceQueueSize)
	var nextHandle uint64
	clockOffset, err := monotonicWallOffset()
	if err != nil {
		s.reportError(fmt.Errorf("read monotonic clock: %w", err))
		return
	}
	if err := s.reconcile(streams, byHandle, &nextHandle, &drains); err != nil {
		s.reportError(err)
	}
	if err := s.finishTerminated(streams, &pending); err != nil {
		s.reportError(err)
	}

	reconcileTicker := time.NewTicker(reconcileInterval)
	defer reconcileTicker.Stop()
	for {
		var output chan<- api.SourceEvent
		var next api.SourceEvent
		if len(pending) != 0 {
			output = s.out
			next = pending[0]
		}
		select {
		case <-ctx.Done():
			return
		case output <- next:
			pending[0] = api.SourceEvent{}
			if len(pending) == 1 {
				pending = pending[:0]
			} else {
				pending = pending[1:]
			}
		case message, ok := <-s.tracer.Messages():
			if !ok {
				s.reportError(errors.New("syscall tracer event stream closed"))
				return
			}
			if message.Drained {
				s.handleDrain(streams, byHandle, &drains, &pending)
				continue
			}
			if err := s.enqueueEvent(&pending, byHandle[message.Event.Handle], message.Event, clockOffset); err != nil {
				s.reportError(err)
			}
		case err, ok := <-s.tracer.Errors():
			if !ok {
				s.reportError(errors.New("syscall tracer error stream closed"))
				return
			}
			if err != nil {
				s.reportError(fmt.Errorf("read syscall tracer: %w", err))
				return
			}
		case <-s.store.Changes():
			if err := s.reconcile(streams, byHandle, &nextHandle, &drains); err != nil {
				s.reportError(err)
			}
			if err := s.finishTerminated(streams, &pending); err != nil {
				s.reportError(err)
			}
		case <-reconcileTicker.C:
			if err := s.reconcile(streams, byHandle, &nextHandle, &drains); err != nil {
				s.reportError(err)
			}
			if err := s.collectLoss(byHandle); err != nil {
				s.reportError(err)
			}
			if nextOffset, err := monotonicWallOffset(); err != nil {
				s.reportError(fmt.Errorf("refresh monotonic clock: %w", err))
			} else {
				clockOffset = nextOffset
			}
			if err := s.finishTerminated(streams, &pending); err != nil {
				s.reportError(err)
			}
			if err := drains.start(); err != nil {
				s.reportError(fmt.Errorf("flush syscall ring buffer: %w", err))
			}
		}
	}
}

type drainCoordinator struct {
	tracer     kernelTracer
	generation uint64
	inFlight   uint64
	queued     bool
}

func (d *drainCoordinator) schedule() (uint64, error) {
	target := d.generation + 1
	d.queued = true
	if err := d.start(); err != nil {
		return target, err
	}
	return target, nil
}

func (d *drainCoordinator) start() error {
	if d.inFlight != 0 || !d.queued {
		return nil
	}
	if err := d.tracer.Flush(); err != nil {
		return err
	}
	d.generation++
	d.inFlight = d.generation
	d.queued = false
	return nil
}

func (d *drainCoordinator) complete() (uint64, error) {
	if d.inFlight == 0 {
		return 0, errors.New("unexpected syscall drain barrier")
	}
	completed := d.inFlight
	d.inFlight = 0
	return completed, nil
}

func (s *source) reconcile(streams map[string]*streamRuntime, byHandle map[uint64]*streamBinding, nextHandle *uint64, drains *drainCoordinator) error {
	resources := s.store.List()
	seen := make(map[string]bool, len(resources))
	toResolve := make([]store.Resource, 0, len(resources))
	for _, resource := range resources {
		seen[resource.PodUID] = true
		stream := streams[resource.PodUID]
		if !resource.Terminated && resource.ContainerID != "" && (stream == nil || !stream.endReady && (stream.handle == 0 || stream.resource.ContainerID != resource.ContainerID || stream.resource.ContainerRestartCount != resource.ContainerRestartCount)) {
			toResolve = append(toResolve, resource)
		}
	}
	resolved, resolveErr := resolveCgroups(s.cgroupRoot, toResolve)
	for _, resource := range resources {
		stream := streams[resource.PodUID]
		if stream != nil && stream.endReady {
			continue
		}
		if resource.Terminated {
			if stream == nil {
				s.store.Forget(resource.PodUID)
				continue
			}
			if err := s.prepareEnd(stream, byHandle[stream.handle], drains); err != nil {
				return errors.Join(resolveErr, err)
			}
			continue
		}
		if resource.ContainerID == "" {
			continue
		}
		if stream != nil && stream.handle != 0 && stream.resource.ContainerID == resource.ContainerID && stream.resource.ContainerRestartCount == resource.ContainerRestartCount {
			stream.resource = resource
			continue
		}
		cgroup, found := resolved[resource.PodUID]
		if !found {
			continue
		}
		cgroupID := cgroup.id
		if stream == nil {
			stream = &streamRuntime{
				resource:          resource,
				coverageStartedAt: time.Now().UTC().Truncate(time.Second),
				outcome: api.SourceOutcome{
					HadSourceGaps: true,
					LossReasons:   []string{lossReasonLateAttach},
				},
			}
			streams[resource.PodUID] = stream
			if err := s.persistStream(stream); err != nil {
				delete(streams, resource.PodUID)
				return errors.Join(resolveErr, err)
			}
		}
		if stream.handle == 0 {
			if err := s.attach(stream, resource, cgroupID, nextHandle, byHandle); err != nil {
				return errors.Join(resolveErr, err)
			}
			continue
		}
		previousContainerID := stream.resource.ContainerID
		previousRestartCount := stream.resource.ContainerRestartCount
		if stream.cgroupID != cgroupID || previousContainerID != resource.ContainerID || previousRestartCount != resource.ContainerRestartCount {
			previousBinding := byHandle[stream.handle]
			if previousBinding != nil && previousBinding.drainAfter != 0 {
				continue
			}
			if err := s.tracer.Untrack(stream.cgroupID); err != nil {
				return errors.Join(resolveErr, fmt.Errorf("untrack replaced cgroup %d: %w", stream.cgroupID, err))
			}
			generation, err := drains.schedule()
			if previousBinding != nil {
				previousBinding.drainAfter = generation
			}
			if err != nil {
				return errors.Join(resolveErr, fmt.Errorf("flush replaced cgroup %d: %w", stream.cgroupID, err))
			}
			(*nextHandle)++
			if err := s.tracer.Track(cgroupID, *nextHandle); err != nil {
				return errors.Join(resolveErr, fmt.Errorf("track replacement cgroup %d: %w", cgroupID, err))
			}
			stream.handle = *nextHandle
			stream.cgroupID = cgroupID
			byHandle[stream.handle] = &streamBinding{stream: stream, cgroupID: cgroupID, restartCount: resource.ContainerRestartCount}
		}
		stream.resource = resource
	}
	for podUID, stream := range streams {
		if seen[podUID] || stream.endReady || stream.endDrain != 0 {
			continue
		}
		if err := s.prepareEnd(stream, byHandle[stream.handle], drains); err != nil {
			return errors.Join(resolveErr, err)
		}
	}
	return resolveErr
}

func (s *source) attach(stream *streamRuntime, resource store.Resource, cgroupID uint64, nextHandle *uint64, byHandle map[uint64]*streamBinding) error {
	(*nextHandle)++
	stream.resource = resource
	stream.handle = *nextHandle
	stream.cgroupID = cgroupID
	if err := s.tracer.Track(cgroupID, stream.handle); err != nil {
		stream.handle = 0
		stream.cgroupID = 0
		return fmt.Errorf("track cgroup %d: %w", cgroupID, err)
	}
	byHandle[stream.handle] = &streamBinding{stream: stream, cgroupID: cgroupID, restartCount: resource.ContainerRestartCount}
	return nil
}

func (s *source) prepareEnd(stream *streamRuntime, binding *streamBinding, drains *drainCoordinator) error {
	if stream.endDrain != 0 || binding != nil && binding.drainAfter != 0 {
		return nil
	}
	if stream.handle == 0 {
		stream.endReady = true
		return nil
	}
	if err := s.tracer.Untrack(stream.cgroupID); err != nil {
		return fmt.Errorf("untrack cgroup %d before stream end: %w", stream.cgroupID, err)
	}
	generation, err := drains.schedule()
	if binding != nil {
		binding.drainAfter = generation
	}
	stream.endDrain = generation
	if err != nil {
		return fmt.Errorf("flush cgroup %d before stream end: %w", stream.cgroupID, err)
	}
	return nil
}

func (s *source) enqueueEvent(pending *[]api.SourceEvent, binding *streamBinding, event kernelEvent, clockOffset int64) error {
	if binding == nil || binding.cgroupID != event.CgroupID {
		return nil
	}
	if len(*pending) >= sourceQueueSize {
		stream := binding.stream
		stream.outcome.HadSourceGaps = true
		if contains(stream.outcome.LossReasons, lossReasonSourceBackpressure) {
			return nil
		}
		stream.outcome.LossReasons = append(stream.outcome.LossReasons, lossReasonSourceBackpressure)
		s.log.Warnf("syscall source queue is full; dropping events for sandbox %s", stream.resource.SandboxID)
		if err := s.persistStream(stream); err != nil {
			return err
		}
		return nil
	}
	stream := binding.stream
	stream.sequence++
	timestamp := time.Unix(0, clockOffset+int64(event.MonotonicNS)).UTC()
	body, err := json.Marshal(syscallRecord{SchemaVersion: 1, Timestamp: timestamp, MonotonicNS: event.MonotonicNS, SyscallNR: event.SyscallNR, NodeArch: runtime.GOARCH, HostPID: event.HostPID, HostTID: event.HostTID, Comm: event.Comm, ContainerRestartCount: binding.restartCount})
	if err != nil {
		return fmt.Errorf("encode syscall record: %w", err)
	}
	streamRef := api.StreamRef{ID: streamformat.SyscallStreamID(stream.resource.PodUID, stream.resource.Container), Kind: api.RecordKindSyscall}
	value := make([]byte, 8)
	binary.LittleEndian.PutUint64(value, stream.sequence)
	eventID := streamRef.ID + ":" + s.runID + ":" + strconv.FormatUint(stream.sequence, 10)
	*pending = append(*pending, api.SourceEvent{Delivery: &api.Delivery{
		Record:    api.Record{Kind: api.RecordKindSyscall, Timestamp: timestamp, Body: body, Resource: stream.resource.Resource},
		StreamRef: streamRef,
		AckToken:  api.AckToken{ID: eventID, Source: sourceName, StreamRef: streamRef, Value: value},
		RecordID:  eventID,
	}})
	return nil
}

func (s *source) handleDrain(streams map[string]*streamRuntime, byHandle map[uint64]*streamBinding, drains *drainCoordinator, pending *[]api.SourceEvent) {
	completed, err := drains.complete()
	if err != nil {
		s.reportError(err)
		return
	}
	if err := s.collectLoss(byHandle); err != nil {
		s.reportError(err)
	}
	for handle, binding := range byHandle {
		if binding.drainAfter == 0 || binding.drainAfter > completed {
			continue
		}
		if err := s.tracer.Forget(handle); err != nil {
			s.reportError(fmt.Errorf("forget drained syscall handle %d: %w", handle, err))
		}
		delete(byHandle, handle)
		if binding.stream.handle == handle {
			binding.stream.handle = 0
			binding.stream.cgroupID = 0
		}
	}
	for _, stream := range streams {
		if stream.endDrain != 0 && stream.endDrain <= completed {
			stream.endDrain = 0
			stream.endReady = true
		}
	}
	if err := s.finishTerminated(streams, pending); err != nil {
		s.reportError(err)
	}
	if err := drains.start(); err != nil {
		s.reportError(fmt.Errorf("flush queued syscall ring buffer: %w", err))
	}
}

func (s *source) collectLoss(byHandle map[uint64]*streamBinding) error {
	lost, err := s.tracer.Lost()
	if err != nil {
		return fmt.Errorf("read syscall loss counters: %w", err)
	}
	for handle, total := range lost {
		binding := byHandle[handle]
		if binding == nil || total <= binding.lost {
			continue
		}
		stream := binding.stream
		delta := total - binding.lost
		stream.outcome.HadSourceGaps = true
		if !contains(stream.outcome.LossReasons, lossReasonOverflow) {
			stream.outcome.LossReasons = append(stream.outcome.LossReasons, lossReasonOverflow)
		}
		s.log.Warnf("syscall ring buffer dropped %d events for sandbox %s", delta, stream.resource.SandboxID)
		if err := s.persistStream(stream); err != nil {
			return err
		}
		binding.lost = total
	}
	return nil
}

func (s *source) finishTerminated(streams map[string]*streamRuntime, pending *[]api.SourceEvent) error {
	for podUID, stream := range streams {
		if !stream.endReady {
			continue
		}
		if err := s.persistStream(stream); err != nil {
			return err
		}
		streamRef := api.StreamRef{ID: streamformat.SyscallStreamID(stream.resource.PodUID, stream.resource.Container), Kind: api.RecordKindSyscall}
		value := make([]byte, 8)
		binary.LittleEndian.PutUint64(value, 1)
		end := &api.StreamEnd{StreamRef: streamRef, EndToken: api.EndToken{ID: streamRef.ID + ":end:1", Source: sourceName, StreamRef: streamRef, Value: value}, Revision: 1, CoverageStartedAt: stream.coverageStartedAt, Resource: stream.resource.Resource, Outcome: stream.outcome}
		*pending = append(*pending, api.SourceEvent{End: end})
		delete(streams, podUID)
		s.store.Forget(podUID)
	}
	return nil
}

func (s *source) reportError(err error) {
	if s.onError != nil {
		s.onError(err)
	}
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
