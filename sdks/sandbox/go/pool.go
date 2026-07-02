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

package opensandbox

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// SandboxPool maintains a pool of pre-warmed sandbox instances for
// millisecond-level acquisition. It implements OSEP-0005.
type SandboxPool struct {
	config PoolConfig

	mu       sync.Mutex
	state    PoolState
	inFlight int32 // atomic
	stopCh   chan struct{}
	doneCh   chan struct{}

	recon *reconcileState
}

// NewSandboxPool creates a new pool. Call Start to begin reconciliation.
func NewSandboxPool(config PoolConfig) *SandboxPool {
	config.applyDefaults()
	return &SandboxPool{
		config: config,
		state:  PoolStateCreated,
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
		recon:  newReconcileState(config.DegradedThreshold),
	}
}

// Start begins the background reconciliation loop. Returns an error if
// the pool configuration is invalid or the pool is already started.
func (p *SandboxPool) Start(ctx context.Context) error {
	if err := p.config.validate(); err != nil {
		return err
	}

	p.mu.Lock()
	if p.state != PoolStateCreated {
		p.mu.Unlock()
		return &PoolNotRunningError{PoolName: p.config.PoolName, State: p.state}
	}
	p.state = PoolStateRunning
	p.mu.Unlock()

	go p.runReconcileLoop(ctx)
	return nil
}

// Acquire obtains a sandbox from the pool. If no idle sandbox is available,
// behavior depends on AcquirePolicy (default: DirectCreate).
func (p *SandboxPool) Acquire(ctx context.Context, opts ...AcquireOption) (*Sandbox, error) {
	p.mu.Lock()
	if p.state != PoolStateRunning {
		state := p.state
		p.mu.Unlock()
		return nil, &PoolNotRunningError{PoolName: p.config.PoolName, State: state}
	}
	p.mu.Unlock()

	atomic.AddInt32(&p.inFlight, 1)
	defer atomic.AddInt32(&p.inFlight, -1)

	o := &acquireOpts{policy: DirectCreate}
	for _, fn := range opts {
		fn(o)
	}

	// Try to get an idle sandbox
	sandboxID, ok, err := p.config.StateStore.TryTakeIdle(ctx, p.config.PoolName)
	if err != nil {
		return nil, fmt.Errorf("pool %q: state store error: %w", p.config.PoolName, err)
	}

	if ok {
		sb, err := p.connectAndValidate(ctx, sandboxID, o.sandboxTimeout)
		if err == nil {
			return sb, nil
		}
		// Stale sandbox; clean up and fall through
		_ = p.config.StateStore.RemoveIdle(ctx, p.config.PoolName, sandboxID)
		go p.killSandbox(sandboxID)
	}

	// No idle sandbox available
	switch o.policy {
	case FailFast:
		return nil, &PoolEmptyError{PoolName: p.config.PoolName}
	default:
		return p.directCreate(ctx, o.sandboxTimeout)
	}
}

// Resize dynamically changes the target idle count.
func (p *SandboxPool) Resize(_ context.Context, maxIdle int) error {
	if maxIdle <= 0 {
		return &InvalidArgumentError{Field: "maxIdle", Message: "must be positive"}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.config.MaxIdle = maxIdle
	return nil
}

// ReleaseAllIdle kills all idle sandboxes and returns the count released.
func (p *SandboxPool) ReleaseAllIdle(ctx context.Context) (int, error) {
	count := 0
	for {
		id, ok, err := p.config.StateStore.TryTakeIdle(ctx, p.config.PoolName)
		if err != nil {
			return count, err
		}
		if !ok {
			break
		}
		go p.killSandbox(id)
		count++
	}
	return count, nil
}

// Snapshot returns a point-in-time view of the pool state.
func (p *SandboxPool) Snapshot() PoolSnapshot {
	p.mu.Lock()
	state := p.state
	p.mu.Unlock()

	idleCount, _ := p.config.StateStore.CountIdle(context.Background(), p.config.PoolName)
	failureCount, lastError, backoffActive := p.recon.snapshot()

	return PoolSnapshot{
		State:         state,
		IdleCount:     idleCount,
		InFlight:      atomic.LoadInt32(&p.inFlight),
		FailureCount:  failureCount,
		LastError:     lastError,
		BackoffActive: backoffActive,
	}
}

// Shutdown gracefully stops the pool. It waits for in-flight operations
// to complete (up to DrainTimeout), then kills remaining idle sandboxes.
func (p *SandboxPool) Shutdown(ctx context.Context) error {
	p.mu.Lock()
	if p.state == PoolStateStopped {
		p.mu.Unlock()
		return nil
	}
	p.state = PoolStateDraining
	p.mu.Unlock()

	// Signal reconciler to stop
	close(p.stopCh)

	// Wait for reconciler goroutine to finish
	select {
	case <-p.doneCh:
	case <-ctx.Done():
	}

	// Wait for in-flight operations
	deadline := time.After(p.config.DrainTimeout)
	for atomic.LoadInt32(&p.inFlight) > 0 {
		select {
		case <-deadline:
			goto done
		case <-time.After(100 * time.Millisecond):
		}
	}

done:
	// Kill all remaining idle sandboxes
	p.ReleaseAllIdle(context.Background())

	// Release the leader lock
	_ = p.config.StateStore.ReleaseLock(context.Background(), p.config.PoolName, p.config.OwnerID)

	p.mu.Lock()
	p.state = PoolStateStopped
	p.mu.Unlock()

	return nil
}

// runReconcileLoop is the background goroutine that maintains idle sandbox count.
func (p *SandboxPool) runReconcileLoop(ctx context.Context) {
	defer close(p.doneCh)

	// Immediate first tick
	p.runReconcileTick(ctx)

	ticker := time.NewTicker(p.config.ReconcileInterval)
	defer ticker.Stop()

	for {
		select {
		case <-p.stopCh:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.runReconcileTick(ctx)
		}
	}
}

func (p *SandboxPool) runReconcileTick(ctx context.Context) {
	// Acquire leader lock
	acquired, err := p.config.StateStore.TryAcquireLock(ctx, p.config.PoolName, p.config.OwnerID, p.config.PrimaryLockTTL)
	if err != nil || !acquired {
		return
	}

	// Reap expired idle entries
	_ = p.config.StateStore.ReapExpired(ctx, p.config.PoolName, time.Now())

	// Check if in backoff
	if p.recon.isBackoffActive() {
		return
	}

	// Compute deficit
	currentIdle, err := p.config.StateStore.CountIdle(ctx, p.config.PoolName)
	if err != nil {
		return
	}

	p.mu.Lock()
	maxIdle := p.config.MaxIdle
	p.mu.Unlock()

	deficit := maxIdle - currentIdle
	if deficit <= 0 {
		return
	}

	// Limit concurrent warmup
	toCreate := deficit
	if toCreate > p.config.WarmupConcurrency {
		toCreate = p.config.WarmupConcurrency
	}

	// Create sandboxes
	var wg sync.WaitGroup
	failures := int32(0)

	for i := 0; i < toCreate; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := p.warmupOne(ctx); err != nil {
				atomic.AddInt32(&failures, 1)
			}
		}()
	}
	wg.Wait()

	// Record results
	failCount := int(atomic.LoadInt32(&failures))
	if failCount > 0 {
		p.recon.recordFailure(fmt.Sprintf("%d/%d warmup failures", failCount, toCreate))
	}
	if failCount < toCreate {
		p.recon.recordSuccess()
	}

	// Renew lock after work
	_, _ = p.config.StateStore.RenewLock(ctx, p.config.PoolName, p.config.OwnerID, p.config.PrimaryLockTTL)
}

func (p *SandboxPool) warmupOne(ctx context.Context) error {
	spec := p.config.CreationSpec
	timeout := spec.Timeout
	if timeout == 0 {
		timeout = DefaultTimeoutSeconds
	}

	sb, err := CreateSandbox(ctx, p.config.ConnectionConfig, SandboxCreateOptions{
		Image:          spec.Image,
		Entrypoint:     spec.Entrypoint,
		ResourceLimits: spec.ResourceLimits,
		TimeoutSeconds: &timeout,
		Env:            spec.Env,
		Metadata:       spec.Metadata,
		NetworkPolicy:  spec.NetworkPolicy,
		Volumes:        spec.Volumes,
		Extensions:     spec.Extensions,
	})
	if err != nil {
		return fmt.Errorf("warmup create: %w", err)
	}

	// Run optional preparer
	if p.config.SandboxPreparer != nil {
		if err := p.config.SandboxPreparer(ctx, sb); err != nil {
			_ = sb.Kill(context.Background())
			return fmt.Errorf("warmup preparer: %w", err)
		}
	}

	// Put into idle set
	expiresAt := time.Now().Add(p.config.IdleTimeout)
	if err := p.config.StateStore.PutIdle(ctx, p.config.PoolName, sb.ID(), expiresAt); err != nil {
		_ = sb.Kill(context.Background())
		return fmt.Errorf("warmup put idle: %w", err)
	}

	return nil
}

func (p *SandboxPool) connectAndValidate(ctx context.Context, sandboxID string, renewDuration time.Duration) (*Sandbox, error) {
	readyOpts := ReadyOptions{
		Timeout:         p.config.AcquireReadyTimeout,
		PollingInterval: p.config.AcquireHealthCheckInterval,
		HealthCheck:     p.config.HealthCheck,
	}

	sb, err := ConnectSandbox(ctx, p.config.ConnectionConfig, sandboxID, readyOpts)
	if err != nil {
		return nil, err
	}

	// Renew TTL if requested
	if renewDuration > 0 {
		if _, err := sb.Renew(ctx, renewDuration); err != nil {
			_ = sb.Close()
			return nil, fmt.Errorf("renew after acquire: %w", err)
		}
	}

	return sb, nil
}

func (p *SandboxPool) directCreate(ctx context.Context, timeout time.Duration) (*Sandbox, error) {
	spec := p.config.CreationSpec
	t := spec.Timeout
	if timeout > 0 {
		secs := int(timeout.Seconds())
		t = secs
	}
	if t == 0 {
		t = DefaultTimeoutSeconds
	}

	return CreateSandbox(ctx, p.config.ConnectionConfig, SandboxCreateOptions{
		Image:          spec.Image,
		Entrypoint:     spec.Entrypoint,
		ResourceLimits: spec.ResourceLimits,
		TimeoutSeconds: &t,
		Env:            spec.Env,
		Metadata:       spec.Metadata,
		NetworkPolicy:  spec.NetworkPolicy,
		Volumes:        spec.Volumes,
		Extensions:     spec.Extensions,
		HealthCheck:    p.config.HealthCheck,
	})
}

func (p *SandboxPool) killSandbox(sandboxID string) {
	lc := p.config.ConnectionConfig.lifecycleClient()
	_ = lc.DeleteSandbox(context.Background(), sandboxID)
}
