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

package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	opensandbox "github.com/alibaba/OpenSandbox/sdks/sandbox/go"
)

func TestDesiredMaxIdle(t *testing.T) {
	for _, tt := range []struct {
		name, zone, instant string
		want                int
	}{
		{"before opening", "Asia/Shanghai", "2026-10-05T00:59:59Z", 0},
		{"opening inclusive", "Asia/Shanghai", "2026-10-05T01:00:00Z", 5},
		{"before closing", "Asia/Shanghai", "2026-10-05T12:59:59Z", 5},
		{"closing exclusive", "Asia/Shanghai", "2026-10-05T13:00:00Z", 0},
		{"Friday", "Asia/Shanghai", "2026-10-09T04:00:00Z", 5},
		{"Saturday", "Asia/Shanghai", "2026-10-10T04:00:00Z", 0},
		{"Sunday", "Asia/Shanghai", "2026-10-11T04:00:00Z", 0},
		{"local Friday UTC Saturday", "Pacific/Honolulu", "2026-10-10T06:00:00Z", 5},
		{"before US spring DST", "America/New_York", "2026-03-06T14:00:00Z", 5},
		{"after US spring DST", "America/New_York", "2026-03-09T13:00:00Z", 5},
		{"before US fall DST", "America/New_York", "2026-10-30T13:00:00Z", 5},
		{"after US fall DST before opening", "America/New_York", "2026-11-02T13:00:00Z", 0},
		{"after US fall DST opening", "America/New_York", "2026-11-02T14:00:00Z", 5},
	} {
		t.Run(tt.name, func(t *testing.T) {
			location, err := time.LoadLocation(tt.zone)
			if err != nil {
				t.Fatal(err)
			}
			now, err := time.Parse(time.RFC3339, tt.instant)
			if err != nil {
				t.Fatal(err)
			}
			if got := desiredMaxIdle(now, location, 5, 0); got != tt.want {
				t.Fatalf("target = %d, want %d", got, tt.want)
			}
		})
	}
	if got := desiredMaxIdle(time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC), time.UTC, 10, 2); got != 2 {
		t.Fatalf("custom offpeak target = %d, want 2", got)
	}
}

type observedStore struct {
	opensandbox.PoolStateStore
	writes   int
	readErr  error
	writeErr error
}

func (s *observedStore) GetMaxIdle(ctx context.Context, name string) (int, error) {
	if s.readErr != nil {
		return 0, s.readErr
	}
	return s.PoolStateStore.GetMaxIdle(ctx, name)
}

func (s *observedStore) SetMaxIdle(ctx context.Context, name string, target int) error {
	s.writes++
	if s.writeErr != nil {
		return s.writeErr
	}
	return s.PoolStateStore.SetMaxIdle(ctx, name, target)
}

func testPool(t *testing.T) (*opensandbox.DefaultSandboxPool, *observedStore) {
	t.Helper()
	store := &observedStore{PoolStateStore: opensandbox.NewInMemoryPoolStateStore()}
	pool, err := opensandbox.NewSandboxPoolBuilder().
		PoolName("schedule-test").MaxIdle(0).StateStore(store).
		ConnectionConfig(opensandbox.ConnectionConfig{}).
		CreationSpec(opensandbox.PoolCreationSpec{Image: "ubuntu:22.04"}).Build()
	if err != nil {
		t.Fatal(err)
	}
	// Resize and Snapshot work without starting the warmup loop or a server.
	return pool, store
}

func TestResizeForTime(t *testing.T) {
	pool, store := testPool(t)
	ctx := context.Background()
	for _, tt := range []struct {
		hour    int
		changed bool
		target  int
		writes  int
	}{
		{8, false, 0, 0},
		{9, true, 5, 1},
		{12, false, 5, 1}, // Idle count is still zero; compare the target, not idle count.
		{21, true, 0, 2},
		{23, false, 0, 2},
	} {
		now := time.Date(2026, 10, 5, tt.hour, 0, 0, 0, time.UTC)
		changed, err := resizeForTime(ctx, pool, now, time.UTC, 5, 0)
		if err != nil || changed != tt.changed {
			t.Fatalf("hour %d: changed=%v err=%v", tt.hour, changed, err)
		}
		snapshot, err := pool.Snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.MaxIdle != tt.target || store.writes != tt.writes {
			t.Fatalf("hour %d: target=%d writes=%d, want %d/%d", tt.hour, snapshot.MaxIdle, store.writes, tt.target, tt.writes)
		}
	}
	// Observe an external target write instead of relying on a cached last value.
	if err := store.PoolStateStore.SetMaxIdle(ctx, "schedule-test", 3); err != nil {
		t.Fatal(err)
	}
	changed, err := resizeForTime(ctx, pool, time.Date(2026, 10, 5, 23, 0, 0, 0, time.UTC), time.UTC, 5, 0)
	if err != nil || !changed || store.writes != 3 {
		t.Fatalf("external change: changed=%v writes=%d err=%v", changed, store.writes, err)
	}
}

func TestResizeForTimeErrors(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	for _, operation := range []string{"read", "write"} {
		t.Run(operation, func(t *testing.T) {
			pool, store := testPool(t)
			failure := errors.New("store unavailable")
			if operation == "read" {
				store.readErr = failure
			} else {
				store.writeErr = failure
			}
			changed, err := resizeForTime(context.Background(), pool, now, time.UTC, 5, 0)
			if changed || !errors.Is(err, failure) {
				t.Fatalf("changed=%v err=%v, want original failure", changed, err)
			}
			if operation == "read" && store.writes != 0 {
				t.Fatal("must not resize after a failed read")
			}
		})
	}
	for _, destroyed := range []bool{false, true} {
		pool, store := testPool(t)
		ctx := context.Background()
		if err := store.BeginDestroy(ctx, "schedule-test", "retiring-owner"); err != nil {
			t.Fatal(err)
		}
		if destroyed {
			if err := store.MarkDestroyed(ctx, "schedule-test", "retiring-owner", 0); err != nil {
				t.Fatal(err)
			}
		}
		changed, err := resizeForTime(ctx, pool, now, time.UTC, 5, 0)
		var fenced *opensandbox.PoolDestroyedError
		if changed || !errors.As(err, &fenced) {
			t.Fatalf("destroyed=%v: changed=%v err=%v, want namespace fence", destroyed, changed, err)
		}
	}
}

func TestRunSchedule(t *testing.T) {
	t.Run("periodic evaluation", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		calls := 0
		err := runSchedule(ctx, time.Millisecond, func(time.Time) error {
			calls++
			if calls == 2 {
				cancel()
			}
			return nil
		})
		if err != nil || calls != 2 {
			t.Fatalf("calls=%d err=%v, want two schedule evaluations", calls, err)
		}
	})
	t.Run("apply immediately and cancel", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		calls := 0
		err := runSchedule(ctx, time.Hour, func(time.Time) error {
			calls++
			cancel()
			return nil
		})
		if err != nil || calls != 1 {
			t.Fatalf("calls=%d err=%v", calls, err)
		}
	})
	t.Run("stop on failure", func(t *testing.T) {
		failure := errors.New("resize failed")
		err := runSchedule(context.Background(), time.Hour, func(time.Time) error { return failure })
		if !errors.Is(err, failure) {
			t.Fatalf("err=%v, want original failure", err)
		}
	})
	t.Run("already canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := runSchedule(ctx, time.Hour, func(time.Time) error {
			t.Fatal("must not resize after cancellation")
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})
}

func TestRunCancellationWithZeroCapacity(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// Exercise startup and shutdown with the actual SDK, without creating resources.
	if err := run(ctx, time.UTC, 0, 0, time.Hour, "ubuntu:22.04"); err != nil {
		t.Fatal(err)
	}
}

func TestShutdownPoolWaitsForKill(t *testing.T) {
	requestStarted := make(chan struct{})
	releaseKill := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/v1/sandboxes/idle-1" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		close(requestStarted)
		<-releaseKill
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() {
		select {
		case <-releaseKill:
		default:
			close(releaseKill)
		}
	})
	store := opensandbox.NewInMemoryPoolStateStore()
	pool, err := opensandbox.NewSandboxPoolBuilder().
		PoolName("shutdown-test").MaxIdle(0).StateStore(store).
		ConnectionConfig(opensandbox.ConnectionConfig{Domain: server.URL}).
		CreationSpec(opensandbox.PoolCreationSpec{Image: "ubuntu:22.04"}).Build()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutIdle(context.Background(), "shutdown-test", "idle-1"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- shutdownPool(pool) }()
	select {
	case <-requestStarted:
	case err := <-done:
		t.Fatalf("cleanup returned before the kill request started: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("kill request did not start")
	}
	select {
	case err := <-done:
		t.Fatalf("cleanup returned while the kill request was pending: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseKill)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup did not finish after the kill response")
	}
	snapshot, err := pool.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.IdleCount != 0 {
		t.Fatalf("remaining idle = %d, want 0", snapshot.IdleCount)
	}
}
