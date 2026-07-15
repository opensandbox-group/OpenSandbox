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

package renewintent

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alibaba/opensandbox/internal/logger"
	"github.com/redis/go-redis/v9"
	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	redisOpTimeout = 5 * time.Second
	publishWorkers = 4
	publishChanCap = 8192
)

type RedisPublisherConfig struct {
	QueueKey    string
	QueueMaxLen int
	MinInterval time.Duration
	Logger      logger.Logger
}

type intentReq struct {
	sandboxID  string
	port       int
	requestURI string
}

type RedisPublisher struct {
	client   *redis.Client
	cfg      RedisPublisherConfig
	lastSent sync.Map
	ch       chan intentReq
	stopped  atomic.Bool
}

func NewRedisPublisher(ctx context.Context, client *redis.Client, cfg RedisPublisherConfig) *RedisPublisher {
	p := &RedisPublisher{client: client, cfg: cfg, ch: make(chan intentReq, publishChanCap)}
	for i := 0; i < publishWorkers; i++ {
		go func() {
			for {
				select {
				case req := <-p.ch:
					p.doPublish(req.sandboxID, req.port, req.requestURI)
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	go func() {
		<-ctx.Done()
		p.stopped.Store(true)
	}()

	if cfg.MinInterval > 0 {
		go wait.UntilWithContext(ctx, p.runCleanupThrottle, cfg.MinInterval*2)
	}
	return p
}

func (p *RedisPublisher) shouldSendIntent(sandboxID string) bool {
	if p.cfg.MinInterval <= 0 {
		return true
	}

	now := time.Now()
	prev, loaded := p.lastSent.LoadOrStore(sandboxID, now)
	if !loaded {
		return true
	}
	if now.Sub(prev.(time.Time)) < p.cfg.MinInterval {
		return false
	}
	p.lastSent.Store(sandboxID, now)
	return true
}

func (p *RedisPublisher) PublishIntent(sandboxID string, port int, requestURI string) {
	if p.stopped.Load() {
		return
	}
	select {
	case p.ch <- intentReq{sandboxID: sandboxID, port: port, requestURI: requestURI}:
	default:
	}
}

func (p *RedisPublisher) doPublish(sandboxID string, port int, requestURI string) {
	if !p.shouldSendIntent(sandboxID) {
		return
	}

	intent := NewIntent(sandboxID, port, requestURI)
	payload, err := json.Marshal(intent)
	if err != nil {
		p.cfg.Logger.With(logger.Field{Key: "sandbox_id", Value: sandboxID}).Errorf("renewintent: marshal intent: %v", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), redisOpTimeout)
	defer cancel()
	pipe := p.client.Pipeline()
	pipe.LPush(ctx, p.cfg.QueueKey, string(payload))
	if p.cfg.QueueMaxLen > 0 {
		pipe.LTrim(ctx, p.cfg.QueueKey, 0, int64(p.cfg.QueueMaxLen-1))
	}
	_, err = pipe.Exec(ctx)
	if err != nil {
		p.cfg.Logger.With(
			logger.Field{Key: "sandbox_id", Value: sandboxID},
			logger.Field{Key: "queue_key", Value: p.cfg.QueueKey},
			logger.Field{Key: "error", Value: err},
		).Errorf("renewintent: redis publish failed")
		return
	}
	p.cfg.Logger.With(
		logger.Field{Key: "sandbox_id", Value: sandboxID},
		logger.Field{Key: "queue_key", Value: p.cfg.QueueKey},
	).Debugf("renewintent: published")
}

func RedisClientFromDSN(dsn string) (*redis.Client, error) {
	opts, err := redis.ParseURL(dsn)
	if err != nil {
		return nil, err
	}
	if opts == nil {
		return nil, errors.New("renewintent: redis DSN produced nil options")
	}
	return redis.NewClient(opts), nil
}

func (p *RedisPublisher) runCleanupThrottle(_ context.Context) {
	cutoff := time.Now().Add(-p.cfg.MinInterval * 2)
	p.lastSent.Range(func(key, value any) bool {
		if value.(time.Time).Before(cutoff) {
			p.lastSent.Delete(key)
		}
		return true
	})
}
