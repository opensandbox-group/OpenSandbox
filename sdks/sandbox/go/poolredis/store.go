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

package poolredis

import (
	"context"
	"encoding/base64"
	"fmt"
	"strconv"
	"time"

	opensandbox "github.com/alibaba/OpenSandbox/sdks/sandbox/go"
	"github.com/redis/go-redis/v9"
)

// Lua scripts for atomic Redis operations.
var (
	takeIdleScript = redis.NewScript(`
local redis_time = redis.call('TIME')
local now_ms = tonumber(redis_time[1]) * 1000 + math.floor(tonumber(redis_time[2]) / 1000)
local min_remaining_ttl_ms = tonumber(ARGV[1]) or 0
local cutoff_ms = now_ms + min_remaining_ttl_ms
local discarded_alive = {}
while true do
  local sandbox_id = redis.call('LPOP', KEYS[1])
  if not sandbox_id then
    if #discarded_alive == 0 then
      return nil
    end
    return {'', discarded_alive}
  end
  local expires_at = redis.call('HGET', KEYS[2], sandbox_id)
  if expires_at then
    redis.call('HDEL', KEYS[2], sandbox_id)
    local exp = tonumber(expires_at)
    if exp > cutoff_ms then
      return {sandbox_id, discarded_alive}
    end
    if exp > now_ms then
      table.insert(discarded_alive, sandbox_id)
    end
  end
end
`)

	putIdleScript = redis.NewScript(`
local redis_time = redis.call('TIME')
local now_ms = tonumber(redis_time[1]) * 1000 + math.floor(tonumber(redis_time[2]) / 1000)
local expires_at = now_ms + tonumber(ARGV[2])
local current_expires_at = redis.call('HGET', KEYS[2], ARGV[1])
if current_expires_at and tonumber(current_expires_at) > now_ms then
  return 0
end
if current_expires_at then
  -- Re-activate expired entry: remove from old position and re-add at tail for FIFO
  redis.call('LREM', KEYS[1], 0, ARGV[1])
end
redis.call('RPUSH', KEYS[1], ARGV[1])
redis.call('HSET', KEYS[2], ARGV[1], expires_at)
return 1
`)

	reapExpiredScript = redis.NewScript(`
local redis_time = redis.call('TIME')
local now_ms = tonumber(redis_time[1]) * 1000 + math.floor(tonumber(redis_time[2]) / 1000)
local min_remaining_ttl_ms = tonumber(ARGV[1]) or 0
local cutoff_ms = now_ms + min_remaining_ttl_ms
local discarded_alive = {}
local entries = redis.call('HGETALL', KEYS[2])
for i = 1, #entries, 2 do
  local sandbox_id = entries[i]
  local exp = tonumber(entries[i + 1])
  if exp <= cutoff_ms then
    redis.call('HDEL', KEYS[2], sandbox_id)
    redis.call('LREM', KEYS[1], 0, sandbox_id)
    if exp > now_ms then
      table.insert(discarded_alive, sandbox_id)
    end
  end
end
return discarded_alive
`)

	acquireLockScript = redis.NewScript(`
local current = redis.call('GET', KEYS[1])
if not current then
  redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[2])
  return 1
elseif current == ARGV[1] then
  redis.call('PEXPIRE', KEYS[1], ARGV[2])
  return 1
end
return 0
`)

	renewLockScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
  redis.call('PEXPIRE', KEYS[1], ARGV[2])
  return 1
end
return 0
`)

	releaseLockScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('DEL', KEYS[1])
end
return 0
`)

	removeIdleScript = redis.NewScript(`
redis.call('HDEL', KEYS[2], ARGV[1])
redis.call('LREM', KEYS[1], 0, ARGV[1])
return 1
`)

	snapshotCountersScript = redis.NewScript(`
local redis_time = redis.call('TIME')
local now_ms = tonumber(redis_time[1]) * 1000 + math.floor(tonumber(redis_time[2]) / 1000)
local entries = redis.call('HGETALL', KEYS[1])
local count = 0
for i = 1, #entries, 2 do
  local exp = tonumber(entries[i + 1])
  if exp > now_ms then
    count = count + 1
  end
end
return count
`)
)

// DefaultRedisKeyPrefix is the default key prefix for pool state in Redis.
const DefaultRedisKeyPrefix = "opensandbox:pool"

// RedisPoolStateStoreConfig configures the Redis-backed pool state store.
type RedisPoolStateStoreConfig struct {
	Client    redis.UniversalClient
	KeyPrefix string // default: "opensandbox:pool"
}

// RedisPoolStateStore is a Redis-backed implementation of PoolStateStore.
// It uses Lua scripts for atomic compound operations and is safe for use
// across multiple processes coordinating the same pool.
type RedisPoolStateStore struct {
	client    redis.UniversalClient
	keyPrefix string
}

// NewRedisPoolStateStore creates a new RedisPoolStateStore with the given configuration.
// Returns an error if config.Client is nil.
func NewRedisPoolStateStore(config RedisPoolStateStoreConfig) (*RedisPoolStateStore, error) {
	if config.Client == nil {
		return nil, fmt.Errorf("opensandbox: RedisPoolStateStoreConfig.Client must not be nil")
	}
	prefix := config.KeyPrefix
	if prefix == "" {
		prefix = DefaultRedisKeyPrefix
	}
	return &RedisPoolStateStore{
		client:    config.Client,
		keyPrefix: prefix,
	}, nil
}

// TryTakeIdle atomically takes the oldest idle sandbox from the pool.
// Returns empty string if no idle sandbox is available.
func (s *RedisPoolStateStore) TryTakeIdle(ctx context.Context, poolName string) (string, error) {
	result, err := s.runTakeIdle(ctx, poolName, 0)
	if err != nil {
		return "", err
	}
	return result.SandboxID, nil
}

// TryTakeIdleWithMinTTL atomically takes the oldest idle sandbox that has
// at least minRemaining TTL left.
func (s *RedisPoolStateStore) TryTakeIdleWithMinTTL(ctx context.Context, poolName string, minRemaining time.Duration) (*opensandbox.TakeIdleResult, error) {
	if minRemaining <= 0 {
		id, err := s.TryTakeIdle(ctx, poolName)
		if err != nil {
			return nil, err
		}
		return &opensandbox.TakeIdleResult{SandboxID: id}, nil
	}
	return s.runTakeIdle(ctx, poolName, minRemaining.Milliseconds())
}

func (s *RedisPoolStateStore) runTakeIdle(ctx context.Context, poolName string, minTTLMs int64) (*opensandbox.TakeIdleResult, error) {
	keys := []string{s.idleListKey(poolName), s.idleExpiresKey(poolName)}
	argv := []interface{}{strconv.FormatInt(minTTLMs, 10)}

	raw, err := takeIdleScript.Run(ctx, s.client, keys, argv...).Result()
	if err == redis.Nil {
		return &opensandbox.TakeIdleResult{}, nil
	}
	if err != nil {
		return nil, &opensandbox.PoolStateStoreUnavailableError{Operation: "TryTakeIdle", Cause: err}
	}

	return s.decodeTakeIdleResult(raw), nil
}

func (s *RedisPoolStateStore) decodeTakeIdleResult(raw interface{}) *opensandbox.TakeIdleResult {
	if raw == nil {
		return &opensandbox.TakeIdleResult{}
	}

	list, ok := raw.([]interface{})
	if !ok {
		return &opensandbox.TakeIdleResult{}
	}
	if len(list) == 0 {
		return &opensandbox.TakeIdleResult{}
	}

	result := &opensandbox.TakeIdleResult{}

	// First element: sandbox ID (empty string means none taken but discards exist)
	if takenRaw, ok := list[0].(string); ok && takenRaw != "" {
		result.SandboxID = takenRaw
	}

	// Second element: discarded alive list
	if len(list) > 1 {
		if discardedRaw, ok := list[1].([]interface{}); ok {
			for _, d := range discardedRaw {
				if id, ok := d.(string); ok {
					result.DiscardedAliveSandboxIDs = append(result.DiscardedAliveSandboxIDs, id)
				}
			}
		}
	}

	return result
}

// PutIdle adds a sandbox to the idle pool. Idempotent: if already present and not expired, no-op.
func (s *RedisPoolStateStore) PutIdle(ctx context.Context, poolName string, sandboxID string) error {
	if sandboxID == "" {
		return fmt.Errorf("opensandbox: sandboxID must not be blank")
	}

	idleTTLMs, err := s.resolveIdleTTL(ctx, poolName)
	if err != nil {
		return err
	}

	keys := []string{s.idleListKey(poolName), s.idleExpiresKey(poolName)}
	argv := []interface{}{sandboxID, strconv.FormatInt(idleTTLMs, 10)}

	_, err = putIdleScript.Run(ctx, s.client, keys, argv...).Result()
	if err != nil && err != redis.Nil {
		return &opensandbox.PoolStateStoreUnavailableError{Operation: "PutIdle", Cause: err}
	}
	return nil
}

// RemoveIdle atomically removes a sandbox from the idle pool. Idempotent.
func (s *RedisPoolStateStore) RemoveIdle(ctx context.Context, poolName string, sandboxID string) error {
	if sandboxID == "" {
		return fmt.Errorf("opensandbox: sandboxID must not be blank")
	}
	keys := []string{s.idleListKey(poolName), s.idleExpiresKey(poolName)}
	argv := []interface{}{sandboxID}

	_, err := removeIdleScript.Run(ctx, s.client, keys, argv...).Result()
	if err != nil && err != redis.Nil {
		return &opensandbox.PoolStateStoreUnavailableError{Operation: "RemoveIdle", Cause: err}
	}
	return nil
}

// TryAcquirePrimaryLock atomically acquires or re-entrantly renews the primary lock.
func (s *RedisPoolStateStore) TryAcquirePrimaryLock(ctx context.Context, poolName string, ownerID string, ttl time.Duration) (bool, error) {
	ttlMs := ttl.Milliseconds()
	if ttlMs < 1 {
		ttlMs = 1
	}
	result, err := acquireLockScript.Run(ctx, s.client,
		[]string{s.PrimaryLockKey(poolName)},
		ownerID, strconv.FormatInt(ttlMs, 10)).Int64()
	if err != nil && err != redis.Nil {
		return false, &opensandbox.PoolStateStoreUnavailableError{Operation: "TryAcquirePrimaryLock", Cause: err}
	}
	return result == 1, nil
}

// RenewPrimaryLock extends the primary lock TTL. Only succeeds if caller is the current owner.
func (s *RedisPoolStateStore) RenewPrimaryLock(ctx context.Context, poolName string, ownerID string, ttl time.Duration) (bool, error) {
	ttlMs := ttl.Milliseconds()
	if ttlMs < 1 {
		ttlMs = 1
	}

	keys := []string{s.PrimaryLockKey(poolName)}
	argv := []interface{}{ownerID, strconv.FormatInt(ttlMs, 10)}

	result, err := renewLockScript.Run(ctx, s.client, keys, argv...).Int64()
	if err != nil && err != redis.Nil {
		return false, &opensandbox.PoolStateStoreUnavailableError{Operation: "RenewPrimaryLock", Cause: err}
	}
	return result == 1, nil
}

// ReleasePrimaryLock releases the primary lock. Only succeeds if caller is the current owner.
func (s *RedisPoolStateStore) ReleasePrimaryLock(ctx context.Context, poolName string, ownerID string) error {
	keys := []string{s.PrimaryLockKey(poolName)}
	argv := []interface{}{ownerID}

	_, err := releaseLockScript.Run(ctx, s.client, keys, argv...).Result()
	if err != nil && err != redis.Nil {
		return &opensandbox.PoolStateStoreUnavailableError{Operation: "ReleasePrimaryLock", Cause: err}
	}
	return nil
}

// ReapExpiredIdle removes fully expired idle entries.
func (s *RedisPoolStateStore) ReapExpiredIdle(ctx context.Context, poolName string, _ time.Time) error {
	_, err := s.runReapExpired(ctx, poolName, 0)
	return err
}

// ReapExpiredIdleWithMinTTL removes expired and near-expiry idle entries.
// Returns IDs of entries that were still alive but below the TTL threshold.
func (s *RedisPoolStateStore) ReapExpiredIdleWithMinTTL(ctx context.Context, poolName string, _ time.Time, minRemaining time.Duration) (*opensandbox.ReapResult, error) {
	minMs := minRemaining.Milliseconds()
	if minMs < 0 {
		minMs = 0
	}

	discarded, err := s.runReapExpired(ctx, poolName, minMs)
	if err != nil {
		return nil, err
	}
	return &opensandbox.ReapResult{DiscardedAliveSandboxIDs: discarded}, nil
}

func (s *RedisPoolStateStore) runReapExpired(ctx context.Context, poolName string, minTTLMs int64) ([]string, error) {
	keys := []string{s.idleListKey(poolName), s.idleExpiresKey(poolName)}
	argv := []interface{}{strconv.FormatInt(minTTLMs, 10)}

	raw, err := reapExpiredScript.Run(ctx, s.client, keys, argv...).Result()
	if err != nil && err != redis.Nil {
		return nil, &opensandbox.PoolStateStoreUnavailableError{Operation: "ReapExpiredIdle", Cause: err}
	}

	if raw == nil {
		return nil, nil
	}

	list, ok := raw.([]interface{})
	if !ok {
		return nil, nil
	}

	var discarded []string
	for _, item := range list {
		if id, ok := item.(string); ok {
			discarded = append(discarded, id)
		}
	}
	return discarded, nil
}

// SnapshotCounters returns current pool counters, filtering out expired entries.
// The Lua script always returns an integer (0 when the key doesn't exist),
// so redis.Nil is not expected here.
func (s *RedisPoolStateStore) SnapshotCounters(ctx context.Context, poolName string) (*opensandbox.StoreCounters, error) {
	result, err := snapshotCountersScript.Run(ctx, s.client, []string{s.idleExpiresKey(poolName)}).Int()
	if err != nil {
		return nil, &opensandbox.PoolStateStoreUnavailableError{Operation: "SnapshotCounters", Cause: err}
	}
	return &opensandbox.StoreCounters{IdleCount: result}, nil
}

// SnapshotIdleEntries returns all current idle entries in FIFO order.
func (s *RedisPoolStateStore) SnapshotIdleEntries(ctx context.Context, poolName string) ([]opensandbox.IdleEntry, error) {
	ids, err := s.client.LRange(ctx, s.idleListKey(poolName), 0, -1).Result()
	if err != nil {
		return nil, &opensandbox.PoolStateStoreUnavailableError{Operation: "SnapshotIdleEntries", Cause: err}
	}

	expiresMap, err := s.client.HGetAll(ctx, s.idleExpiresKey(poolName)).Result()
	if err != nil {
		return nil, &opensandbox.PoolStateStoreUnavailableError{Operation: "SnapshotIdleEntries", Cause: err}
	}

	var entries []opensandbox.IdleEntry
	for _, id := range ids {
		expiresStr, ok := expiresMap[id]
		if !ok {
			continue
		}
		expiresMs, err := strconv.ParseInt(expiresStr, 10, 64)
		if err != nil {
			continue
		}
		entries = append(entries, opensandbox.IdleEntry{
			SandboxID: id,
			ExpiresAt: time.UnixMilli(expiresMs),
		})
	}
	return entries, nil
}

// GetMaxIdle returns the stored maxIdle value for the pool.
func (s *RedisPoolStateStore) GetMaxIdle(ctx context.Context, poolName string) (int, error) {
	val, err := s.client.Get(ctx, s.maxIdleKey(poolName)).Result()
	if err == redis.Nil {
		return 0, nil
	}
	if err != nil {
		return 0, &opensandbox.PoolStateStoreUnavailableError{Operation: "GetMaxIdle", Cause: err}
	}
	n, err := strconv.Atoi(val)
	if err != nil {
		return 0, &opensandbox.PoolStateStoreUnavailableError{Operation: "GetMaxIdle", Cause: err}
	}
	return n, nil
}

// SetMaxIdle persists the maxIdle value for the pool.
func (s *RedisPoolStateStore) SetMaxIdle(ctx context.Context, poolName string, maxIdle int) error {
	err := s.client.Set(ctx, s.maxIdleKey(poolName), strconv.Itoa(maxIdle), 0).Err()
	if err != nil {
		return &opensandbox.PoolStateStoreUnavailableError{Operation: "SetMaxIdle", Cause: err}
	}
	return nil
}

// SetIdleEntryTTL persists the idle entry TTL for the pool.
func (s *RedisPoolStateStore) SetIdleEntryTTL(ctx context.Context, poolName string, ttl time.Duration) error {
	ms := ttl.Milliseconds()
	if ms < 1 {
		ms = 1
	}
	err := s.client.Set(ctx, s.idleTTLKey(poolName), strconv.FormatInt(ms, 10), 0).Err()
	if err != nil {
		return &opensandbox.PoolStateStoreUnavailableError{Operation: "SetIdleEntryTTL", Cause: err}
	}
	return nil
}

// resolveIdleTTL reads the configured idle TTL from Redis (in ms).
// Falls back to DefaultIdleTimeout if not set.
func (s *RedisPoolStateStore) resolveIdleTTL(ctx context.Context, poolName string) (int64, error) {
	val, err := s.client.Get(ctx, s.idleTTLKey(poolName)).Result()
	if err == redis.Nil {
		return opensandbox.DefaultIdleTimeout.Milliseconds(), nil
	}
	if err != nil {
		return 0, &opensandbox.PoolStateStoreUnavailableError{Operation: "resolveIdleTTL", Cause: err}
	}
	ms, err := strconv.ParseInt(val, 10, 64)
	if err != nil {
		return opensandbox.DefaultIdleTimeout.Milliseconds(), nil
	}
	if ms < 1 {
		ms = 1
	}
	return ms, nil
}

// Key construction helpers.

func (s *RedisPoolStateStore) poolKey(poolName string, suffix string) string {
	encoded := base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString([]byte(poolName))
	return s.keyPrefix + ":{" + encoded + "}:" + suffix
}

func (s *RedisPoolStateStore) idleListKey(poolName string) string {
	return s.poolKey(poolName, "idle:list")
}

func (s *RedisPoolStateStore) idleExpiresKey(poolName string) string {
	return s.poolKey(poolName, "idle:expires")
}

// PrimaryLockKey returns the Redis key used for the primary (leader) lock.
func (s *RedisPoolStateStore) PrimaryLockKey(poolName string) string {
	return s.poolKey(poolName, "lock")
}

func (s *RedisPoolStateStore) maxIdleKey(poolName string) string {
	return s.poolKey(poolName, "maxIdle")
}

func (s *RedisPoolStateStore) idleTTLKey(poolName string) string {
	return s.poolKey(poolName, "idleTtlMillis")
}

// Compile-time interface check.
var _ opensandbox.PoolStateStore = (*RedisPoolStateStore)(nil)
