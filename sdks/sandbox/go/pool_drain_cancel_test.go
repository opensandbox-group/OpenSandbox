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

package opensandbox

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestPoolGracefulDrainHonorsContext(t *testing.T) {
	for _, mode := range []string{"cancelled", "expired", "live-cancel"} {
		t.Run(mode, func(t *testing.T) {
			pool := newTestPool(t, "http://unused.invalid", func(b *SandboxPoolBuilder) {
				b.MaxIdle(0).ReconcileInterval(time.Hour).DrainTimeout(time.Minute)
			})
			if err := pool.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			// Hold one in-flight acquire without making its cancellation depend
			// on Shutdown's context, just as real acquire callers are independent.
			atomic.StoreInt32(&pool.inFlight, 1)
			ctx, cancel := context.WithCancel(context.Background())
			want := error(context.Canceled)
			var observed chan struct{}
			switch mode {
			case "cancelled":
				cancel()
			case "expired":
				cancel()
				ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				want = context.DeadlineExceeded
			default:
				observed = make(chan struct{})
				ctx = &observedWaitContext{Context: ctx, observed: observed}
			}
			defer cancel()
			result := make(chan error, 1)
			go func() { result <- pool.Shutdown(ctx, true) }()
			returned := false
			defer func() {
				atomic.StoreInt32(&pool.inFlight, 0)
				if !returned {
					<-result // also joins the old implementation on regression failure
				}
				_ = pool.Shutdown(context.Background(), false)
			}()
			if observed != nil {
				select {
				case <-observed:
					cancel()
				case <-time.After(time.Second):
					t.Fatal("drain wait never observed caller cancellation")
				}
			}
			select {
			case err := <-result:
				returned = true
				if !errors.Is(err, want) {
					t.Fatalf("Shutdown error = %v, want %v", err, want)
				}
			case <-time.After(time.Second):
				t.Fatal("drain wait ignored the completed context")
			}
			if pool.lifecycleState != PoolLifecycleStopped {
				t.Fatalf("state = %v, want STOPPED", pool.lifecycleState)
			}
			select {
			case <-pool.shutdownDone:
			default:
				t.Fatal("shutdown completion signal was not closed")
			}
			if atomic.LoadInt32(&pool.inFlight) != 1 {
				t.Fatal("shutdown cancellation changed the independent acquire count")
			}
			atomic.StoreInt32(&pool.inFlight, 0)
			if err := pool.Start(context.Background()); err != nil {
				t.Fatalf("restart after cancelled drain: %v", err)
			}
			if err := pool.Shutdown(context.Background(), false); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPoolGracefulDrainTimeoutStillCompletes(t *testing.T) {
	pool := newTestPool(t, "http://unused.invalid", func(b *SandboxPoolBuilder) {
		b.MaxIdle(0).ReconcileInterval(time.Hour).DrainTimeout(time.Millisecond)
	})
	if err := pool.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	atomic.StoreInt32(&pool.inFlight, 1)
	defer atomic.StoreInt32(&pool.inFlight, 0)
	if err := pool.Shutdown(context.Background(), true); err != nil {
		t.Fatalf("configured drain timeout changed its result: %v", err)
	}
}
