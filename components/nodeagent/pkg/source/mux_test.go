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

package source

import (
	"context"
	"errors"
	"testing"

	"github.com/alibaba/opensandbox/nodeagent/pkg/api"
)

type fakeSource struct {
	kind            api.RecordKind
	events          chan<- api.SourceEvent
	startErr        error
	acks            []api.AckResult
	endTokens       []api.EndToken
	stopped         bool
	stopCalls       int
	stopSawCanceled bool
	runCtx          context.Context
}

func (s *fakeSource) Capabilities() api.Capabilities {
	return api.Capabilities{RecordKinds: []api.RecordKind{s.kind}}
}
func (s *fakeSource) Start(ctx context.Context, events chan<- api.SourceEvent) error {
	s.runCtx = ctx
	s.events = events
	return s.startErr
}
func (s *fakeSource) Acknowledge(_ context.Context, results []api.AckResult) error {
	s.acks = append(s.acks, results...)
	return nil
}
func (s *fakeSource) AcknowledgeEnd(_ context.Context, token api.EndToken) error {
	s.endTokens = append(s.endTokens, token)
	return nil
}
func (s *fakeSource) Stop(context.Context) error {
	s.stopped = true
	s.stopCalls++
	s.stopSawCanceled = s.runCtx != nil && s.runCtx.Err() != nil
	return nil
}

func TestMuxForwardsAndRoutesBySource(t *testing.T) {
	first := &fakeSource{kind: "first-kind"}
	second := &fakeSource{kind: "second-kind"}
	mux, err := NewMux([]Child{{Name: "first", Source: first}, {Name: "second", Source: second}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan api.SourceEvent)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mux.Start(ctx, events); err != nil {
		t.Fatal(err)
	}
	if first.events == second.events {
		t.Fatal("Sources received the same event channel")
	}

	firstRef := api.StreamRef{ID: "first/stream", Kind: "first-kind"}
	first.events <- api.SourceEvent{Delivery: &api.Delivery{
		Record:    api.Record{Kind: "first-kind"},
		StreamRef: firstRef,
		AckToken:  api.AckToken{ID: "ack", Source: "first", StreamRef: firstRef},
	}}
	forwarded := <-events
	if got := forwarded.Delivery.StreamRef.Kind; got != "first-kind" {
		t.Fatalf("forwarded stream kind = %q, want first-kind", got)
	}
	if err := mux.Acknowledge(context.Background(), []api.AckResult{{Token: forwarded.Delivery.AckToken}}); err != nil {
		t.Fatal(err)
	}
	if len(first.acks) != 1 || len(second.acks) != 0 {
		t.Fatalf("ack routing counts = first:%d second:%d", len(first.acks), len(second.acks))
	}

	secondRef := api.StreamRef{ID: "second/stream", Kind: "second-kind"}
	token := api.EndToken{ID: "end", Source: "second", StreamRef: secondRef}
	if err := mux.AcknowledgeEnd(context.Background(), token); err != nil {
		t.Fatal(err)
	}
	if len(first.endTokens) != 0 || len(second.endTokens) != 1 {
		t.Fatalf("end routing counts = first:%d second:%d", len(first.endTokens), len(second.endTokens))
	}

	if err := mux.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !first.stopped || !second.stopped {
		t.Fatal("Stop() did not stop every child")
	}
	if !first.stopSawCanceled || !second.stopSawCanceled {
		t.Fatal("Stop() called a child before canceling its run context")
	}
}

func TestMuxStopsStartedSourcesWhenLaterStartFails(t *testing.T) {
	first := &fakeSource{kind: "first-kind"}
	wantErr := errors.New("start failed")
	second := &fakeSource{kind: "second-kind", startErr: wantErr}
	mux, err := NewMux([]Child{{Name: "first", Source: first}, {Name: "second", Source: second}}, nil)
	if err != nil {
		t.Fatal(err)
	}

	err = mux.Start(context.Background(), make(chan api.SourceEvent))
	if !errors.Is(err, wantErr) {
		t.Fatalf("Start() error = %v, want %v", err, wantErr)
	}
	if !first.stopped || !first.stopSawCanceled {
		t.Fatal("Start() did not cancel and stop the previously started Source")
	}
	if second.stopped {
		t.Fatal("Start() stopped a Source whose Start did not succeed")
	}
	if err := mux.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if first.stopCalls != 1 {
		t.Fatalf("started Source Stop() calls = %d, want 1", first.stopCalls)
	}
	if second.stopCalls != 0 {
		t.Fatalf("failed Source Stop() calls = %d, want 0", second.stopCalls)
	}
}

func TestMuxRejectsUnknownTokenOwner(t *testing.T) {
	mux, err := NewMux([]Child{{Name: "known", Source: &fakeSource{kind: "kind"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := mux.Acknowledge(context.Background(), []api.AckResult{{Token: api.AckToken{Source: "unknown"}}}); err == nil {
		t.Fatal("Acknowledge() unexpectedly succeeded")
	} else if api.IsRetryableError(err) {
		t.Fatalf("Acknowledge() error = %v, want permanent error", err)
	}
	if err := mux.AcknowledgeEnd(context.Background(), api.EndToken{Source: "unknown"}); err == nil {
		t.Fatal("AcknowledgeEnd() unexpectedly succeeded")
	}
}

func TestMuxRejectsDuplicateSources(t *testing.T) {
	_, err := NewMux([]Child{{Name: "same", Source: &fakeSource{kind: "one"}}, {Name: "same", Source: &fakeSource{kind: "two"}}}, nil)
	if err == nil {
		t.Fatal("NewMux() unexpectedly accepted duplicate Sources")
	}
	if _, err := NewMux([]Child{{Name: "nested/source", Source: &fakeSource{kind: "one"}}}, nil); err == nil {
		t.Fatal("NewMux() unexpectedly accepted a Source name containing a path separator")
	}
}

func TestMuxRejectsMismatchedEvent(t *testing.T) {
	source := &fakeSource{kind: "kind"}
	errCh := make(chan error, 1)
	mux, err := NewMux([]Child{{Name: "source", Source: source}}, func(err error) { errCh <- err })
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan api.SourceEvent)
	if err := mux.Start(context.Background(), events); err != nil {
		t.Fatal(err)
	}
	ref := api.StreamRef{ID: "source/stream", Kind: "kind"}
	source.events <- api.SourceEvent{Delivery: &api.Delivery{
		Record:    api.Record{Kind: "different-kind"},
		StreamRef: ref,
		AckToken:  api.AckToken{Source: "source", StreamRef: ref},
	}}
	if err := <-errCh; err == nil {
		t.Fatal("mismatched event did not fail the mux")
	}
	if err := mux.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestMuxRequiresSourceNamespacedStreamIDs(t *testing.T) {
	children := []Child{
		{Name: "first", Source: &fakeSource{kind: "kind"}},
		{Name: "second", Source: &fakeSource{kind: "kind"}},
	}
	for _, child := range children {
		ref := api.StreamRef{ID: "shared", Kind: "kind"}
		_, err := normalizeEvent(child, child.Source.Capabilities().RecordKinds, api.SourceEvent{Delivery: &api.Delivery{
			Record:    api.Record{Kind: "kind"},
			StreamRef: ref,
			AckToken:  api.AckToken{Source: child.Name, StreamRef: ref},
		}})
		if err == nil {
			t.Fatalf("source %q emitted an unqualified shared stream ID", child.Name)
		}
	}

	var normalized []api.StreamRef
	for _, child := range children {
		ref := api.StreamRef{ID: child.Name + "/shared", Kind: "kind"}
		event, err := normalizeEvent(child, child.Source.Capabilities().RecordKinds, api.SourceEvent{Delivery: &api.Delivery{
			Record:    api.Record{Kind: "kind"},
			StreamRef: ref,
			AckToken:  api.AckToken{Source: child.Name, StreamRef: ref},
		}})
		if err != nil {
			t.Fatalf("source %q namespaced event: %v", child.Name, err)
		}
		normalized = append(normalized, event.Delivery.StreamRef)
	}
	if normalized[0].ID == normalized[1].ID {
		t.Fatal("Source namespaces did not isolate the same local stream ID")
	}
	if sourceOwnsStream("first", "first/") {
		t.Fatal("empty Source-local stream ID was accepted")
	}
}

func TestMuxRejectsAcknowledgementsFromMultipleSources(t *testing.T) {
	first := &fakeSource{kind: "first-kind"}
	second := &fakeSource{kind: "second-kind"}
	mux, err := NewMux([]Child{{Name: "first", Source: first}, {Name: "second", Source: second}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	results := []api.AckResult{
		{Token: api.AckToken{Source: "first"}},
		{Token: api.AckToken{Source: "second"}},
	}
	if err := mux.Acknowledge(context.Background(), results); err == nil || api.IsRetryableError(err) {
		t.Fatalf("Acknowledge() error = %v, want permanent error", err)
	}
	if len(first.acks) != 0 || len(second.acks) != 0 {
		t.Fatal("Acknowledge() forwarded part of a mixed-Source batch")
	}
}
