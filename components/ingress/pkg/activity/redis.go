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
	"fmt"
	"strings"
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

	// Worker batching: one pipeline round trip per batch; the flush window
	// adds at most 50ms of write delay, noise against X >= 30s.
	recordPipeBatchSize = 64
	recordPipeFlush     = 50 * time.Millisecond

	// warnDropIntervalSeconds rate-limits the escalated drop warnings.
	warnDropIntervalSeconds = 30

	// KeyPrefix is a fixed convention shared with the server's idle sweeper,
	// not a configurable.
	KeyPrefix = "opensandbox:activity"

	// monotonicMaxScript advances the stored timestamp only forward, so
	// out-of-order writes across replicas never regress the newest
	// observation.
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
	// threshold X must never exceed it. Applied as whole seconds.
	TTL time.Duration
	// MinInterval coalesces writes: at most one write per (replica, sandbox)
	// per interval, independent of request rate.
	MinInterval time.Duration
	// NowMillis overrides the observation clock (tests); nil uses the wall
	// clock.
	NowMillis func() int64

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

	// scriptSHA caches the loaded Lua script; batches send EVALSHA and a
	// NOSCRIPT triggers one reload + retry.
	scriptSHA atomic.Value // string
	scriptMu  sync.Mutex   // serializes ScriptLoad only
	// lastWarnUnix rate-limits the escalated drop warnings.
	lastWarnUnix atomic.Int64
}

func NewRedisRecorder(ctx context.Context, client *redis.Client, cfg RedisConfig) (*RedisRecorder, error) {
	if client == nil {
		return nil, errors.New("activity: Redis client is required")
	}
	if cfg.Logger == nil {
		return nil, errors.New("activity: Logger is required")
	}
	// Whole seconds: a sub-second TTL truncates to EX 0 and Redis rejects
	// every write.
	if cfg.TTL < time.Second {
		return nil, errors.New("activity: TTL must be at least one second, or every write is rejected by Redis and the sweeper never sees activity")
	}
	if cfg.MinInterval < 0 {
		return nil, errors.New("activity: MinInterval cannot be negative")
	}
	nowMillis := cfg.NowMillis
	if nowMillis == nil {
		nowMillis = func() int64 { return time.Now().UnixMilli() }
	}
	r := &RedisRecorder{client: client, cfg: cfg, ch: make(chan observation, recordChanCap), nowMillis: nowMillis}
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
	// Surface an unreachable Redis now; the recorder still starts — activity
	// failures must never block the ingress.
	if err := client.Ping(ctx).Err(); err != nil {
		cfg.Logger.With(logger.Field{Key: "error", Value: err}).Warnf(
			"activity: Redis is not reachable at startup; activity observations will be dropped until it recovers")
	}
	return r, nil
}

// runWorker drains observations into batches and flushes each batch as one
// pipelined round trip. On shutdown it counts every observation still queued
// so the dropped-writes metric matches the documented contract.
func (r *RedisRecorder) runWorker(ctx context.Context) {
	for {
		batch, ok := r.drainBatch(ctx)
		if !ok {
			break
		}
		if len(batch) > 0 {
			r.doRecordBatch(batch)
		}
	}
	for {
		select {
		case <-r.ch:
			telemetry.RecordActivityWriteDropped(1)
		default:
			return
		}
	}
}

// drainBatch collects up to recordPipeBatchSize observations, returning
// early on the flush window or shutdown. The bool reports whether the worker
// should keep running.
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

// Record enqueues one observation, fire-and-forget: a full channel or
// shutdown drops it, which can only delay a pause, never accelerate one.
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

// shouldRecord applies the per-sandbox min-interval coalescing; drops only
// make the recorded last-active slightly stale. The compare-and-swap loop
// keeps the bound exact under concurrency — a plain check-then-act would
// let every request pass once the window reopens.
func (r *RedisRecorder) shouldRecord(obs observation) bool {
	if r.cfg.MinInterval <= 0 {
		return true
	}
	key := struct{ namespace, sandboxID string }{namespace: obs.namespace, sandboxID: obs.sandboxID}
	now := time.UnixMilli(obs.atMillis)
	for {
		prev, loaded := r.lastSent.LoadOrStore(key, now)
		if !loaded {
			return true
		}
		if now.Sub(prev.(time.Time)) < r.cfg.MinInterval {
			return false
		}
		if r.lastSent.CompareAndSwap(key, prev, now) {
			return true
		}
	}
}

// doRecordBatch flushes one batch as a single pipelined round trip.
func (r *RedisRecorder) doRecordBatch(batch []observation) {
	ctx, cancel := context.WithTimeout(context.Background(), redisOpTimeout)
	defer cancel()
	for attempt := 0; ; attempt++ {
		sha, err := r.loadedScriptSHA(ctx)
		if err != nil {
			r.dropBatch(batch, err)
			return
		}
		pipe := r.client.Pipeline()
		for _, obs := range batch {
			pipe.EvalSha(
				ctx,
				sha,
				[]string{KeyPrefix + ":" + obs.sandboxID},
				obs.atMillis,
				int64(r.cfg.TTL/time.Second),
			)
		}
		cmds, err := pipe.Exec(ctx)
		if attempt == 0 && isNoScriptErr(cmds, err) {
			// Redis lost the script (restart, SCRIPT FLUSH): reload once and
			// replay the whole batch.
			r.scriptMu.Lock()
			r.scriptSHA.Store("")
			r.scriptMu.Unlock()
			continue
		}
		r.reportBatchOutcome(batch, cmds, err)
		return
	}
}

// loadedScriptSHA returns the cached script SHA, loading it on first use or
// after invalidation. A non-nil error means the batch must be dropped.
func (r *RedisRecorder) loadedScriptSHA(ctx context.Context) (string, error) {
	if sha, ok := r.scriptSHA.Load().(string); ok && sha != "" {
		return sha, nil
	}
	// Never serialize the workers behind a load: a concurrent loader wins
	// and the losers wait briefly for its cached SHA before giving up.
	if !r.scriptMu.TryLock() {
		for range 50 {
			time.Sleep(2 * time.Millisecond)
			if sha, ok := r.scriptSHA.Load().(string); ok && sha != "" {
				return sha, nil
			}
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
		}
		return "", errors.New("activity: concurrent script load did not complete promptly")
	}
	defer r.scriptMu.Unlock()
	if sha, ok := r.scriptSHA.Load().(string); ok && sha != "" {
		return sha, nil
	}
	sha, err := r.client.ScriptLoad(ctx, monotonicMaxScript).Result()
	if err != nil {
		return "", fmt.Errorf("load monotonic-max script: %w", err)
	}
	r.scriptSHA.Store(sha)
	return sha, nil
}

func isNoScriptErr(cmds []redis.Cmder, err error) bool {
	if err != nil && strings.Contains(err.Error(), "NOSCRIPT") {
		return true
	}
	for _, cmd := range cmds {
		if cmd.Err() != nil && strings.Contains(cmd.Err().Error(), "NOSCRIPT") {
			return true
		}
	}
	return false
}

// dropBatch records a fully dropped batch; the first drop of a warn window
// is escalated (sustained drops mean the sweeper is flying blind).
func (r *RedisRecorder) dropBatch(batch []observation, err error) {
	telemetry.RecordActivityWriteDropped(int64(len(batch)))
	fields := []logger.Field{
		{Key: "batch", Value: len(batch)},
		{Key: "error", Value: err},
	}
	now := time.Now().Unix()
	if last := r.lastWarnUnix.Load(); now-last >= warnDropIntervalSeconds && r.lastWarnUnix.CompareAndSwap(last, now) {
		r.cfg.Logger.With(fields...).Warnf("activity: dropping activity writes — the idle sweeper is flying blind and may pause sandboxes in active use")
		return
	}
	r.cfg.Logger.With(fields...).Debugf("activity: redis batch dropped")
}

func (r *RedisRecorder) reportBatchOutcome(batch []observation, cmds []redis.Cmder, err error) {
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
		r.dropBatch(batch[:dropped], err)
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
