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

// Package activity records per-sandbox last-traffic observations in Redis as
// a side effect of proxying (OSEP-0024 auto-pause). Writes are asynchronous,
// coalesced per sandbox, and fire-and-forget: they never add synchronous
// latency or failure modes to the proxy path.
package activity

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alibaba/opensandbox/internal/logger"
	"github.com/redis/go-redis/v9"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/alibaba/opensandbox/ingress/pkg/telemetry"
)

const (
	redisOpTimeout = 5 * time.Second
	recordWorkers  = 4
	recordChanCap  = 8192

	// Worker batching: observations are drained into a pipeline so one Redis
	// round trip carries many writes. The batch bounds and the flush window
	// trade throughput against a tiny extra write delay (irrelevant for idle
	// measurement at X >= 30s).
	recordPipeBatchSize = 64
	recordPipeFlush     = 50 * time.Millisecond

	// KeyPrefix namespaces the per-sandbox activity keys. It is a fixed
	// convention shared with the server's idle sweeper, not a configurable.
	KeyPrefix = "opensandbox:activity"

	// monotonicMaxScript advances the stored timestamp only forward.
	// Buffered writes from different replicas can arrive out of order; an
	// older observation must never regress the newest one.
	monotonicMaxScript = `local cur = redis.call('GET', KEYS[1])
if cur and tonumber(cur) >= tonumber(ARGV[1]) then return 0 end
redis.call('SET', KEYS[1], ARGV[1], 'EX', ARGV[2])
return 1`
)

// Recorder records a last-activity observation for one sandbox. OSEP-0009's
// "access renew skip" sentinel does not suppress activity: skipping a renewal
// is not skipping activity.
type Recorder interface {
	Record(namespace, sandboxID string)
}

// Noop is the disabled recorder.
type Noop struct{}

func (Noop) Record(string, string) {}

type RedisConfig struct {
	// TTL bounds how long an observation stays fresh; the server's idle
	// threshold X must never exceed it.
	TTL time.Duration
	// MinInterval coalesces writes: at most one Redis write per (replica,
	// sandbox) per interval, so the write rate is independent of the
	// per-sandbox request rate.
	MinInterval time.Duration

	Logger logger.Logger
}

type observation struct {
	namespace string
	sandboxID string
	atMillis  int64
}

// RedisRecorder is the Redis-backed activity writer.
type RedisRecorder struct {
	client    *redis.Client
	cfg       RedisConfig
	lastSent  sync.Map
	ch        chan observation
	stopped   atomic.Bool
	nowMillis func() int64
}

func NewRedisRecorder(ctx context.Context, client *redis.Client, cfg RedisConfig) *RedisRecorder {
	r := &RedisRecorder{client: client, cfg: cfg, ch: make(chan observation, recordChanCap), nowMillis: func() int64 { return time.Now().UnixMilli() }}
	for range recordWorkers {
		go r.runWorker(ctx)
	}
	go func() {
		<-ctx.Done()
		r.stopped.Store(true)
	}()
	if cfg.MinInterval > 0 {
		go wait.UntilWithContext(ctx, r.runCleanupThrottle, cfg.MinInterval*2)
	}
	return r
}

// runWorker drains observations into batches and flushes each batch as one
// pipelined round trip.
func (r *RedisRecorder) runWorker(ctx context.Context) {
	for {
		batch, ok := r.drainBatch(ctx)
		if !ok {
			return
		}
		if len(batch) > 0 {
			r.doRecordBatch(batch)
		}
	}
}

// drainBatch collects up to recordPipeBatchSize observations, returning
// early on the flush window or shutdown. The bool reports whether the worker
// should keep running; idle windows yield an empty batch and continue.
func (r *RedisRecorder) drainBatch(ctx context.Context) ([]observation, bool) {
	batch := make([]observation, 0, recordPipeBatchSize)
	timer := time.NewTimer(recordPipeFlush)
	defer timer.Stop()
	for {
		select {
		case obs := <-r.ch:
			batch = append(batch, obs)
			if len(batch) >= recordPipeBatchSize {
				return batch, true
			}
		case <-timer.C:
			return batch, true
		case <-ctx.Done():
			if len(batch) == 0 {
				return nil, false
			}
			return batch, true
		}
	}
}

// Record enqueues one observation. It is fire-and-forget: full channel and
// shutdown both drop the observation, which can only delay a pause, never
// accelerate one.
func (r *RedisRecorder) Record(namespace, sandboxID string) {
	if r.stopped.Load() {
		return
	}
	obs := observation{namespace: namespace, sandboxID: sandboxID, atMillis: r.nowMillis()}
	if !r.shouldRecord(obs) {
		return
	}
	select {
	case r.ch <- obs:
	default:
		telemetry.RecordActivityWriteDropped(1)
	}
}

// shouldRecord applies the per-sandbox min-interval coalescing. Dropped
// observations only make the recorded last-active slightly stale, which can
// delay a pause by at most one interval and never accelerate one; the Lua
// monotonic max keeps any racing order harmless.
func (r *RedisRecorder) shouldRecord(obs observation) bool {
	if r.cfg.MinInterval <= 0 {
		return true
	}
	key := struct{ namespace, sandboxID string }{namespace: obs.namespace, sandboxID: obs.sandboxID}
	now := time.UnixMilli(obs.atMillis)
	prev, loaded := r.lastSent.LoadOrStore(key, now)
	if !loaded {
		return true
	}
	if now.Sub(prev.(time.Time)) < r.cfg.MinInterval {
		return false
	}
	r.lastSent.Store(key, now)
	return true
}

// doRecordBatch flushes one batch as a single pipelined round trip. Every
// command applies the monotonic max update: the stored timestamp moves
// forward only, so out-of-order deliveries across replicas are harmless.
func (r *RedisRecorder) doRecordBatch(batch []observation) {
	ctx, cancel := context.WithTimeout(context.Background(), redisOpTimeout)
	defer cancel()
	pipe := r.client.Pipeline()
	for _, obs := range batch {
		pipe.Eval(
			ctx,
			monotonicMaxScript,
			[]string{KeyPrefix + ":" + obs.sandboxID},
			obs.atMillis,
			int64(r.cfg.TTL/time.Second),
		)
	}
	cmds, err := pipe.Exec(ctx)
	dropped := 0
	if err != nil {
		dropped = len(batch)
	} else {
		for _, cmd := range cmds {
			if cmd.Err() != nil {
				dropped++
			}
		}
	}
	if dropped > 0 {
		telemetry.RecordActivityWriteDropped(int64(dropped))
		r.cfg.Logger.With(
			logger.Field{Key: "dropped", Value: dropped},
			logger.Field{Key: "batch", Value: len(batch)},
			logger.Field{Key: "error", Value: err},
		).Debugf("activity: redis pipeline partially dropped")
	}
}

// RedisClientFromDSN builds a Redis client from a redis:// DSN.
func RedisClientFromDSN(dsn string) (*redis.Client, error) {
	opts, err := redis.ParseURL(dsn)
	if err != nil {
		return nil, err
	}
	if opts == nil {
		return nil, errors.New("activity: redis DSN produced nil options")
	}
	return redis.NewClient(opts), nil
}

func (r *RedisRecorder) runCleanupThrottle(_ context.Context) {
	// The cutoff uses the same clock as the observations so a custom clock
	// (tests) stays consistent with the throttle state.
	cutoff := time.UnixMilli(r.nowMillis()).Add(-r.cfg.MinInterval * 2)
	r.lastSent.Range(func(key, value any) bool {
		if value.(time.Time).Before(cutoff) {
			r.lastSent.Delete(key)
		}
		return true
	})
}
