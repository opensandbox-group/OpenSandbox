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

package activity

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/internal/logger"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func newTestRecorder(t *testing.T, minInterval time.Duration) (*RedisRecorder, *miniredis.Miniredis, *int64) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	now := time.Now().UnixMilli()
	recorder := NewRedisRecorder(context.Background(), client, RedisConfig{
		TTL:         30 * time.Minute,
		MinInterval: minInterval,
		Logger:      mustLogger(),
	})
	recorder.nowMillis = func() int64 { return now }
	return recorder, mr, &now
}

func mustLogger() logger.Logger {
	return logger.MustNew(logger.Config{Level: "error"})
}

func waitForValue(t *testing.T, mr *miniredis.Miniredis, key string) string {
	t.Helper()
	for range 200 {
		if value, err := mr.Get(key); err == nil && value != "" {
			return value
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("activity key %s never appeared", key)
	return ""
}

func TestActivityRecordWritesTimestampWithTTL(t *testing.T) {
	recorder, mr, now := newTestRecorder(t, 0)

	recorder.Record("tenant-a", "sb-1")
	value := waitForValue(t, mr, KeyPrefix+":sb-1")
	require.Equal(t, strconv.FormatInt(*now, 10), value)

	ttl := mr.TTL(KeyPrefix + ":sb-1")
	require.Greater(t, ttl, 29*time.Minute)
}

func TestActivityMonotonicMaxNeverRegresses(t *testing.T) {
	recorder, mr, now := newTestRecorder(t, 0)
	base := *now

	recorder.Record("tenant-a", "sb-1")
	waitForValue(t, mr, KeyPrefix+":sb-1")

	*now = base - 500 // an out-of-order older observation must not regress the key
	recorder.Record("tenant-a", "sb-1")
	time.Sleep(100 * time.Millisecond)

	value, err := mr.Get(KeyPrefix + ":sb-1")
	require.NoError(t, err)
	require.Equal(t, strconv.FormatInt(base, 10), value)
}

func TestActivityCoalescesWritesPerSandbox(t *testing.T) {
	recorder, mr, now := newTestRecorder(t, time.Second)
	base := *now

	recorder.Record("tenant-a", "sb-1")
	waitForValue(t, mr, KeyPrefix+":sb-1")

	*now = base + 500 // within the one-second min interval: dropped by coalescing
	recorder.Record("tenant-a", "sb-1")
	time.Sleep(100 * time.Millisecond)

	value, err := mr.Get(KeyPrefix + ":sb-1")
	require.NoError(t, err)
	require.Equal(t, strconv.FormatInt(base, 10), value)

	*now = base + 1_500 // beyond the min interval: recorded
	recorder.Record("tenant-a", "sb-1")
	for range 200 {
		if value, _ := mr.Get(KeyPrefix + ":sb-1"); value == strconv.FormatInt(base+1_500, 10) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("activity key never advanced past the min interval")
}

func TestActivityKeyNamespacesPerSandbox(t *testing.T) {
	recorder, mr, _ := newTestRecorder(t, 0)

	recorder.Record("tenant-a", "sb-1")
	recorder.Record("tenant-a", "sb-2")
	waitForValue(t, mr, KeyPrefix+":sb-1")
	waitForValue(t, mr, KeyPrefix+":sb-2")
}

func TestActivityBatchingFlushesEveryObservation(t *testing.T) {
	recorder, mr, now := newTestRecorder(t, 0)
	const sandboxes = 150 // spans multiple pipeline batches (batch size 64)

	base := *now
	for i := range sandboxes {
		*now = base + int64(i)*(int64(i)+1)/2 // strictly increasing so every write wins the max
		recorder.Record("tenant-a", fmt.Sprintf("sb-%d", i))
	}

	for i := range sandboxes {
		key := KeyPrefix + ":" + fmt.Sprintf("sb-%d", i)
		value := waitForValue(t, mr, key)
		require.Equal(t, strconv.FormatInt(base+int64(i)*(int64(i)+1)/2, 10), value)
	}
}
