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
	"testing"
	"time"
)

type acquireCancellationStore struct {
	*InMemoryPoolStateStore
	onCheck func()
	onTake  func()
	takes   int
}

func (s *acquireCancellationStore) GetDestroyState(ctx context.Context, name string) (PoolDestroyState, error) {
	if s.onCheck != nil {
		s.onCheck()
	}
	return s.InMemoryPoolStateStore.GetDestroyState(ctx, name)
}

func (s *acquireCancellationStore) TryTakeIdle(ctx context.Context, name string) (string, error) {
	s.takes++
	if s.onTake != nil {
		s.onTake()
	}
	return s.InMemoryPoolStateStore.TryTakeIdle(ctx, name)
}

func (s *acquireCancellationStore) TryTakeIdleWithMinTTL(ctx context.Context, name string, minTTL time.Duration) (*TakeIdleResult, error) {
	s.takes++
	if s.onTake != nil {
		s.onTake()
	}
	return s.InMemoryPoolStateStore.TryTakeIdleWithMinTTL(ctx, name, minTTL)
}

func TestPoolAcquireCancelledBeforeTakePreservesIdle(t *testing.T) {
	for _, policy := range []AcquirePolicy{AcquirePolicyFailFast, AcquirePolicyDirectCreate, AcquirePolicyRetryNextIdle, AcquirePolicyRetryNextIdleThenCreate} {
		for _, mode := range []string{"cancelled", "expired", "cancelled-during-namespace-check"} {
			for _, minTTL := range []time.Duration{0, time.Second} {
				t.Run(policy.String()+"/"+mode+"/"+minTTL.String(), func(t *testing.T) {
					server := newMockLifecycleServer(t, "http://unused.invalid")
					store := &acquireCancellationStore{InMemoryPoolStateStore: NewInMemoryPoolStateStore()}
					pool := newTestPool(t, server.URL, func(b *SandboxPoolBuilder) {
						b.MaxIdle(0).ReconcileInterval(time.Hour).StateStore(store).AcquireMinRemainingTTL(minTTL)
					})
					// Exercise both take primitives; the builder replaces zero with a default.
					pool.config.AcquireMinRemainingTTL = minTTL
					if err := pool.Start(context.Background()); err != nil {
						t.Fatal(err)
					}
					defer pool.Shutdown(context.Background(), false)
					if err := store.PutIdle(context.Background(), "test-pool", "healthy-idle"); err != nil {
						t.Fatal(err)
					}
					ctx, cancel := context.WithCancel(context.Background())
					want := error(context.Canceled)
					switch mode {
					case "expired":
						cancel()
						ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
						want = context.DeadlineExceeded
					case "cancelled":
						cancel()
					default:
						store.onCheck = cancel
					}
					defer cancel()
					_, err := pool.Acquire(ctx, AcquireOptions{Policy: &policy, SkipHealthCheck: true})
					if !errors.Is(err, want) {
						t.Errorf("Acquire error = %v, want %v", err, want)
					}
					if store.takes != 0 {
						t.Errorf("cancelled acquire attempted %d destructive takes", store.takes)
					}
					entries, err := store.SnapshotIdleEntries(context.Background(), "test-pool")
					if err != nil || len(entries) != 1 || entries[0].SandboxID != "healthy-idle" {
						t.Fatalf("idle entries = %v, err = %v; cancelled acquire consumed the idle sandbox", entries, err)
					}
				})
			}
		}
	}
}

func TestPoolAcquireCancelledDoesNotDirectCreate(t *testing.T) {
	for _, policy := range []AcquirePolicy{AcquirePolicyDirectCreate, AcquirePolicyRetryNextIdleThenCreate} {
		for _, mode := range []string{"before-acquire", "during-empty-take"} {
			t.Run(policy.String()+"/"+mode, func(t *testing.T) {
				creates := 0
				store := &acquireCancellationStore{InMemoryPoolStateStore: NewInMemoryPoolStateStore()}
				pool := newTestPool(t, "http://unused.invalid", func(b *SandboxPoolBuilder) {
					b.MaxIdle(0).ReconcileInterval(time.Hour).StateStore(store).SandboxCreator(&mockSandboxCreator{
						createFn: func(context.Context, PooledSandboxCreateContext) (*Sandbox, error) {
							creates++
							return &Sandbox{id: "unexpected"}, nil
						},
					})
				})
				if err := pool.Start(context.Background()); err != nil {
					t.Fatal(err)
				}
				defer pool.Shutdown(context.Background(), false)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if mode == "before-acquire" {
					cancel()
				} else {
					store.onTake = cancel
				}
				_, err := pool.Acquire(ctx, AcquireOptions{Policy: &policy, SkipHealthCheck: true})
				if !errors.Is(err, context.Canceled) || creates != 0 {
					t.Fatalf("Acquire error = %v, creates = %d; want cancellation without creation", err, creates)
				}
			})
		}
	}
}
