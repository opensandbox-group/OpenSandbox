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
	"github.com/alibaba/opensandbox/nodeagent/pkg/state"
)

var pipelineTestCoverageStartedAt = time.Date(2026, 7, 23, 9, 58, 0, 0, time.UTC)

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
	streamRef := api.StreamRef{ID: "stream"}
	events <- api.SourceEvent{End: &api.StreamEnd{StreamRef: streamRef, EndToken: api.EndToken{ID: "end", Source: "fake", StreamRef: streamRef}, Revision: 1, CoverageStartedAt: pipelineTestCoverageStartedAt}}
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
	streamRef := api.StreamRef{ID: "stream"}
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
	events <- api.SourceEvent{End: &api.StreamEnd{StreamRef: streamRef, EndToken: api.EndToken{ID: "end", Source: "fake", StreamRef: streamRef}, Revision: 1, CoverageStartedAt: pipelineTestCoverageStartedAt}}
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
	streamRef := api.StreamRef{ID: "stream"}
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
	streamRef := api.StreamRef{ID: "stream"}
	events <- api.SourceEvent{Delivery: &api.Delivery{StreamRef: streamRef, AckToken: api.AckToken{ID: "large", Source: "fake", StreamRef: streamRef}, RecordID: "large", Record: api.Record{Kind: api.RecordKindContainerLog, Body: make([]byte, 2048), Resource: api.Resource{SandboxID: "sb"}}}}
	select {
	case results := <-source.acked:
		if len(results) != 1 || results[0].Disposition != api.AckIntentionalDrop || results[0].Reason != "pipeline-record-too-large" {
			t.Fatalf("results=%+v", results)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("drop acknowledgement timed out")
	}
	events <- api.SourceEvent{End: &api.StreamEnd{StreamRef: streamRef, EndToken: api.EndToken{ID: "end", Source: "fake", StreamRef: streamRef}, Revision: 1, CoverageStartedAt: pipelineTestCoverageStartedAt}}
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
	streamRef := api.StreamRef{ID: "stream"}
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
	streamRef := api.StreamRef{ID: "stream"}
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
	streamRef := api.StreamRef{ID: "stream"}
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
	streamRef := api.StreamRef{ID: "stream"}
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
	streamRef := api.StreamRef{ID: "stream"}
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
	streamRef := api.StreamRef{ID: "stream"}
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
	streamRef := api.StreamRef{ID: "stream"}
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
	streamRef := api.StreamRef{ID: "stream"}
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
	streamRef := api.StreamRef{ID: "stream"}

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
	streamRef := api.StreamRef{ID: "stream"}
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
	streamRef := api.StreamRef{ID: "stream"}
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
	streamRef := api.StreamRef{ID: "stream"}
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
