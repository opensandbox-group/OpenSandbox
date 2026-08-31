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

// Package source coordinates the Sources compiled into Node Agent.
package source

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/alibaba/opensandbox/nodeagent/pkg/api"
)

type Child struct {
	Name   string
	Source api.Source
}

type Mux struct {
	children     []Child
	byName       map[string]api.Source
	kindsByName  map[string][]api.RecordKind
	capabilities api.Capabilities
	onError      func(error)

	mu      sync.Mutex
	started bool
	cancel  context.CancelFunc
	active  []Child
	done    chan struct{}
}

func NewMux(children []Child, onError func(error)) (*Mux, error) {
	if len(children) == 0 {
		return nil, errors.New("source mux requires at least one Source")
	}
	mux := &Mux{
		children:    append([]Child(nil), children...),
		byName:      make(map[string]api.Source, len(children)),
		kindsByName: make(map[string][]api.RecordKind, len(children)),
		onError:     onError,
		done:        make(chan struct{}),
	}
	kinds := make(map[api.RecordKind]struct{})
	for _, child := range mux.children {
		if child.Name == "" || strings.Contains(child.Name, "/") || child.Source == nil {
			return nil, errors.New("source mux child requires a name and Source")
		}
		if _, exists := mux.byName[child.Name]; exists {
			return nil, fmt.Errorf("source mux contains duplicate Source %q", child.Name)
		}
		mux.byName[child.Name] = child.Source
		childKinds := child.Source.Capabilities().RecordKinds
		if len(childKinds) == 0 {
			return nil, fmt.Errorf("source %q declares no record kinds", child.Name)
		}
		mux.kindsByName[child.Name] = append([]api.RecordKind(nil), childKinds...)
		for _, kind := range childKinds {
			if kind == "" {
				return nil, fmt.Errorf("source %q declares an empty record kind", child.Name)
			}
			if _, exists := kinds[kind]; exists {
				continue
			}
			kinds[kind] = struct{}{}
			mux.capabilities.RecordKinds = append(mux.capabilities.RecordKinds, kind)
		}
	}
	return mux, nil
}

func (m *Mux) Capabilities() api.Capabilities {
	return api.Capabilities{RecordKinds: append([]api.RecordKind(nil), m.capabilities.RecordKinds...)}
}

func (m *Mux) Start(ctx context.Context, out chan<- api.SourceEvent) error {
	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		return errors.New("source mux already started")
	}
	runCtx, cancel := context.WithCancel(ctx)
	m.started = true
	m.cancel = cancel
	m.mu.Unlock()

	type childInput struct {
		child  Child
		events chan api.SourceEvent
	}
	inputs := make([]childInput, len(m.children))
	var forwarders sync.WaitGroup
	forwarders.Add(len(inputs))
	for index, child := range m.children {
		inputs[index] = childInput{child: child, events: make(chan api.SourceEvent)}
		go func(input childInput) {
			defer forwarders.Done()
			m.forward(runCtx, input.child, input.events, out)
		}(inputs[index])
	}
	go func() {
		forwarders.Wait()
		close(out)
		close(m.done)
	}()

	started := make([]Child, 0, len(inputs))
	for _, input := range inputs {
		if err := input.child.Source.Start(runCtx, input.events); err != nil {
			cancel()
			stopErr := stopChildren(ctx, started)
			<-m.done
			m.mu.Lock()
			m.active = nil
			m.mu.Unlock()
			return errors.Join(fmt.Errorf("start source %q: %w", input.child.Name, err), stopErr)
		}
		started = append(started, input.child)
		m.mu.Lock()
		m.active = append(m.active, input.child)
		m.mu.Unlock()
	}
	return nil
}

func (m *Mux) Acknowledge(ctx context.Context, results []api.AckResult) error {
	if len(results) == 0 {
		return nil
	}
	name := results[0].Token.Source
	child := m.byName[name]
	if child == nil {
		return api.Permanent(fmt.Errorf("ack token refers to unknown Source %q", name))
	}
	for _, result := range results[1:] {
		if result.Token.Source != name {
			return api.Permanent(errors.New("acknowledgement batch contains multiple Sources"))
		}
	}
	return child.Acknowledge(ctx, results)
}

func (m *Mux) AcknowledgeEnd(ctx context.Context, token api.EndToken) error {
	child := m.byName[token.Source]
	if child == nil {
		return api.Permanent(fmt.Errorf("end token refers to unknown Source %q", token.Source))
	}
	return child.AcknowledgeEnd(ctx, token)
}

func (m *Mux) Stop(ctx context.Context) error {
	m.mu.Lock()
	if !m.started {
		m.mu.Unlock()
		return nil
	}
	cancel := m.cancel
	active := append([]Child(nil), m.active...)
	m.active = nil
	m.mu.Unlock()
	cancel()
	stopErr := stopChildren(ctx, active)
	select {
	case <-m.done:
		return stopErr
	case <-ctx.Done():
		return errors.Join(stopErr, ctx.Err())
	}
}

func (m *Mux) forward(ctx context.Context, child Child, events <-chan api.SourceEvent, out chan<- api.SourceEvent) {
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-events:
			if !ok {
				if ctx.Err() == nil {
					m.fail(fmt.Errorf("source %q event channel closed unexpectedly", child.Name))
				}
				return
			}
			normalized, err := normalizeEvent(child, m.kindsByName[child.Name], event)
			if err != nil {
				m.fail(err)
				return
			}
			select {
			case out <- normalized:
			case <-ctx.Done():
				return
			}
		}
	}
}

func (m *Mux) fail(err error) {
	if m.onError != nil {
		m.onError(err)
	}
	m.mu.Lock()
	cancel := m.cancel
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func normalizeEvent(child Child, kinds []api.RecordKind, event api.SourceEvent) (api.SourceEvent, error) {
	if !event.Valid() {
		return api.SourceEvent{}, fmt.Errorf("source %q emitted an invalid event", child.Name)
	}
	if event.Delivery != nil {
		delivery := *event.Delivery
		if !sourceOwnsStream(child.Name, delivery.StreamRef.ID) {
			return api.SourceEvent{}, fmt.Errorf("source %q emitted non-namespaced stream ID %q", child.Name, delivery.StreamRef.ID)
		}
		kind := delivery.StreamRef.Kind
		if !containsKind(kinds, kind) {
			return api.SourceEvent{}, fmt.Errorf("source %q emitted unsupported record kind %q", child.Name, kind)
		}
		if delivery.Record.Kind != kind {
			return api.SourceEvent{}, fmt.Errorf("source %q emitted record kind %q for stream kind %q", child.Name, delivery.Record.Kind, kind)
		}
		if delivery.AckToken.Source != child.Name {
			return api.SourceEvent{}, fmt.Errorf("source %q emitted ack token owned by %q", child.Name, delivery.AckToken.Source)
		}
		if delivery.AckToken.StreamRef != delivery.StreamRef {
			return api.SourceEvent{}, fmt.Errorf("source %q emitted an ack token for a different stream", child.Name)
		}
		return api.SourceEvent{Delivery: &delivery}, nil
	}

	end := *event.End
	if !sourceOwnsStream(child.Name, end.StreamRef.ID) {
		return api.SourceEvent{}, fmt.Errorf("source %q emitted non-namespaced stream ID %q", child.Name, end.StreamRef.ID)
	}
	kind := end.StreamRef.Kind
	if !containsKind(kinds, kind) {
		return api.SourceEvent{}, fmt.Errorf("source %q emitted unsupported stream kind %q", child.Name, kind)
	}
	if end.EndToken.Source != child.Name {
		return api.SourceEvent{}, fmt.Errorf("source %q emitted end token owned by %q", child.Name, end.EndToken.Source)
	}
	if end.EndToken.StreamRef != end.StreamRef {
		return api.SourceEvent{}, fmt.Errorf("source %q emitted an end token for a different stream", child.Name)
	}
	return api.SourceEvent{End: &end}, nil
}

func sourceOwnsStream(source, streamID string) bool {
	prefix, localID, found := strings.Cut(streamID, "/")
	return found && prefix == source && localID != ""
}

func containsKind(kinds []api.RecordKind, target api.RecordKind) bool {
	for _, kind := range kinds {
		if kind == target {
			return true
		}
	}
	return false
}

func stopChildren(ctx context.Context, children []Child) error {
	results := make(chan error, len(children))
	for _, child := range children {
		go func(child Child) { results <- child.Source.Stop(ctx) }(child)
	}
	var errs []error
	for range children {
		errs = append(errs, <-results)
	}
	return errors.Join(errs...)
}
