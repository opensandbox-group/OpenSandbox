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
	"math/rand/v2"
	"sync"
	"time"

	"github.com/alibaba/opensandbox/internal/logger"
	"github.com/alibaba/opensandbox/internal/safego"
	"github.com/alibaba/opensandbox/nodeagent/pkg/api"
	"github.com/alibaba/opensandbox/nodeagent/pkg/identity"
	"github.com/alibaba/opensandbox/nodeagent/pkg/state"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"golang.org/x/time/rate"
)

type finalizeStore interface {
	GetFinalizeIntent(streamRef string, revision uint64) (state.FinalizeIntent, bool, error)
	PutFinalizeIntent(state.FinalizeIntent) error
	ListFinalizeIntents() ([]state.FinalizeIntent, error)
	GetSourceStream(streamRef string) (state.SourceStream, bool, error)
}

type Config struct {
	BatchMaxItems        int
	FlushInterval        time.Duration
	SinkTimeout          time.Duration
	RetryMaxInterval     time.Duration
	OnRetryStateChange   func(bool)
	MemoryBudgetBytes    int64
	PerSandboxQueueBytes int64
	PerSandboxRateLimit  float64
	DropPolicy           string
}

const perStreamQueueSize = 1

type Pipeline struct {
	cfg      Config
	source   api.Source
	sink     api.Sink
	state    finalizeStore
	targetID string
	log      logger.Logger
	onError  func(error)

	mu             sync.Mutex
	workers        map[string]*worker
	handoffs       map[string]<-chan struct{}
	wg             sync.WaitGroup
	activeSends    sync.WaitGroup
	closed         bool
	workerCtx      context.Context
	cancelWorkers  context.CancelFunc
	workerErrors   chan error
	retryMu        sync.Mutex
	activeRetries  int
	retryNotified  bool
	retryNotifying bool

	budgetMu      sync.Mutex
	globalBytes   int64
	sandboxBytes  map[string]int64
	budgetChanged chan struct{}
	limiters      map[string]*rate.Limiter
	metrics       pipelineMetrics
}

type pipelineMetrics struct {
	records       metric.Int64Counter
	bytes         metric.Int64Counter
	drops         metric.Int64Counter
	retries       metric.Int64Counter
	queueBytes    metric.Int64UpDownCounter
	consumeMillis metric.Float64Histogram
}

type worker struct {
	streamRef   api.StreamRef
	input       chan admittedEvent
	done        chan struct{}
	predecessor <-chan struct{}
	outcomeMu   sync.Mutex
	dropOutcome api.SourceOutcome
}

type admittedEvent struct {
	event     api.SourceEvent
	bytes     int64
	sandboxID string
}

type pending struct {
	item      api.BatchItem
	token     api.AckToken
	bytes     int64
	sandboxID string
}

type retryOperation struct {
	call             func(context.Context) error
	timeout          time.Duration
	nonRetryableText string
	retryText        string
	scope            *retryScope
}

type retryScope struct {
	end func()
}

func (s *retryScope) activate(p *Pipeline) {
	if s.end == nil {
		s.end = p.beginRetry()
	}
}

func (s *retryScope) close() {
	if s.end != nil {
		s.end()
		s.end = nil
	}
}

func New(cfg Config, source api.Source, sink api.Sink, store finalizeStore, targetID string, log logger.Logger, onError func(error)) (*Pipeline, error) {
	if cfg.BatchMaxItems <= 0 || cfg.FlushInterval <= 0 || cfg.SinkTimeout <= 0 || cfg.RetryMaxInterval <= 0 || cfg.MemoryBudgetBytes <= 0 || cfg.PerSandboxQueueBytes <= 0 {
		return nil, errors.New("pipeline limits and durations must be positive")
	}
	if cfg.PerSandboxQueueBytes > cfg.MemoryBudgetBytes {
		return nil, errors.New("per-sandbox queue budget exceeds global memory budget")
	}
	if cfg.DropPolicy != "block" && cfg.DropPolicy != "drop" {
		return nil, errors.New("pipeline drop policy must be block or drop")
	}
	if !compatible(source.Capabilities(), sink.Capabilities()) {
		return nil, errors.New("source and sink record kinds are incompatible")
	}
	workerCtx, cancelWorkers := context.WithCancel(context.Background())
	metrics, err := newPipelineMetrics()
	if err != nil {
		cancelWorkers()
		return nil, err
	}
	return &Pipeline{cfg: cfg, source: source, sink: sink, state: store, targetID: targetID, log: log.Named("pipeline"), onError: onError, workers: make(map[string]*worker), handoffs: make(map[string]<-chan struct{}), workerCtx: workerCtx, cancelWorkers: cancelWorkers, workerErrors: make(chan error, 1), sandboxBytes: make(map[string]int64), budgetChanged: make(chan struct{}, 1), limiters: make(map[string]*rate.Limiter), metrics: metrics}, nil
}

func (p *Pipeline) Run(ctx context.Context, events <-chan api.SourceEvent) error {
	if err := p.reconcileFinalizeIntents(); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-p.workerCtx.Done():
			select {
			case err := <-p.workerErrors:
				return err
			default:
				return p.workerCtx.Err()
			}
		case err := <-p.workerErrors:
			return err
		case event, ok := <-events:
			if !ok {
				if err := ctx.Err(); err != nil {
					return err
				}
				return errors.New("source event channel closed unexpectedly")
			}
			if !event.Valid() {
				return errors.New("invalid source event")
			}
			streamRef := eventStream(event)
			worker, err := p.getWorker(streamRef)
			if err != nil {
				return err
			}
			err = p.sendEvent(ctx, worker, event)
			p.activeSends.Done()
			if err != nil {
				return err
			}
			if event.End != nil {
				p.retireWorker(worker)
			}
		}
	}
}

func (p *Pipeline) sendEvent(ctx context.Context, worker *worker, event api.SourceEvent) error {
	sendCtx, cancel := context.WithCancel(ctx)
	stopCancel := context.AfterFunc(p.workerCtx, cancel)
	defer func() {
		stopCancel()
		cancel()
	}()
	admitted, err := p.admit(sendCtx, worker, event)
	if err != nil {
		return err
	}
	if admitted == nil {
		return nil
	}
	select {
	case worker.input <- *admitted:
	case <-worker.done:
		p.release(admitted.bytes, admitted.sandboxID)
		return fmt.Errorf("stream worker %s stopped", worker.streamRef.ID)
	case <-sendCtx.Done():
		p.release(admitted.bytes, admitted.sandboxID)
		return sendCtx.Err()
	}
	return nil
}

func (p *Pipeline) reconcileFinalizeIntents() error {
	intents, err := p.state.ListFinalizeIntents()
	if err != nil {
		return err
	}
	for _, intent := range intents {
		if intent.TargetID != p.targetID || intent.FinalizeID != identity.FinalizeID(intent.StreamRef, intent.Revision, p.targetID) {
			return fmt.Errorf("finalize intent identity mismatch for stream %s revision %d", intent.StreamRef, intent.Revision)
		}
		if intent.SourceDone && !intent.SinkDone {
			return fmt.Errorf("finalize intent for stream %s revision %d completed Source before Sink", intent.StreamRef, intent.Revision)
		}
		if intent.SourceDone {
			continue
		}
		stream, found, err := p.state.GetSourceStream(intent.StreamRef)
		if err != nil {
			return err
		}
		if !found || stream.AcknowledgedRevision < intent.Revision {
			continue
		}
		if !intent.SinkDone {
			return fmt.Errorf("source revision %d is acknowledged without a durable Sink completion", intent.Revision)
		}
		intent.SourceDone = true
		if err := p.state.PutFinalizeIntent(intent); err != nil {
			return err
		}
	}
	return nil
}

func (p *Pipeline) getWorker(streamRef api.StreamRef) (*worker, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, errors.New("pipeline is closing")
	}
	p.activeSends.Add(1)
	if existing := p.workers[streamRef.ID]; existing != nil {
		return existing, nil
	}
	predecessor := p.handoffs[streamRef.ID]
	delete(p.handoffs, streamRef.ID)
	w := &worker{streamRef: streamRef, input: make(chan admittedEvent, perStreamQueueSize), done: make(chan struct{}), predecessor: predecessor}
	p.workers[streamRef.ID] = w
	p.wg.Add(1)
	safego.Go(func() {
		defer p.wg.Done()
		defer func() {
			close(w.done)
			p.clearHandoff(w)
		}()
		if err := p.runWorker(p.workerCtx, w); err != nil && !errors.Is(err, context.Canceled) {
			p.fail(err)
			select {
			case p.workerErrors <- err:
			default:
			}
		}
	})
	return w, nil
}

func (p *Pipeline) runWorker(ctx context.Context, worker *worker) error {
	var items []pending
	defer func() {
		for _, item := range items {
			p.release(item.bytes, item.sandboxID)
		}
		for {
			select {
			case admitted, ok := <-worker.input:
				if !ok {
					return
				}
				p.release(admitted.bytes, admitted.sandboxID)
			default:
				return
			}
		}
	}()
	if worker.predecessor != nil {
		select {
		case <-worker.predecessor:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	timer := time.NewTimer(p.cfg.FlushInterval)
	defer timer.Stop()
	flush := func() error {
		if len(items) == 0 {
			return nil
		}
		if err := p.consumeWithRetry(ctx, worker.streamRef, items); err != nil {
			return err
		}
		for _, item := range items {
			p.release(item.bytes, item.sandboxID)
		}
		items = items[:0]
		return nil
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			if err := flush(); err != nil {
				return err
			}
			timer.Reset(p.cfg.FlushInterval)
		case admitted, ok := <-worker.input:
			if !ok {
				return flush()
			}
			event := admitted.event
			if event.Delivery != nil {
				items = append(items, pending{item: api.BatchItem{Record: event.Delivery.Record, RecordID: event.Delivery.RecordID}, token: event.Delivery.AckToken, bytes: admitted.bytes, sandboxID: admitted.sandboxID})
				if len(items) >= p.cfg.BatchMaxItems {
					if err := flush(); err != nil {
						return err
					}
					if !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
					}
					timer.Reset(p.cfg.FlushInterval)
				}
				continue
			}
			if err := flush(); err != nil {
				return err
			}
			if err := p.finalize(ctx, worker, event.End); err != nil {
				return err
			}
			return nil
		}
	}
}

func (p *Pipeline) consumeWithRetry(ctx context.Context, streamRef api.StreamRef, pendingItems []pending) error {
	scope := &retryScope{}
	defer scope.close()
	batch := api.Batch{StreamRef: streamRef, Items: make([]api.BatchItem, len(pendingItems))}
	for i := range pendingItems {
		batch.Items[i] = pendingItems[i].item
	}
	err := p.retry(ctx, retryOperation{
		call: func(callCtx context.Context) error {
			started := time.Now()
			err := p.sink.Consume(callCtx, batch)
			p.metrics.consumeMillis.Record(context.Background(), float64(time.Since(started).Microseconds())/1000)
			return err
		},
		timeout:          p.cfg.SinkTimeout,
		nonRetryableText: "non-retryable sink consume failure",
		retryText:        "sink consume failed; retrying",
		scope:            scope,
	})
	if err != nil {
		return err
	}
	results := make([]api.AckResult, len(pendingItems))
	for i := range pendingItems {
		results[i] = api.AckResult{Token: pendingItems[i].token, Disposition: api.AckDelivered, Guarantee: p.sink.Guarantee()}
	}
	return p.acknowledgeWithRetry(ctx, results, scope)
}

func (p *Pipeline) acknowledgeWithRetry(ctx context.Context, results []api.AckResult, scope *retryScope) error {
	return p.retry(ctx, retryOperation{
		call:             func(callCtx context.Context) error { return p.source.Acknowledge(callCtx, results) },
		nonRetryableText: "non-retryable source acknowledge failure",
		retryText:        "source acknowledge failed; retrying without re-consuming",
		scope:            scope,
	})
}

func (p *Pipeline) finalize(ctx context.Context, worker *worker, end *api.StreamEnd) error {
	if end.CoverageStartedAt.IsZero() || end.CoverageStartedAt.Location() != time.UTC || end.CoverageStartedAt.Nanosecond() != 0 {
		return errors.New("stream end has an invalid coverage boundary")
	}
	finalizeID := identity.FinalizeID(end.StreamRef.ID, end.Revision, p.targetID)
	intent, found, err := p.state.GetFinalizeIntent(end.StreamRef.ID, end.Revision)
	if err != nil {
		return err
	}
	if !found {
		finalizedAt := time.Now().UTC().Truncate(time.Second)
		if finalizedAt.Before(end.CoverageStartedAt) {
			finalizedAt = end.CoverageStartedAt
		}
		intent = state.FinalizeIntent{FinalizeID: finalizeID, TargetID: p.targetID, StreamRef: end.StreamRef.ID, Revision: end.Revision, CoverageStartedAt: end.CoverageStartedAt, FinalizedAt: finalizedAt}
		if err := p.state.PutFinalizeIntent(intent); err != nil {
			return err
		}
	} else if intent.FinalizeID != finalizeID || intent.TargetID != p.targetID || !intent.CoverageStartedAt.Equal(end.CoverageStartedAt) {
		return errors.New("finalize intent identity mismatch")
	}
	outcome := worker.mergeDropOutcome(end.Outcome)
	request := api.FinalizeRequest{FinalizeID: intent.FinalizeID, TargetID: intent.TargetID, StreamRef: end.StreamRef, Revision: end.Revision, CoverageStartedAt: intent.CoverageStartedAt, Resource: end.Resource, Outcome: outcome, FinalizedAt: intent.FinalizedAt}
	if !intent.SinkDone {
		if err := p.finalizeSinkWithRetry(ctx, request); err != nil {
			return err
		}
		intent.SinkDone = true
		if err := p.state.PutFinalizeIntent(intent); err != nil {
			return err
		}
	}
	if !intent.SourceDone {
		if err := p.acknowledgeEndWithRetry(ctx, end.EndToken); err != nil {
			return err
		}
		intent.SourceDone = true
		if err := p.state.PutFinalizeIntent(intent); err != nil {
			return err
		}
	}
	p.budgetMu.Lock()
	delete(p.limiters, end.Resource.SandboxID)
	p.budgetMu.Unlock()
	return nil
}

func (p *Pipeline) acknowledgeEndWithRetry(ctx context.Context, token api.EndToken) error {
	return p.retry(ctx, retryOperation{
		call:             func(callCtx context.Context) error { return p.source.AcknowledgeEnd(callCtx, token) },
		nonRetryableText: "non-retryable source end acknowledgement failure",
		retryText:        "source end acknowledgement failed; retrying",
	})
}

func (p *Pipeline) finalizeSinkWithRetry(ctx context.Context, request api.FinalizeRequest) error {
	return p.retry(ctx, retryOperation{
		call:             func(callCtx context.Context) error { return p.sink.Finalize(callCtx, request) },
		nonRetryableText: "non-retryable sink finalize failure",
		retryText:        "sink finalize failed; retrying",
	})
}

func (p *Pipeline) retry(ctx context.Context, operation retryOperation) error {
	scope := operation.scope
	if scope == nil {
		scope = &retryScope{}
		defer scope.close()
	}
	delay := 100 * time.Millisecond
	for {
		callCtx := ctx
		cancel := func() {}
		if operation.timeout > 0 {
			callCtx, cancel = context.WithTimeout(ctx, operation.timeout)
		}
		err := operation.call(callCtx)
		cancel()
		if err == nil {
			return nil
		}
		if !api.IsRetryableError(err) {
			return fmt.Errorf("%s: %w", operation.nonRetryableText, err)
		}
		scope.activate(p)
		p.log.Warnf("%s: %v", operation.retryText, err)
		p.metrics.retries.Add(context.Background(), 1)
		jitter := time.Duration(rand.Int64N(max(int64(delay/4), 1)))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay + jitter):
		}
		delay *= 2
		if delay > p.cfg.RetryMaxInterval {
			delay = p.cfg.RetryMaxInterval
		}
	}
}

func (p *Pipeline) beginRetry() func() {
	p.changeRetryCount(1)
	return func() { p.changeRetryCount(-1) }
}

func (p *Pipeline) changeRetryCount(delta int) {
	p.retryMu.Lock()
	p.activeRetries += delta
	if p.activeRetries < 0 {
		p.retryMu.Unlock()
		panic("pipeline retry state underflow")
	}
	shouldNotify := !p.retryNotifying && (p.activeRetries > 0) != p.retryNotified
	if shouldNotify {
		p.retryNotifying = true
	}
	p.retryMu.Unlock()
	if shouldNotify {
		p.drainRetryStateChanges()
	}
}

func (p *Pipeline) drainRetryStateChanges() {
	for {
		p.retryMu.Lock()
		active := p.activeRetries > 0
		if active == p.retryNotified {
			p.retryNotifying = false
			p.retryMu.Unlock()
			return
		}
		p.retryNotified = active
		callback := p.cfg.OnRetryStateChange
		p.retryMu.Unlock()
		if callback != nil {
			callback(active)
		}
	}
}

func (p *Pipeline) retireWorker(worker *worker) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.workers[worker.streamRef.ID] == worker {
		delete(p.workers, worker.streamRef.ID)
		select {
		case <-worker.done:
		default:
			p.handoffs[worker.streamRef.ID] = worker.done
		}
	}
}

func (p *Pipeline) clearHandoff(worker *worker) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.handoffs[worker.streamRef.ID] == worker.done {
		delete(p.handoffs, worker.streamRef.ID)
	}
}

func (p *Pipeline) Close(ctx context.Context) error {
	p.mu.Lock()
	shouldClose := false
	if !p.closed {
		p.closed = true
		shouldClose = true
	}
	p.mu.Unlock()
	var closeErr error
	if shouldClose {
		sendsDone := make(chan struct{})
		go func() {
			p.activeSends.Wait()
			close(sendsDone)
		}()
		select {
		case <-sendsDone:
		case <-ctx.Done():
			closeErr = ctx.Err()
			p.cancelWorkers()
			<-sendsDone
		}
		drainCanceledWorkers := p.workerCtx.Err() != nil
		p.mu.Lock()
		for _, worker := range p.workers {
			close(worker.input)
			if drainCanceledWorkers {
				for admitted := range worker.input {
					p.release(admitted.bytes, admitted.sandboxID)
				}
			}
		}
		p.mu.Unlock()
	}
	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()
	if closeErr != nil {
		<-done
		return errors.Join(closeErr, p.sink.Close(ctx))
	}
	select {
	case <-done:
		p.cancelWorkers()
		return p.sink.Close(ctx)
	case <-ctx.Done():
		p.cancelWorkers()
		<-done
		return errors.Join(ctx.Err(), p.sink.Close(ctx))
	}
}

func (p *Pipeline) fail(err error) {
	p.log.Errorf("%v", err)
	if p.onError != nil {
		p.onError(err)
	}
}

func (p *Pipeline) admit(ctx context.Context, worker *worker, event api.SourceEvent) (*admittedEvent, error) {
	if event.End != nil {
		return &admittedEvent{event: event}, nil
	}
	delivery := event.Delivery
	bytes := eventBytes(delivery)
	sandboxID := delivery.Record.Resource.SandboxID
	if bytes > p.cfg.MemoryBudgetBytes || bytes > p.cfg.PerSandboxQueueBytes {
		if p.cfg.DropPolicy == "drop" {
			return nil, p.drop(ctx, worker, delivery, "pipeline-record-too-large")
		}
		return nil, fmt.Errorf("record requires %d bytes and cannot fit configured queue budgets", bytes)
	}
	if p.cfg.PerSandboxRateLimit > 0 {
		limiter := p.limiter(sandboxID)
		if p.cfg.DropPolicy == "drop" {
			if !limiter.Allow() {
				return nil, p.drop(ctx, worker, delivery, "pipeline-rate-limit")
			}
		} else if err := limiter.Wait(ctx); err != nil {
			return nil, err
		}
	}
	for {
		p.budgetMu.Lock()
		if p.globalBytes+bytes <= p.cfg.MemoryBudgetBytes && p.sandboxBytes[sandboxID]+bytes <= p.cfg.PerSandboxQueueBytes {
			p.globalBytes += bytes
			p.sandboxBytes[sandboxID] += bytes
			p.budgetMu.Unlock()
			p.metrics.records.Add(context.Background(), 1)
			p.metrics.bytes.Add(context.Background(), bytes)
			p.metrics.queueBytes.Add(context.Background(), bytes)
			return &admittedEvent{event: event, bytes: bytes, sandboxID: sandboxID}, nil
		}
		p.budgetMu.Unlock()
		if p.cfg.DropPolicy == "drop" {
			return nil, p.drop(ctx, worker, delivery, "pipeline-queue-limit")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-p.budgetChanged:
		}
	}
}

func (p *Pipeline) drop(ctx context.Context, worker *worker, delivery *api.Delivery, reason string) error {
	result := api.AckResult{Token: delivery.AckToken, Disposition: api.AckIntentionalDrop, Reason: reason, Guarantee: p.sink.Guarantee()}
	if err := p.acknowledgeWithRetry(ctx, []api.AckResult{result}, nil); err != nil {
		return err
	}
	worker.outcomeMu.Lock()
	worker.dropOutcome.HadDrops = true
	worker.dropOutcome.LossReasons = addReason(worker.dropOutcome.LossReasons, reason)
	worker.outcomeMu.Unlock()
	p.metrics.drops.Add(context.Background(), 1)
	return nil
}

func (p *Pipeline) limiter(sandboxID string) *rate.Limiter {
	p.budgetMu.Lock()
	defer p.budgetMu.Unlock()
	limiter := p.limiters[sandboxID]
	if limiter == nil {
		burst := max(1, int(p.cfg.PerSandboxRateLimit))
		limiter = rate.NewLimiter(rate.Limit(p.cfg.PerSandboxRateLimit), burst)
		p.limiters[sandboxID] = limiter
	}
	return limiter
}

func (p *Pipeline) release(bytes int64, sandboxID string) {
	if bytes == 0 {
		return
	}
	p.budgetMu.Lock()
	p.globalBytes -= bytes
	p.sandboxBytes[sandboxID] -= bytes
	if p.sandboxBytes[sandboxID] == 0 {
		delete(p.sandboxBytes, sandboxID)
	}
	p.budgetMu.Unlock()
	p.metrics.queueBytes.Add(context.Background(), -bytes)
	select {
	case p.budgetChanged <- struct{}{}:
	default:
	}
}

func (w *worker) mergeDropOutcome(outcome api.SourceOutcome) api.SourceOutcome {
	w.outcomeMu.Lock()
	drops := w.dropOutcome
	w.outcomeMu.Unlock()
	if drops.HadDrops {
		outcome.HadDrops = true
		for _, reason := range drops.LossReasons {
			outcome.LossReasons = addReason(outcome.LossReasons, reason)
		}
	}
	return outcome
}

func eventBytes(delivery *api.Delivery) int64 {
	size := int64(512 + len(delivery.Record.Body) + len(delivery.RecordID) + len(delivery.AckToken.Value))
	for key, value := range delivery.Record.Attributes {
		size += int64(len(key) + len(value))
	}
	return size
}

func addReason(reasons []string, reason string) []string {
	for _, existing := range reasons {
		if existing == reason {
			return reasons
		}
	}
	return append(reasons, reason)
}

func newPipelineMetrics() (pipelineMetrics, error) {
	meter := otel.Meter("github.com/alibaba/opensandbox/nodeagent/pipeline")
	records, err1 := meter.Int64Counter("opensandbox.nodeagent.records")
	bytes, err2 := meter.Int64Counter("opensandbox.nodeagent.bytes")
	drops, err3 := meter.Int64Counter("opensandbox.nodeagent.drops")
	retries, err4 := meter.Int64Counter("opensandbox.nodeagent.retries")
	queueBytes, err5 := meter.Int64UpDownCounter("opensandbox.nodeagent.queue.bytes")
	consumeMillis, err6 := meter.Float64Histogram("opensandbox.nodeagent.sink.consume.duration", metric.WithUnit("ms"))
	if err := errors.Join(err1, err2, err3, err4, err5, err6); err != nil {
		return pipelineMetrics{}, err
	}
	return pipelineMetrics{records: records, bytes: bytes, drops: drops, retries: retries, queueBytes: queueBytes, consumeMillis: consumeMillis}, nil
}

func eventStream(event api.SourceEvent) api.StreamRef {
	if event.Delivery != nil {
		return event.Delivery.StreamRef
	}
	return event.End.StreamRef
}

func compatible(source, sink api.Capabilities) bool {
	for _, sourceKind := range source.RecordKinds {
		for _, sinkKind := range sink.RecordKinds {
			if sourceKind == sinkKind {
				return true
			}
		}
	}
	return false
}
