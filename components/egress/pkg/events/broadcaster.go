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
	"sync/atomic"
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
	closed      atomic.Bool
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
	b.subscribers = append(b.subscribers, ch)
	b.mu.Unlock()

	safego.Go(func() {
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
	if b.closed.Load() {
		return
	}

	b.mu.RLock()
	defer b.mu.RUnlock()

	for _, ch := range b.subscribers {
		select {
		case ch <- event:
		default:
			log.Warnf("[events] blocked-event queue full; dropping hostname %s", event.Hostname)
		}
	}
}

func (b *Broadcaster) Close() {
	if b.closed.Load() {
		return
	}

	b.cancel()

	b.mu.Lock()
	defer b.mu.Unlock()
	subs := b.subscribers
	b.subscribers = nil

	for _, ch := range subs {
		close(ch)
	}
	b.closed.Store(true)
}
