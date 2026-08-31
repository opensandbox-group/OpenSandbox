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

package pipeline

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/internal/logger"
	"github.com/alibaba/opensandbox/nodeagent/pkg/api"
	"github.com/alibaba/opensandbox/nodeagent/pkg/identity"
	sourcepkg "github.com/alibaba/opensandbox/nodeagent/pkg/source"
	"github.com/alibaba/opensandbox/nodeagent/pkg/state"
	"golang.org/x/time/rate"
)

var pipelineTestCoverageStartedAt = time.Date(2026, 7, 23, 9, 58, 0, 0, time.UTC)

func testStreamRef() api.StreamRef {
	return api.StreamRef{ID: "stream", Kind: api.RecordKindContainerLog}
}

type fakeSource struct {
	acked         chan []api.AckResult
	ackMu         sync.Mutex
	ackErrs       []error
	ackCalls      int
	ended         chan api.EndToken
	endMu         sync.Mutex
	endErrs       []error
	endCalls      int
	endGate       <-chan struct{}
	endGateOnce   sync.Once
	endAckStarted chan api.EndToken
	endMutator    func(*api.EndToken)
}

type multiKindSource struct {
	*fakeSource
	kinds []api.RecordKind
}

func (s *multiKindSource) Capabilities() api.Capabilities {
	return api.Capabilities{RecordKinds: append([]api.RecordKind(nil), s.kinds...)}
}

func (*fakeSource) Capabilities() api.Capabilities {
	return api.Capabilities{RecordKinds: []api.RecordKind{api.RecordKindContainerLog}}
}
func (*fakeSource) Start(context.Context, chan<- api.SourceEvent) error { return nil }
func (s *fakeSource) Acknowledge(_ context.Context, results []api.AckResult) error {
	s.ackMu.Lock()
	s.ackCalls++
	if len(s.ackErrs) > 0 {
		err := s.ackErrs[0]
		s.ackErrs = s.ackErrs[1:]
		s.ackMu.Unlock()
		return err
	}
	s.ackMu.Unlock()
	s.acked <- results
	return nil
}
func (s *fakeSource) AcknowledgeEnd(ctx context.Context, token api.EndToken) error {
	s.endMu.Lock()
	s.endCalls++
	if len(s.endErrs) > 0 {
		err := s.endErrs[0]
		s.endErrs = s.endErrs[1:]
		s.endMu.Unlock()
		return err
	}
	s.endMu.Unlock()
	if s.endMutator != nil {
		s.endMutator(&token)
	}
	wait := false
	s.endGateOnce.Do(func() { wait = s.endGate != nil })
	if wait {
		if s.endAckStarted != nil {
			s.endAckStarted <- token
		}
		select {
		case <-s.endGate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s.ended <- token
	return nil
}
func (*fakeSource) Stop(context.Context) error { return nil }

type fakeSink struct {
	mu           sync.Mutex
	batches      []api.Batch
	finalized    []api.FinalizeRequest
	consumeErr   error
	consumeErrs  []error
	finalizeErrs []error
	finalizeWait time.Duration
	consumeCalls int
	consumeStart chan struct{}
}

type multiKindSink struct {
	*fakeSink
	kinds []api.RecordKind
}

func (s *multiKindSink) Capabilities() api.Capabilities {
	return api.Capabilities{RecordKinds: append([]api.RecordKind(nil), s.kinds...)}
}

type muxPipelineSource struct {
	kind   api.RecordKind
	events chan<- api.SourceEvent
	acked  chan []api.AckResult
	ended  chan api.EndToken
}

func (s *muxPipelineSource) Capabilities() api.Capabilities {
	return api.Capabilities{RecordKinds: []api.RecordKind{s.kind}}
}

func (s *muxPipelineSource) Start(_ context.Context, events chan<- api.SourceEvent) error {
	s.events = events
	return nil
}

func (s *muxPipelineSource) Acknowledge(ctx context.Context, results []api.AckResult) error {
	select {
	case s.acked <- append([]api.AckResult(nil), results...):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *muxPipelineSource) AcknowledgeEnd(ctx context.Context, token api.EndToken) error {
	select {
	case s.ended <- cloneEndToken(token):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (*muxPipelineSource) Stop(context.Context) error { return nil }

type nonRetryableTestError struct{}

func (nonRetryableTestError) Error() string   { return "permanent sink failure" }
func (nonRetryableTestError) Retryable() bool { return false }

type nonRetryableAckError struct{}

func (nonRetryableAckError) Error() string   { return "permanent source acknowledgement failure" }
func (nonRetryableAckError) Retryable() bool { return false }

type nonRetryableAckSource struct {
	fakeSource
	mu    sync.Mutex
	calls int
}

func (s *nonRetryableAckSource) Acknowledge(context.Context, []api.AckResult) error {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	return nonRetryableAckError{}
}

func (*fakeSink) Capabilities() api.Capabilities {
	return api.Capabilities{RecordKinds: []api.RecordKind{api.RecordKindContainerLog}}
}
func (*fakeSink) Guarantee() api.DeliveryGuarantee { return api.GuaranteeDurable }
func (s *fakeSink) Consume(_ context.Context, batch api.Batch) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.consumeCalls++
	if s.consumeStart != nil {
		select {
		case s.consumeStart <- struct{}{}:
		default:
		}
	}
	if s.consumeErr != nil {
		return s.consumeErr
	}
	if len(s.consumeErrs) > 0 {
		err := s.consumeErrs[0]
		s.consumeErrs = s.consumeErrs[1:]
		return err
	}
	s.batches = append(s.batches, batch)
	return nil
}
func (s *fakeSink) Finalize(ctx context.Context, request api.FinalizeRequest) error {
	if s.finalizeWait > 0 {
		select {
		case <-time.After(s.finalizeWait):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finalized = append(s.finalized, request)
	if len(s.finalizeErrs) > 0 {
		err := s.finalizeErrs[0]
		s.finalizeErrs = s.finalizeErrs[1:]
		return err
	}
	return nil
}

func TestPipelineFinalizeIsNotBoundedBySingleRequestTimeout(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	source := &fakeSource{acked: make(chan []api.AckResult, 1), ended: make(chan api.EndToken, 1)}
	sink := &fakeSink{finalizeWait: 50 * time.Millisecond}
	log, _ := logger.New(logger.Config{OutputPaths: []string{"stdout"}})
	cfg := testConfig()
	cfg.SinkTimeout = 10 * time.Millisecond
	p, err := New(cfg, source, sink, db, "target", log, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan api.SourceEvent, 1)
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx, events) }()
	streamRef := testStreamRef()
	events <- api.SourceEvent{End: &api.StreamEnd{StreamRef: streamRef, EndToken: api.EndToken{ID: "end", Source: "fake", StreamRef: streamRef}, Revision: 1, CoverageStartedAt: pipelineTestCoverageStartedAt, Resource: api.Resource{SandboxID: "sb"}}}
	select {
	case <-source.ended:
	case <-time.After(2 * time.Second):
		t.Fatal("Finalize remained limited by the per-request Sink timeout")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer closeCancel()
	if err := p.Close(closeCtx); err != nil {
		t.Fatal(err)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.finalized) != 1 {
		t.Fatalf("finalize calls=%d, want 1", len(sink.finalized))
	}
}
func (*fakeSink) Close(context.Context) error { return nil }

func TestPipelineAcknowledgesOnlyAfterSink(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	source := &fakeSource{acked: make(chan []api.AckResult, 1), ended: make(chan api.EndToken, 1)}
	sink := &fakeSink{}
	log, err := logger.New(logger.Config{OutputPaths: []string{"stdout"}})
	if err != nil {
		t.Fatal(err)
	}
	p, err := New(testConfig(), source, sink, db, "target", log, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan api.SourceEvent, 2)
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx, events) }()
	streamRef := testStreamRef()
	token := api.AckToken{ID: "ack", Source: "fake", StreamRef: streamRef}
	events <- api.SourceEvent{Delivery: &api.Delivery{StreamRef: streamRef, AckToken: token, RecordID: "record", Record: api.Record{Kind: api.RecordKindContainerLog, Resource: api.Resource{SandboxID: "sb"}}}}
	select {
	case results := <-source.acked:
		if len(results) != 1 || results[0].Token.ID != token.ID || results[0].Guarantee != api.GuaranteeDurable {
			t.Fatalf("results=%+v", results)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("acknowledgement timed out")
	}
	events <- api.SourceEvent{End: &api.StreamEnd{StreamRef: streamRef, EndToken: api.EndToken{ID: "end", Source: "fake", StreamRef: streamRef}, Revision: 1, CoverageStartedAt: pipelineTestCoverageStartedAt, Resource: api.Resource{SandboxID: "sb"}}}
	select {
	case <-source.ended:
	case <-time.After(2 * time.Second):
		t.Fatal("end acknowledgement timed out")
	}
	cancel()
	close(events)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer closeCancel()
	if err := p.Close(closeCtx); err != nil {
		t.Fatal(err)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.batches) != 1 || len(sink.finalized) != 1 {
		t.Fatalf("batches=%d finalized=%d", len(sink.batches), len(sink.finalized))
	}
}

func TestMuxPipelineRoutesTwoKinds(t *testing.T) {
	const (
		logKind     api.RecordKind = "test-log"
		syscallKind api.RecordKind = "test-syscall"
	)
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	logs := &muxPipelineSource{kind: logKind, acked: make(chan []api.AckResult, 1), ended: make(chan api.EndToken, 1)}
	syscalls := &muxPipelineSource{kind: syscallKind, acked: make(chan []api.AckResult, 1), ended: make(chan api.EndToken, 1)}
	mux, err := sourcepkg.NewMux([]sourcepkg.Child{
		{Name: "logs", Source: logs},
		{Name: "syscalls", Source: syscalls},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	baseSink := &fakeSink{}
	sink := &multiKindSink{fakeSink: baseSink, kinds: []api.RecordKind{logKind, syscallKind}}
	log, _ := logger.New(logger.Config{OutputPaths: []string{"stdout"}})
	p, err := New(testConfig(), mux, sink, db, "target", log, nil)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan api.SourceEvent)
	if err := mux.Start(ctx, events); err != nil {
		cancel()
		t.Fatal(err)
	}
	runDone := make(chan error, 1)
	go func() { runDone <- p.Run(ctx, events) }()

	logRef := api.StreamRef{ID: "logs/stream", Kind: logKind}
	syscallRef := api.StreamRef{ID: "syscalls/stream", Kind: syscallKind}
	logs.events <- api.SourceEvent{Delivery: &api.Delivery{
		Record:    api.Record{Kind: logKind, Body: []byte("log"), Resource: api.Resource{SandboxID: "logs"}},
		StreamRef: logRef,
		AckToken:  api.AckToken{ID: "log-ack", Source: "logs", StreamRef: logRef},
		RecordID:  "log-record",
	}}
	syscalls.events <- api.SourceEvent{Delivery: &api.Delivery{
		Record:    api.Record{Kind: syscallKind, Body: []byte("syscall"), Resource: api.Resource{SandboxID: "syscalls"}},
		StreamRef: syscallRef,
		AckToken:  api.AckToken{ID: "syscall-ack", Source: "syscalls", StreamRef: syscallRef},
		RecordID:  "syscall-record",
	}}

	select {
	case results := <-logs.acked:
		if len(results) != 1 || results[0].Token.Source != "logs" || results[0].Token.StreamRef != logRef {
			t.Fatalf("log ACK results = %+v", results)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("log ACK was not routed to the log Source")
	}
	select {
	case results := <-syscalls.acked:
		if len(results) != 1 || results[0].Token.Source != "syscalls" || results[0].Token.StreamRef != syscallRef {
			t.Fatalf("syscall ACK results = %+v", results)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("syscall ACK was not routed to the syscall Source")
	}

	cancel()
	if err := <-runDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("Pipeline Run error = %v", err)
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer stopCancel()
	if err := mux.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(stopCtx); err != nil {
		t.Fatal(err)
	}
	baseSink.mu.Lock()
	defer baseSink.mu.Unlock()
	if len(baseSink.batches) != 2 || baseSink.batches[0].StreamRef.Kind == baseSink.batches[1].StreamRef.Kind {
		t.Fatalf("batches = %+v", baseSink.batches)
	}
}

func TestPipelineRejectsStreamIDReusedWithAnotherKind(t *testing.T) {
	const (
		firstKind  api.RecordKind = "first-kind"
		secondKind api.RecordKind = "second-kind"
	)
	kinds := []api.RecordKind{firstKind, secondKind}
	for _, test := range []struct {
		name      string
		persisted bool
	}{
		{name: "after worker retirement"},
		{name: "after restart", persisted: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, err := state.Open(t.TempDir(), "target", 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			first := api.StreamRef{ID: "source/shared", Kind: firstKind}
			resource := api.Resource{SandboxID: "sb"}
			outcome := api.SourceOutcome{LossReasons: []string{}}
			endToken := api.EndToken{ID: "end", Source: "source", StreamRef: first}
			if test.persisted {
				if err := db.PutFinalizeIntent(state.FinalizeIntent{FinalizeID: identity.FinalizeID(first.ID, 1, "target"), TargetID: "target", StreamRef: first.ID, StreamKind: first.Kind, Revision: 1, CoverageStartedAt: pipelineTestCoverageStartedAt, FinalizedAt: pipelineTestCoverageStartedAt.Add(time.Minute), Resource: &resource, Outcome: &outcome, EndToken: &endToken, SinkDone: true, SourceDone: true}); err != nil {
					t.Fatal(err)
				}
			}
			baseSource := &fakeSource{acked: make(chan []api.AckResult, 1), ended: make(chan api.EndToken, 1)}
			baseSink := &fakeSink{}
			log, _ := logger.New(logger.Config{OutputPaths: []string{"stdout"}})
			p, err := New(testConfig(), &multiKindSource{fakeSource: baseSource, kinds: kinds}, &multiKindSink{fakeSink: baseSink, kinds: kinds}, db, "target", log, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !test.persisted {
				if err := db.BindStreamKind(first); err != nil {
					t.Fatal(err)
				}
			}
			second := api.StreamRef{ID: first.ID, Kind: secondKind}
			events := make(chan api.SourceEvent, 1)
			events <- api.SourceEvent{Delivery: &api.Delivery{StreamRef: second, AckToken: api.AckToken{ID: "ack", Source: "source", StreamRef: second}, RecordID: "record", Record: api.Record{Kind: secondKind, Resource: resource}}}
			if err := p.Run(context.Background(), events); err == nil || !strings.Contains(err.Error(), "changed record kind") {
				t.Fatalf("Pipeline.Run() error = %v", err)
			}
			baseSink.mu.Lock()
			consumeCalls := baseSink.consumeCalls
			baseSink.mu.Unlock()
			if consumeCalls != 0 {
				t.Fatalf("Sink.Consume() calls = %d, want 0", consumeCalls)
			}
			closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer closeCancel()
			if err := p.Close(closeCtx); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPipelineRetriesSourceAcknowledgeWithoutReconsuming(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	source := &fakeSource{
		acked:   make(chan []api.AckResult, 1),
		ackErrs: []error{errors.New("retry one"), errors.New("retry two")},
		ended:   make(chan api.EndToken, 1),
	}
	sink := &fakeSink{}
	log, _ := logger.New(logger.Config{OutputPaths: []string{"stdout"}})
	cfg := testConfig()
	cfg.RetryMaxInterval = 10 * time.Millisecond
	p, err := New(cfg, source, sink, db, "target", log, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan api.SourceEvent, 1)
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx, events) }()
	streamRef := testStreamRef()
	events <- api.SourceEvent{Delivery: &api.Delivery{StreamRef: streamRef, AckToken: api.AckToken{ID: "ack", Source: "fake", StreamRef: streamRef}, RecordID: "record", Record: api.Record{Kind: api.RecordKindContainerLog, Resource: api.Resource{SandboxID: "sb"}}}}
	select {
	case <-source.acked:
	case <-time.After(2 * time.Second):
		t.Fatal("source acknowledgement retry timed out")
	}
	source.ackMu.Lock()
	ackCalls := source.ackCalls
	source.ackMu.Unlock()
	sink.mu.Lock()
	consumeCalls := sink.consumeCalls
	sink.mu.Unlock()
	if ackCalls != 3 || consumeCalls != 1 {
		t.Fatalf("acknowledge calls=%d consume calls=%d, want 3 and 1", ackCalls, consumeCalls)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer closeCancel()
	if err := p.Close(closeCtx); err != nil {
		t.Fatal(err)
	}
}

func TestPipelineDropPolicyRecordsOutcomeWithoutCallingSink(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	source := &fakeSource{acked: make(chan []api.AckResult, 1), ended: make(chan api.EndToken, 1)}
	sink := &fakeSink{}
	log, _ := logger.New(logger.Config{OutputPaths: []string{"stdout"}})
	cfg := testConfig()
	cfg.DropPolicy = "drop"
	cfg.MemoryBudgetBytes = 1024
	cfg.PerSandboxQueueBytes = 1024
	p, err := New(cfg, source, sink, db, "target", log, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan api.SourceEvent)
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx, events) }()
	streamRef := testStreamRef()
	events <- api.SourceEvent{Delivery: &api.Delivery{StreamRef: streamRef, AckToken: api.AckToken{ID: "large", Source: "fake", StreamRef: streamRef}, RecordID: "large", Record: api.Record{Kind: api.RecordKindContainerLog, Body: make([]byte, 2048), Resource: api.Resource{SandboxID: "sb"}}}}
	select {
	case results := <-source.acked:
		if len(results) != 1 || results[0].Disposition != api.AckIntentionalDrop || results[0].Reason != "pipeline-record-too-large" {
			t.Fatalf("results=%+v", results)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("drop acknowledgement timed out")
	}
	events <- api.SourceEvent{End: &api.StreamEnd{StreamRef: streamRef, EndToken: api.EndToken{ID: "end", Source: "fake", StreamRef: streamRef}, Revision: 1, CoverageStartedAt: pipelineTestCoverageStartedAt, Resource: api.Resource{SandboxID: "sb"}}}
	select {
	case <-source.ended:
	case <-time.After(2 * time.Second):
		t.Fatal("end acknowledgement timed out")
	}
	cancel()
	close(events)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer closeCancel()
	if err := p.Close(closeCtx); err != nil {
		t.Fatal(err)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.batches) != 0 || len(sink.finalized) != 1 || !sink.finalized[0].Outcome.HadDrops {
		t.Fatalf("batches=%d finalized=%+v", len(sink.batches), sink.finalized)
	}
}

func TestPipelineRetriesFinalizeWithStableIntent(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	source := &fakeSource{acked: make(chan []api.AckResult, 1), ended: make(chan api.EndToken, 1)}
	sink := &fakeSink{finalizeErrs: []error{errors.New("retry one"), errors.New("retry two")}}
	log, _ := logger.New(logger.Config{OutputPaths: []string{"stdout"}})
	cfg := testConfig()
	cfg.RetryMaxInterval = 10 * time.Millisecond
	p, err := New(cfg, source, sink, db, "target", log, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan api.SourceEvent, 1)
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx, events) }()
	streamRef := testStreamRef()
	endToken := api.EndToken{ID: "end", Source: "fake", StreamRef: streamRef}
	coverageStartedAt := time.Date(2026, 7, 23, 9, 58, 0, 0, time.UTC)
	events <- api.SourceEvent{End: &api.StreamEnd{StreamRef: streamRef, EndToken: endToken, Revision: 1, CoverageStartedAt: coverageStartedAt}}
	select {
	case <-source.ended:
	case <-time.After(2 * time.Second):
		t.Fatal("end acknowledgement timed out")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer closeCancel()
	if err := p.Close(closeCtx); err != nil {
		t.Fatal(err)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.finalized) != 3 {
		t.Fatalf("finalize calls=%d, want 3", len(sink.finalized))
	}
	first := sink.finalized[0]
	if !first.CoverageStartedAt.Equal(coverageStartedAt) {
		t.Fatalf("coverage boundary=%v want=%v", first.CoverageStartedAt, coverageStartedAt)
	}
	for _, request := range sink.finalized[1:] {
		if request.FinalizeID != first.FinalizeID || request.Revision != first.Revision || !request.CoverageStartedAt.Equal(coverageStartedAt) || !request.FinalizedAt.Equal(first.FinalizedAt) {
			t.Fatalf("finalize request changed across retry: first=%+v retry=%+v", first, request)
		}
	}
}

func TestPipelineRejectsMissingCoverageBoundaryBeforeFinalize(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	source := &fakeSource{acked: make(chan []api.AckResult, 1), ended: make(chan api.EndToken, 1)}
	sink := &fakeSink{}
	log, _ := logger.New(logger.Config{OutputPaths: []string{"stdout"}})
	p, err := New(testConfig(), source, sink, db, "target", log, nil)
	if err != nil {
		t.Fatal(err)
	}
	streamRef := testStreamRef()
	err = p.finalize(context.Background(), &worker{streamRef: streamRef}, &api.StreamEnd{StreamRef: streamRef, Revision: 1})
	if err == nil || !strings.Contains(err.Error(), "coverage boundary") {
		t.Fatalf("finalize error=%v", err)
	}
	if len(sink.finalized) != 0 {
		t.Fatalf("sink received invalid finalize: %+v", sink.finalized)
	}
}

func TestPipelineRetriesSourceEndAcknowledgement(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	source := &fakeSource{acked: make(chan []api.AckResult, 1), ended: make(chan api.EndToken, 1), endErrs: []error{errors.New("retry one"), errors.New("retry two")}}
	sink := &fakeSink{}
	log, _ := logger.New(logger.Config{OutputPaths: []string{"stdout"}})
	cfg := testConfig()
	cfg.RetryMaxInterval = 10 * time.Millisecond
	p, err := New(cfg, source, sink, db, "target", log, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan api.SourceEvent, 1)
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx, events) }()
	streamRef := testStreamRef()
	events <- api.SourceEvent{End: &api.StreamEnd{StreamRef: streamRef, EndToken: api.EndToken{ID: "end", Source: "fake", StreamRef: streamRef}, Revision: 1, CoverageStartedAt: pipelineTestCoverageStartedAt}}
	select {
	case <-source.ended:
	case <-time.After(2 * time.Second):
		t.Fatal("end acknowledgement timed out")
	}
	source.endMu.Lock()
	endCalls := source.endCalls
	source.endMu.Unlock()
	if endCalls != 3 {
		t.Fatalf("end acknowledgement calls=%d, want 3", endCalls)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer closeCancel()
	if err := p.Close(closeCtx); err != nil {
		t.Fatal(err)
	}
}

func TestPipelineStopsOnNonRetryableFinalizeFailure(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	source := &fakeSource{acked: make(chan []api.AckResult, 1), ended: make(chan api.EndToken, 1)}
	sink := &fakeSink{finalizeErrs: []error{nonRetryableTestError{}}}
	log, _ := logger.New(logger.Config{OutputPaths: []string{"stdout"}})
	p, err := New(testConfig(), source, sink, db, "target", log, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan api.SourceEvent, 1)
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx, events) }()
	streamRef := testStreamRef()
	events <- api.SourceEvent{End: &api.StreamEnd{StreamRef: streamRef, EndToken: api.EndToken{ID: "end", Source: "fake", StreamRef: streamRef}, Revision: 1, CoverageStartedAt: pipelineTestCoverageStartedAt}}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "non-retryable sink finalize failure") {
			t.Fatalf("Run() error=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pipeline did not stop after non-retryable finalize failure")
	}
	select {
	case token := <-source.ended:
		t.Fatalf("source end was acknowledged after failed finalize: %+v", token)
	default:
	}
	intent, found, err := db.GetFinalizeIntent(streamRef.ID, 1)
	if err != nil || !found || intent.SinkDone || intent.SourceDone {
		t.Fatalf("intent=%+v found=%v err=%v", intent, found, err)
	}
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer closeCancel()
	if err := p.Close(closeCtx); err != nil {
		t.Fatal(err)
	}
}

func TestPipelineKeepsDropOutcomeOnSuccessorWorker(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	endGate := make(chan struct{})
	source := &fakeSource{acked: make(chan []api.AckResult, 1), ended: make(chan api.EndToken, 2), endGate: endGate, endAckStarted: make(chan api.EndToken, 1)}
	sink := &fakeSink{}
	log, _ := logger.New(logger.Config{OutputPaths: []string{"stdout"}})
	cfg := testConfig()
	cfg.DropPolicy = "drop"
	cfg.MemoryBudgetBytes = 1024
	cfg.PerSandboxQueueBytes = 1024
	p, err := New(cfg, source, sink, db, "target", log, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan api.SourceEvent, 3)
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx, events) }()
	streamRef := testStreamRef()
	firstEnd := api.EndToken{ID: "end-1", Source: "fake", StreamRef: streamRef}
	events <- api.SourceEvent{End: &api.StreamEnd{StreamRef: streamRef, EndToken: firstEnd, Revision: 1, CoverageStartedAt: pipelineTestCoverageStartedAt, Resource: api.Resource{SandboxID: "sb"}}}
	select {
	case <-source.endAckStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("revision 1 end acknowledgement did not start")
	}
	delivery := &api.Delivery{StreamRef: streamRef, AckToken: api.AckToken{ID: "drop", Source: "fake", StreamRef: streamRef}, RecordID: "drop", Record: api.Record{Kind: api.RecordKindContainerLog, Body: make([]byte, 2048), Resource: api.Resource{SandboxID: "sb"}}}
	events <- api.SourceEvent{Delivery: delivery}
	select {
	case results := <-source.acked:
		if len(results) != 1 || results[0].Disposition != api.AckIntentionalDrop {
			t.Fatalf("drop results=%+v", results)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("successor drop acknowledgement timed out")
	}
	close(endGate)
	select {
	case ended := <-source.ended:
		if ended.ID != firstEnd.ID {
			t.Fatalf("first end=%q", ended.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("revision 1 end acknowledgement timed out")
	}
	secondEnd := api.EndToken{ID: "end-2", Source: "fake", StreamRef: streamRef}
	events <- api.SourceEvent{End: &api.StreamEnd{StreamRef: streamRef, EndToken: secondEnd, Revision: 2, CoverageStartedAt: pipelineTestCoverageStartedAt, Resource: api.Resource{SandboxID: "sb"}}}
	select {
	case ended := <-source.ended:
		if ended.ID != secondEnd.ID {
			t.Fatalf("second end=%q", ended.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("revision 2 end acknowledgement timed out")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer closeCancel()
	if err := p.Close(closeCtx); err != nil {
		t.Fatal(err)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.finalized) != 2 || sink.finalized[0].Outcome.HadDrops || !sink.finalized[1].Outcome.HadDrops || !containsReason(sink.finalized[1].Outcome.LossReasons, "pipeline-record-too-large") {
		t.Fatalf("finalized=%+v", sink.finalized)
	}
}

func TestPipelineStopsOnNonRetryableSinkFailure(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	source := &fakeSource{acked: make(chan []api.AckResult, 1), ended: make(chan api.EndToken, 1)}
	sink := &fakeSink{consumeErr: nonRetryableTestError{}}
	log, _ := logger.New(logger.Config{OutputPaths: []string{"stdout"}})
	runtimeErrors := make(chan error, 1)
	p, err := New(testConfig(), source, sink, db, "target", log, func(err error) { runtimeErrors <- err })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan api.SourceEvent, 1)
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx, events) }()
	streamRef := testStreamRef()
	events <- api.SourceEvent{Delivery: &api.Delivery{StreamRef: streamRef, AckToken: api.AckToken{ID: "ack", Source: "fake", StreamRef: streamRef}, RecordID: "record", Record: api.Record{Kind: api.RecordKindContainerLog, Resource: api.Resource{SandboxID: "sb"}}}}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "non-retryable sink consume failure") {
			t.Fatalf("run error=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pipeline kept retrying a non-retryable sink failure")
	}
	select {
	case err := <-runtimeErrors:
		if !strings.Contains(err.Error(), "permanent sink failure") {
			t.Fatalf("runtime error=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("non-retryable sink failure was not reported")
	}
	select {
	case results := <-source.acked:
		t.Fatalf("source was acknowledged after sink failure: %+v", results)
	default:
	}
	sink.mu.Lock()
	consumeCalls := sink.consumeCalls
	sink.mu.Unlock()
	if consumeCalls != 1 {
		t.Fatalf("consume calls=%d", consumeCalls)
	}
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer closeCancel()
	if err := p.Close(closeCtx); err != nil {
		t.Fatal(err)
	}
}

func TestPipelineKeepsRetryStateAcrossSinkAndSourceRecovery(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	source := &fakeSource{acked: make(chan []api.AckResult, 1), ackErrs: []error{errors.New("temporary source failure")}, ended: make(chan api.EndToken, 1)}
	sink := &fakeSink{consumeErrs: []error{errors.New("temporary sink failure")}}
	states := make(chan bool, 4)
	cfg := testConfig()
	cfg.OnRetryStateChange = func(active bool) { states <- active }
	log, _ := logger.New(logger.Config{OutputPaths: []string{"stdout"}})
	p, err := New(cfg, source, sink, db, "target", log, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan api.SourceEvent, 1)
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx, events) }()
	streamRef := testStreamRef()
	events <- api.SourceEvent{Delivery: &api.Delivery{StreamRef: streamRef, AckToken: api.AckToken{ID: "ack", Source: "fake", StreamRef: streamRef}, RecordID: "record", Record: api.Record{Kind: api.RecordKindContainerLog, Resource: api.Resource{SandboxID: "sb"}}}}
	select {
	case active := <-states:
		if !active {
			t.Fatal("retry state cleared before it became active")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("retry state did not become active")
	}
	select {
	case <-source.acked:
	case <-time.After(2 * time.Second):
		t.Fatal("sink and source retries did not recover")
	}
	select {
	case active := <-states:
		if active {
			t.Fatal("retry state remained active after recovery")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("retry state was not cleared")
	}
	select {
	case active := <-states:
		t.Fatalf("retry state oscillated between sink and source recovery: %t", active)
	default:
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer closeCancel()
	if err := p.Close(closeCtx); err != nil {
		t.Fatal(err)
	}
}

func TestPipelineRetryStateTracksConcurrentOperations(t *testing.T) {
	states := make(chan bool, 2)
	p := &Pipeline{cfg: Config{OnRetryStateChange: func(active bool) { states <- active }}}
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	var operations sync.WaitGroup
	operations.Add(2)
	for range 2 {
		go func() {
			defer operations.Done()
			endRetry := p.beginRetry()
			started <- struct{}{}
			<-release
			endRetry()
		}()
	}
	<-started
	<-started
	if active := <-states; !active {
		t.Fatal("retry state did not become active")
	}
	select {
	case active := <-states:
		t.Fatalf("retry state changed to %v while another operation was retrying", active)
	default:
	}
	close(release)
	operations.Wait()
	if active := <-states; active {
		t.Fatal("retry state remained active after all operations recovered")
	}
}

func TestPipelineRetryStateCallbackCanReenter(t *testing.T) {
	states := make(chan bool, 2)
	var p *Pipeline
	var nested sync.Once
	p = &Pipeline{cfg: Config{OnRetryStateChange: func(active bool) {
		states <- active
		if active {
			nested.Do(func() {
				endNested := p.beginRetry()
				endNested()
			})
		}
	}}}
	done := make(chan struct{})
	go func() {
		endRetry := p.beginRetry()
		endRetry()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reentrant retry-state callback deadlocked")
	}
	if active := <-states; !active {
		t.Fatal("retry state did not become active")
	}
	if active := <-states; active {
		t.Fatal("retry state remained active")
	}
}

func TestPipelineStopsOnNonRetryableSourceAcknowledgeFailure(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	source := &nonRetryableAckSource{fakeSource: fakeSource{acked: make(chan []api.AckResult, 1), ended: make(chan api.EndToken, 1)}}
	sink := &fakeSink{}
	log, _ := logger.New(logger.Config{OutputPaths: []string{"stdout"}})
	runtimeErrors := make(chan error, 1)
	p, err := New(testConfig(), source, sink, db, "target", log, func(err error) { runtimeErrors <- err })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan api.SourceEvent, 1)
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx, events) }()
	streamRef := testStreamRef()
	events <- api.SourceEvent{Delivery: &api.Delivery{StreamRef: streamRef, AckToken: api.AckToken{ID: "ack", Source: "fake", StreamRef: streamRef}, RecordID: "record", Record: api.Record{Kind: api.RecordKindContainerLog, Resource: api.Resource{SandboxID: "sb"}}}}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "non-retryable source acknowledge failure") {
			t.Fatalf("run error=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pipeline kept retrying a non-retryable source acknowledgement failure")
	}
	select {
	case err := <-runtimeErrors:
		if !strings.Contains(err.Error(), "permanent source acknowledgement failure") {
			t.Fatalf("runtime error=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("non-retryable source acknowledgement failure was not reported")
	}
	source.mu.Lock()
	ackCalls := source.calls
	source.mu.Unlock()
	if ackCalls != 1 {
		t.Fatalf("acknowledge calls=%d", ackCalls)
	}
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer closeCancel()
	if err := p.Close(closeCtx); err != nil {
		t.Fatal(err)
	}
}

func TestPipelinePrefersQueuedWorkerErrorOverWorkerCancellation(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	source := &fakeSource{acked: make(chan []api.AckResult, 1), ended: make(chan api.EndToken, 1)}
	sink := &fakeSink{}
	log, _ := logger.New(logger.Config{OutputPaths: []string{"stdout"}})
	p, err := New(testConfig(), source, sink, db, "target", log, nil)
	if err != nil {
		t.Fatal(err)
	}
	workerErr := errors.New("worker failed")
	p.workerErrors <- workerErr
	p.cancelWorkers()
	if err := p.Run(context.Background(), make(chan api.SourceEvent)); !errors.Is(err, workerErr) {
		t.Fatalf("Run() error = %v, want queued worker error", err)
	}
	if err := p.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPipelineRetiresFinalizedWorkerAndRecreatesForLateRevision(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	endGate := make(chan struct{})
	source := &fakeSource{acked: make(chan []api.AckResult, 1), ended: make(chan api.EndToken, 2), endGate: endGate, endAckStarted: make(chan api.EndToken, 1)}
	sink := &fakeSink{}
	log, _ := logger.New(logger.Config{OutputPaths: []string{"stdout"}})
	p, err := New(testConfig(), source, sink, db, "target", log, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan api.SourceEvent, 2)
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx, events) }()
	streamRef := testStreamRef()

	first := api.EndToken{ID: "1", Source: "fake", StreamRef: streamRef}
	second := api.EndToken{ID: "2", Source: "fake", StreamRef: streamRef}
	events <- api.SourceEvent{End: &api.StreamEnd{StreamRef: streamRef, EndToken: first, Revision: 1, CoverageStartedAt: pipelineTestCoverageStartedAt}}
	select {
	case started := <-source.endAckStarted:
		if started.ID != first.ID {
			t.Fatalf("started end token=%s want=%s", started.ID, first.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("revision 1 end acknowledgement did not start")
	}
	events <- api.SourceEvent{End: &api.StreamEnd{StreamRef: streamRef, EndToken: second, Revision: 2, CoverageStartedAt: pipelineTestCoverageStartedAt}}
	select {
	case ended := <-source.ended:
		t.Fatalf("revision %s passed its predecessor", ended.ID)
	case <-time.After(50 * time.Millisecond):
	}
	close(endGate)

	for revision, token := range []api.EndToken{first, second} {
		select {
		case ended := <-source.ended:
			if ended.ID != token.ID {
				t.Fatalf("end token=%s want=%s", ended.ID, token.ID)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("revision %d acknowledgement timed out", revision+1)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		p.mu.Lock()
		workerCount := len(p.workers)
		handoffCount := len(p.handoffs)
		p.mu.Unlock()
		if workerCount == 0 && handoffCount == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("workers=%d handoffs=%d", workerCount, handoffCount)
		}
		time.Sleep(time.Millisecond)
	}

	cancel()
	close(events)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer closeCancel()
	if err := p.Close(closeCtx); err != nil {
		t.Fatal(err)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.finalized) != 2 {
		t.Fatalf("finalized=%d", len(sink.finalized))
	}
}

func TestPipelineReleasesQueuedSuccessorWhenPredecessorIsCanceled(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	endGate := make(chan struct{})
	source := &fakeSource{acked: make(chan []api.AckResult, 1), ended: make(chan api.EndToken, 1), endGate: endGate, endAckStarted: make(chan api.EndToken, 1)}
	sink := &fakeSink{}
	log, _ := logger.New(logger.Config{OutputPaths: []string{"stdout"}})
	p, err := New(testConfig(), source, sink, db, "target", log, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan api.SourceEvent, 2)
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx, events) }()
	streamRef := testStreamRef()
	events <- api.SourceEvent{End: &api.StreamEnd{StreamRef: streamRef, EndToken: api.EndToken{ID: "1", Source: "fake", StreamRef: streamRef}, Revision: 1, CoverageStartedAt: pipelineTestCoverageStartedAt}}
	select {
	case <-source.endAckStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("predecessor end acknowledgement did not start")
	}
	events <- api.SourceEvent{Delivery: &api.Delivery{StreamRef: streamRef, AckToken: api.AckToken{ID: "late", Source: "fake", StreamRef: streamRef}, RecordID: "late", Record: api.Record{Kind: api.RecordKindContainerLog, Body: []byte("late"), Resource: api.Resource{SandboxID: "sb"}}}}
	deadline := time.Now().Add(2 * time.Second)
	for {
		p.budgetMu.Lock()
		queuedBytes := p.globalBytes
		p.budgetMu.Unlock()
		if queuedBytes > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("successor event was not admitted")
		}
		time.Sleep(time.Millisecond)
	}

	p.cancelWorkers()
	deadline = time.Now().Add(2 * time.Second)
	for {
		p.budgetMu.Lock()
		queuedBytes := p.globalBytes
		p.budgetMu.Unlock()
		if queuedBytes == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("queued bytes leaked: %d", queuedBytes)
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	close(events)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer closeCancel()
	if err := p.Close(closeCtx); err != nil {
		t.Fatal(err)
	}
}

func TestPipelineReconcilesSourceDoneFinalizeIntentAfterRestart(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	streamRef := "stream"
	intent := state.FinalizeIntent{FinalizeID: "", TargetID: "target", StreamRef: streamRef, Revision: 1, CoverageStartedAt: pipelineTestCoverageStartedAt, FinalizedAt: time.Now().UTC().Truncate(time.Second), SinkDone: true}
	intent.FinalizeID = identity.FinalizeID(streamRef, intent.Revision, intent.TargetID)
	if err := db.PutFinalizeIntent(intent); err != nil {
		t.Fatal(err)
	}
	if err := db.PutSourceStream(state.SourceStream{StreamRef: streamRef, Resource: state.FrozenResource{SandboxID: "sb", ClusterName: "cluster", Namespace: "ns", PodName: "pod", PodUID: "uid", NodeName: "node", Container: "sandbox", LogDirectory: "/var/log/pods/ns_pod_uid/sandbox"}, Revision: 1, AcknowledgedRevision: 1, Ended: true}); err != nil {
		t.Fatal(err)
	}
	source := &fakeSource{acked: make(chan []api.AckResult, 1), ended: make(chan api.EndToken, 1)}
	sink := &fakeSink{}
	log, _ := logger.New(logger.Config{OutputPaths: []string{"stdout"}})
	p, err := New(testConfig(), source, sink, db, "target", log, nil)
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan api.SourceEvent)
	close(events)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.Run(ctx, events); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	got, found, err := db.GetFinalizeIntent(streamRef, 1)
	if err != nil || !found || !got.SourceDone {
		t.Fatalf("intent=%+v found=%v err=%v", got, found, err)
	}
	if err := p.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPipelineReplaysSelfContainedFinalizeIntentAfterRestart(t *testing.T) {
	for _, test := range []struct {
		name              string
		sinkDone          bool
		wantFinalizeCalls int
	}{
		{name: "before sink completion", wantFinalizeCalls: 1},
		{name: "after sink completion", sinkDone: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, err := state.Open(t.TempDir(), "target", 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			ref := testStreamRef()
			resource := api.Resource{SandboxID: "sb", ClusterName: "cluster", Namespace: "ns", PodName: "pod", PodUID: "uid", NodeName: "node", Container: "sandbox"}
			metadata := api.StreamMetadata{"test.format": "value"}
			outcome := api.SourceOutcome{HadDrops: true, LossReasons: []string{"test-drop"}}
			token := api.EndToken{ID: "end", Source: "fake", StreamRef: ref, Value: []byte("opaque")}
			intent := state.FinalizeIntent{
				FinalizeID:        identity.FinalizeID(ref.ID, 1, "target"),
				TargetID:          "target",
				StreamRef:         ref.ID,
				StreamKind:        ref.Kind,
				Revision:          1,
				CoverageStartedAt: pipelineTestCoverageStartedAt,
				FinalizedAt:       pipelineTestCoverageStartedAt.Add(time.Minute),
				Resource:          &resource,
				Metadata:          metadata,
				Outcome:           &outcome,
				EndToken:          &token,
				SinkDone:          test.sinkDone,
			}
			if err := db.PutFinalizeIntent(intent); err != nil {
				t.Fatal(err)
			}
			source := &fakeSource{
				acked: make(chan []api.AckResult, 1),
				ended: make(chan api.EndToken, 1),
				endMutator: func(token *api.EndToken) {
					token.Value[0] = 'X'
				},
			}
			sink := &fakeSink{}
			log, _ := logger.New(logger.Config{OutputPaths: []string{"stdout"}})
			p, err := New(testConfig(), source, sink, db, "target", log, nil)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- p.Run(ctx, make(chan api.SourceEvent)) }()
			select {
			case got := <-source.ended:
				if string(got.Value) != "Xpaque" {
					t.Fatalf("mutated EndToken value = %q, want %q", got.Value, "Xpaque")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("replayed end acknowledgement timed out")
			}
			cancel()
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			got, found, err := db.GetFinalizeIntent(ref.ID, 1)
			if err != nil || !found || !got.SinkDone || !got.SourceDone || got.EndToken == nil || string(got.EndToken.Value) != "opaque" {
				t.Fatalf("finalize intent = %+v found=%v err=%v", got, found, err)
			}
			sink.mu.Lock()
			if len(sink.finalized) != test.wantFinalizeCalls {
				t.Fatalf("Finalize calls = %d, want %d", len(sink.finalized), test.wantFinalizeCalls)
			}
			if len(sink.finalized) == 1 {
				replayed := sink.finalized[0]
				if replayed.StreamRef != ref || replayed.Resource != resource || !replayed.Metadata.Equal(metadata) || !equalSourceOutcome(replayed.Outcome, outcome) {
					t.Fatalf("replayed finalize request = %+v", replayed)
				}
			}
			sink.mu.Unlock()
			if err := p.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPipelineEnrichesLegacyFinalizeIntentWhenSourceReplaysEnd(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ref := testStreamRef()
	legacy := state.FinalizeIntent{FinalizeID: identity.FinalizeID(ref.ID, 1, "target"), TargetID: "target", StreamRef: ref.ID, Revision: 1, CoverageStartedAt: pipelineTestCoverageStartedAt, FinalizedAt: pipelineTestCoverageStartedAt.Add(time.Minute), SinkDone: true}
	if err := db.PutFinalizeIntent(legacy); err != nil {
		t.Fatal(err)
	}
	if err := db.PutSourceStream(state.SourceStream{StreamRef: ref.ID, Resource: state.FrozenResource{SandboxID: "sb", ClusterName: "cluster", Namespace: "ns", PodName: "pod", PodUID: "uid", NodeName: "node", Container: "sandbox", LogDirectory: "/var/log/pods/ns_pod_uid/sandbox"}, Revision: 1, FinalizingRevision: 1, FinalizingOutcome: &state.OutcomeSnapshot{LossReasons: []string{}}}); err != nil {
		t.Fatal(err)
	}
	source := &fakeSource{acked: make(chan []api.AckResult, 1), ended: make(chan api.EndToken, 1)}
	sink := &fakeSink{}
	log, _ := logger.New(logger.Config{OutputPaths: []string{"stdout"}})
	p, err := New(testConfig(), source, sink, db, "target", log, nil)
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan api.SourceEvent, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx, events) }()
	resource := api.Resource{SandboxID: "sb"}
	outcome := api.SourceOutcome{LossReasons: []string{}}
	token := api.EndToken{ID: "end", Source: "fake", StreamRef: ref, Value: []byte("opaque")}
	events <- api.SourceEvent{End: &api.StreamEnd{StreamRef: ref, EndToken: token, Revision: 1, CoverageStartedAt: pipelineTestCoverageStartedAt, Resource: resource, Outcome: outcome}}
	select {
	case <-source.ended:
	case <-time.After(2 * time.Second):
		t.Fatal("legacy end acknowledgement timed out")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := p.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, found, err := db.GetFinalizeIntent(ref.ID, 1)
	if err != nil || !found || !got.SourceDone || got.StreamKind != ref.Kind || got.Resource == nil || *got.Resource != resource || got.Outcome == nil || !equalSourceOutcome(*got.Outcome, outcome) || got.EndToken == nil || !equalEndToken(*got.EndToken, token) {
		t.Fatalf("enriched finalize intent = %+v found=%v err=%v", got, found, err)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.finalized) != 0 {
		t.Fatalf("Finalize calls = %d, want 0 for legacy SinkDone intent", len(sink.finalized))
	}
}

func TestPipelineCloseWhileRunIsSending(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	source := &fakeSource{acked: make(chan []api.AckResult, 128), ended: make(chan api.EndToken, 1)}
	sink := &fakeSink{}
	log, _ := logger.New(logger.Config{OutputPaths: []string{"stdout"}})
	p, err := New(testConfig(), source, sink, db, "target", log, nil)
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan api.SourceEvent, 128)
	streamRef := testStreamRef()
	for index := 0; index < 100; index++ {
		id := string(rune(index + 1))
		events <- api.SourceEvent{Delivery: &api.Delivery{StreamRef: streamRef, AckToken: api.AckToken{ID: id, Source: "fake", StreamRef: streamRef}, RecordID: id, Record: api.Record{Kind: api.RecordKindContainerLog, Body: []byte("record"), Resource: api.Resource{SandboxID: "sb"}}}}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx, events) }()
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer closeCancel()
	if err := p.Close(closeCtx); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pipeline Run did not stop")
	}
	p.budgetMu.Lock()
	defer p.budgetMu.Unlock()
	if p.globalBytes != 0 || len(p.sandboxBytes) != 0 {
		t.Fatalf("queue budget leaked: global=%d sandboxes=%v", p.globalBytes, p.sandboxBytes)
	}
}

func TestPipelineCloseTimeoutCancelsStalledSendAndRun(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	source := &fakeSource{acked: make(chan []api.AckResult, 1), ended: make(chan api.EndToken, 1)}
	sink := &fakeSink{consumeErr: errors.New("retryable failure"), consumeStart: make(chan struct{}, 1)}
	log, _ := logger.New(logger.Config{OutputPaths: []string{"stdout"}})
	p, err := New(testConfig(), source, sink, db, "target", log, nil)
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan api.SourceEvent, 4)
	runDone := make(chan error, 1)
	go func() { runDone <- p.Run(context.Background(), events) }()
	streamRef := testStreamRef()
	delivery := api.Delivery{StreamRef: streamRef, AckToken: api.AckToken{Source: "fake", StreamRef: streamRef}, Record: api.Record{Kind: api.RecordKindContainerLog, Body: []byte("record"), Resource: api.Resource{SandboxID: "sb"}}}
	for index := 0; index < 3; index++ {
		eventDelivery := delivery
		eventDelivery.AckToken.ID = fmt.Sprintf("ack-%d", index)
		eventDelivery.RecordID = fmt.Sprintf("record-%d", index)
		events <- api.SourceEvent{Delivery: &eventDelivery}
	}
	select {
	case <-sink.consumeStart:
	case <-time.After(2 * time.Second):
		t.Fatal("sink consume did not start")
	}
	wantQueued := int64(0)
	for index := 0; index < 3; index++ {
		eventDelivery := delivery
		eventDelivery.AckToken.ID = fmt.Sprintf("ack-%d", index)
		eventDelivery.RecordID = fmt.Sprintf("record-%d", index)
		wantQueued += eventBytes(&eventDelivery)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		p.budgetMu.Lock()
		queued := p.globalBytes
		p.budgetMu.Unlock()
		if queued == wantQueued {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("third send did not stall: queued=%d want=%d", queued, wantQueued)
		}
		time.Sleep(time.Millisecond)
	}

	closeCtx, closeCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer closeCancel()
	closeDone := make(chan error, 1)
	go func() { closeDone <- p.Close(closeCtx) }()
	select {
	case err := <-closeDone:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("close error=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pipeline Close remained blocked after its context expired")
	}
	select {
	case err := <-runDone:
		if err == nil {
			t.Fatal("pipeline Run unexpectedly succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pipeline Run remained active after Close canceled workers")
	}
	p.budgetMu.Lock()
	defer p.budgetMu.Unlock()
	if p.globalBytes != 0 || len(p.sandboxBytes) != 0 {
		t.Fatalf("queue budget leaked: global=%d sandboxes=%v", p.globalBytes, p.sandboxBytes)
	}
}

func TestCapabilitiesRequireEverySourceKind(t *testing.T) {
	first := api.RecordKind("first")
	second := api.RecordKind("second")
	if !compatible(
		api.Capabilities{RecordKinds: []api.RecordKind{first, second}},
		api.Capabilities{RecordKinds: []api.RecordKind{second, first}},
	) {
		t.Fatal("compatible() rejected a Sink that accepts every Source kind")
	}
	if compatible(
		api.Capabilities{RecordKinds: []api.RecordKind{first, second}},
		api.Capabilities{RecordKinds: []api.RecordKind{first}},
	) {
		t.Fatal("compatible() accepted a Sink missing a Source kind")
	}
}

func TestEventBytesIncludesStreamMetadata(t *testing.T) {
	delivery := &api.Delivery{Metadata: api.StreamMetadata{"format-key": "format-value"}}
	withoutMetadata := *delivery
	withoutMetadata.Metadata = nil
	if got, want := eventBytes(delivery)-eventBytes(&withoutMetadata), int64(len("format-key")+len("format-value")); got != want {
		t.Fatalf("metadata bytes = %d, want %d", got, want)
	}
}

func TestPipelineKeepsLimiterWhileSandboxHasAnotherWorker(t *testing.T) {
	p := &Pipeline{
		cfg:          Config{PerSandboxRateLimit: 1},
		limiters:     make(map[string]*rate.Limiter),
		limiterUsers: make(map[string]int),
	}
	p.retainLimiter("sandbox")
	p.retainLimiter("sandbox")
	want := p.limiter("sandbox")

	p.releaseLimiter("sandbox")
	if got := p.limiter("sandbox"); got != want {
		t.Fatal("finishing one stream reset another stream's sandbox limiter")
	}

	p.releaseLimiter("sandbox")
	if _, found := p.limiters["sandbox"]; found {
		t.Fatal("limiter remained after the sandbox's final worker stopped")
	}
}

func TestValidateSourceEventChecksStreamKindAndToken(t *testing.T) {
	ref := testStreamRef()
	valid := api.SourceEvent{Delivery: &api.Delivery{
		Record:    api.Record{Kind: ref.Kind},
		StreamRef: ref,
		AckToken:  api.AckToken{StreamRef: ref},
	}}
	if err := validateSourceEvent(valid); err != nil {
		t.Fatalf("validateSourceEvent() error = %v", err)
	}

	mismatchedKind := valid
	record := *valid.Delivery
	record.Record.Kind = "different"
	mismatchedKind.Delivery = &record
	if err := validateSourceEvent(mismatchedKind); err == nil {
		t.Fatal("validateSourceEvent() accepted a mismatched record kind")
	}

	mismatchedToken := valid
	record = *valid.Delivery
	record.AckToken.StreamRef.ID = "other"
	mismatchedToken.Delivery = &record
	if err := validateSourceEvent(mismatchedToken); err == nil {
		t.Fatal("validateSourceEvent() accepted a mismatched ack token")
	}

	end := api.SourceEvent{End: &api.StreamEnd{StreamRef: ref, EndToken: api.EndToken{StreamRef: api.StreamRef{ID: ref.ID, Kind: "different"}}}}
	if err := validateSourceEvent(end); err == nil {
		t.Fatal("validateSourceEvent() accepted a mismatched end token")
	}
}

func testConfig() Config {
	return Config{BatchMaxItems: 1, FlushInterval: time.Second, SinkTimeout: time.Second, RetryMaxInterval: time.Second, MemoryBudgetBytes: 1 << 20, PerSandboxQueueBytes: 1 << 19, DropPolicy: "block"}
}

func containsReason(reasons []string, want string) bool {
	for _, reason := range reasons {
		if reason == want {
			return true
		}
	}
	return false
}
