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
	"sync"
	"testing"
	"time"
)

// observedWaitContext signals when the waiter starts selecting on cancellation.
// This keeps the live-cancellation regression independent of scheduler timing.
type observedWaitContext struct {
	context.Context
	observed chan struct{}
	once     sync.Once
}

func (c *observedWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.observed) })
	return c.Context.Done()
}

func TestPoolShutdownWaitHonorsContext(t *testing.T) {
	for _, operation := range []string{"start", "shutdown-stopped", "shutdown-draining"} {
		for _, mode := range []string{"cancel", "deadline", "live-cancel"} {
			name := operation + "/" + mode
			t.Run(name, func(t *testing.T) {
				pool := newTestPool(t, "http://unused.invalid", func(b *SandboxPoolBuilder) { b.MaxIdle(0) })
				pool.lifecycleState = PoolLifecycleStopped
				if operation == "shutdown-draining" {
					pool.lifecycleState = PoolLifecycleDraining
				}
				originalState := pool.lifecycleState
				done := make(chan struct{})
				pool.shutdownDone = done
				ctx, cancel := context.WithCancel(context.Background())
				want := error(context.Canceled)
				var observed chan struct{}
				if mode == "deadline" {
					cancel()
					ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
					want = context.DeadlineExceeded
				} else if mode == "cancel" {
					cancel()
				} else {
					observed = make(chan struct{})
					ctx = &observedWaitContext{Context: ctx, observed: observed}
				}
				defer cancel()
				result := make(chan error, 1)
				go func() {
					if operation == "start" {
						result <- pool.Start(ctx)
					} else {
						result <- pool.Shutdown(ctx, true)
					}
				}()
				// Always release the simulated primary shutdown and join the
				// waiter, including on the pre-fix failure path.
				returned := false
				defer func() {
					close(done)
					if !returned {
						<-result
					}
					_ = pool.Shutdown(context.Background(), false)
				}()
				if observed != nil {
					select {
					case <-observed:
						cancel()
					case <-time.After(time.Second):
						t.Fatal("waiter never observed its context")
					}
				}
				select {
				case err := <-result:
					returned = true
					if !errors.Is(err, want) {
						t.Fatalf("error = %v, want %v", err, want)
					}
				case <-time.After(time.Second):
					t.Fatal("waiter ignored its completed context")
				}
				pool.mu.Lock()
				unchanged := pool.lifecycleState == originalState && pool.shutdownDone == done
				pool.mu.Unlock()
				if !unchanged {
					t.Fatal("cancelled waiter changed primary shutdown state")
				}
				select {
				case <-done:
					t.Fatal("cancelled waiter closed primary shutdown signal")
				default:
				}
			})
		}
	}
}

func TestPoolShutdownWaitCompletesNormally(t *testing.T) {
	pool := newTestPool(t, "http://unused.invalid", func(b *SandboxPoolBuilder) { b.MaxIdle(0) })
	pool.lifecycleState = PoolLifecycleStopped
	pool.shutdownDone = make(chan struct{})
	close(pool.shutdownDone)
	if err := pool.Shutdown(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if err := pool.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := pool.Shutdown(context.Background(), false); err != nil {
		t.Fatal(err)
	}
}
