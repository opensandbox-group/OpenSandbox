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

package events

import (
	"context"
	"sync"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/log"
	"github.com/alibaba/opensandbox/internal/safego"
)

const defaultQueueSize = 128

// BlockedEvent is emitted when the DNS path denies a query.
type BlockedEvent struct {
	Hostname  string    `json:"hostname"`
	Timestamp time.Time `json:"timestamp"`
}

type Subscriber interface {
	HandleBlocked(ctx context.Context, ev BlockedEvent)
}

type BroadcasterConfig struct {
	QueueSize int
}

// Broadcaster: per-subscriber buffered channel; full buffer drops and logs a warning.
type Broadcaster struct {
	ctx    context.Context
	cancel context.CancelFunc

	mu          sync.RWMutex
	subscribers []chan BlockedEvent
	queueSize   int
	closed      bool
	workers     sync.WaitGroup
}

func NewBroadcaster(ctx context.Context, cfg BroadcasterConfig) *Broadcaster {
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = defaultQueueSize
	}
	cctx, cancel := context.WithCancel(ctx)
	return &Broadcaster{
		ctx:       cctx,
		cancel:    cancel,
		queueSize: cfg.QueueSize,
	}
}

func (b *Broadcaster) AddSubscriber(sub Subscriber) {
	if sub == nil {
		return
	}
	ch := make(chan BlockedEvent, b.queueSize)

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.workers.Add(1)
	b.subscribers = append(b.subscribers, ch)
	b.mu.Unlock()

	safego.Go(func() {
		defer b.workers.Done()
		for {
			select {
			case <-b.ctx.Done():
				return
			case ev, ok := <-ch:
				if !ok {
					return
				}
				sub.HandleBlocked(b.ctx, ev)
			}
		}
	})
}

func (b *Broadcaster) Publish(event BlockedEvent) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return
	}

	for _, ch := range b.subscribers {
		select {
		case ch <- event:
		default:
			log.Warnf("[events] blocked-event queue full; dropping hostname %s", event.Hostname)
		}
	}
}

// Shutdown seals admission and waits for queued deliveries within ctx's budget.
func (b *Broadcaster) Shutdown(ctx context.Context) error {
	b.seal()
	defer b.cancel()
	done := make(chan struct{})
	safego.Go(func() {
		b.workers.Wait()
		close(done)
	})
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *Broadcaster) Close() {
	b.cancel()
	b.seal()
}

func (b *Broadcaster) seal() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for _, ch := range b.subscribers {
		close(ch)
	}
	b.subscribers = nil
}
