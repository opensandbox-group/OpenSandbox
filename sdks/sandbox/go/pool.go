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
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// SandboxPool is the interface for a client-side sandbox pool.
type SandboxPool interface {
	Start(ctx context.Context) error
	Acquire(ctx context.Context, opts AcquireOptions) (*Sandbox, error)
	ReleaseAllIdle(ctx context.Context) (int, error)
	Resize(ctx context.Context, newMaxIdle int) error
	Snapshot(ctx context.Context) (*PoolSnapshot, error)
	SnapshotIdleEntries(ctx context.Context) ([]IdleEntry, error)
	Shutdown(ctx context.Context, graceful bool) error
}

var _ SandboxPool = (*DefaultSandboxPool)(nil)

// DefaultSandboxPool implements SandboxPool.
type DefaultSandboxPool struct {
	config  *PoolConfig
	manager *SandboxManager

	mu             sync.Mutex
	lifecycleState PoolLifecycleState
	healthState    PoolHealthState

	reconciler   *reconcileState
	reconMu      sync.Mutex // serializes reconcile ticks
	ticker       *time.Ticker
	done         chan struct{}
	doneClosed   bool
	wg           sync.WaitGroup
	shutdownDone chan struct{} // closed when Shutdown fully completes
	inFlight     int32
	reconCancel  context.CancelFunc
}

// Start begins the background reconciliation loop.
func (p *DefaultSandboxPool) Start(ctx context.Context) error {
	p.mu.Lock()

	if p.lifecycleState == PoolLifecycleRunning || p.lifecycleState == PoolLifecycleStarting {
		p.mu.Unlock()
		return nil
	}

	if p.lifecycleState == PoolLifecycleDraining {
		p.mu.Unlock()
		return &PoolNotRunningError{PoolName: p.config.PoolName, State: PoolLifecycleDraining}
	}

	// If restarting from STOPPED, wait for the previous shutdown to fully
	// complete before creating new goroutines on the same WaitGroup.
	if p.lifecycleState == PoolLifecycleStopped && p.shutdownDone != nil {
		ch := p.shutdownDone
		p.mu.Unlock()
		<-ch
		p.mu.Lock()
		// Re-check after re-acquiring lock — another goroutine may have started or shutdown initiated.
		if p.lifecycleState == PoolLifecycleRunning || p.lifecycleState == PoolLifecycleStarting {
			p.mu.Unlock()
			return nil
		}
		if p.lifecycleState == PoolLifecycleDraining {
			p.mu.Unlock()
			return &PoolNotRunningError{PoolName: p.config.PoolName, State: PoolLifecycleDraining}
		}
	}

	p.lifecycleState = PoolLifecycleStarting
	startMaxIdle := p.config.MaxIdle
	p.mu.Unlock()

	// Refuse to bind a retired namespace. Only a definite fence blocks startup;
	// a store outage is left to the writes below to surface.
	if err := p.ensureNamespaceActive(ctx); err != nil {
		var destroyed *PoolDestroyedError
		if errors.As(err, &destroyed) {
			p.mu.Lock()
			if p.lifecycleState == PoolLifecycleStarting {
				p.lifecycleState = PoolLifecycleNotStarted
			}
			p.mu.Unlock()
			return err
		}
	}

	// Initialize state store with pool configuration.
	if err := p.config.StateStore.SetMaxIdle(ctx, p.config.PoolName, startMaxIdle); err != nil {
		p.mu.Lock()
		if p.lifecycleState == PoolLifecycleStarting {
			p.lifecycleState = PoolLifecycleNotStarted
		}
		p.mu.Unlock()
		return fmt.Errorf("opensandbox: pool start: failed to set maxIdle: %w", err)
	}
	if err := p.config.StateStore.SetIdleEntryTTL(ctx, p.config.PoolName, p.config.IdleTimeout); err != nil {
		p.mu.Lock()
		if p.lifecycleState == PoolLifecycleStarting {
			p.lifecycleState = PoolLifecycleNotStarted
		}
		p.mu.Unlock()
		return fmt.Errorf("opensandbox: pool start: failed to set idle TTL: %w", err)
	}

	p.mu.Lock()
	// Re-check: a concurrent Shutdown() may have run while we were unlocked.
	if p.lifecycleState != PoolLifecycleStarting {
		currentState := p.lifecycleState
		p.mu.Unlock()
		if currentState == PoolLifecycleRunning {
			return nil
		}
		return &PoolNotRunningError{PoolName: p.config.PoolName, State: currentState}
	}
	if p.config.PrimaryLockTTL <= p.config.WarmupReadyTimeout {
		p.config.Logger.Warn("pool primary lock TTL may expire during warmup; "+
			"configure PrimaryLockTTL greater than WarmupReadyTimeout plus expected preparer time",
			"pool_name", p.config.PoolName,
			"primary_lock_ttl", p.config.PrimaryLockTTL,
			"warmup_ready_timeout", p.config.WarmupReadyTimeout)
	}
	p.reconciler = newReconcileState(p.config.DegradedThreshold)
	p.ticker = time.NewTicker(p.config.ReconcileInterval)
	p.done = make(chan struct{})
	p.doneClosed = false
	p.shutdownDone = make(chan struct{})

	reconCtx, reconCancel := context.WithCancel(context.Background())
	p.reconCancel = reconCancel

	p.wg.Add(1)
	go p.reconcileLoop(reconCtx)

	// Trigger immediate first tick if maxIdle > 0.
	if p.config.MaxIdle > 0 {
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			p.runReconcileTick(reconCtx)
			p.syncHealthState()
		}()
	}

	p.lifecycleState = PoolLifecycleRunning
	maxIdle := p.config.MaxIdle
	p.mu.Unlock()

	p.config.Logger.Info("pool started",
		"pool_name", p.config.PoolName,
		"max_idle", maxIdle)
	return nil
}

func (p *DefaultSandboxPool) reconcileLoop(ctx context.Context) {
	defer p.wg.Done()
	for {
		select {
		case <-p.done:
			return
		case <-p.ticker.C:
			if p.reconciler.shouldBackoff() {
				continue
			}
			// Do not use ReconcileInterval as context timeout — the interval
			// controls how often ticks fire, not how long each tick may run.
			// Sandbox creation has its own timeouts (WarmupReadyTimeout).
			p.runReconcileTick(ctx)
			p.syncHealthState()
		}
	}
}

func (p *DefaultSandboxPool) syncHealthState() {
	p.mu.Lock()
	hs, _, _, _ := p.reconciler.snapshot()
	p.healthState = hs
	p.mu.Unlock()
}

func (p *DefaultSandboxPool) runReconcileTick(ctx context.Context) {
	p.reconMu.Lock()
	defer p.reconMu.Unlock()

	// A destroy fences the namespace for every peer. Stop rather than keep
	// replenishing a pool that is being retired.
	if err := p.ensureNamespaceActive(ctx); err != nil {
		var destroyed *PoolDestroyedError
		if errors.As(err, &destroyed) {
			p.stopAfterNamespaceDestroyed(destroyed.State)
			return
		}
	}

	createFn := func(ctx context.Context, reason PooledSandboxCreateReason) (string, error) {
		return p.createOneSandbox(ctx, reason)
	}
	deleteFn := func(sandboxID string) {
		p.killSandboxBestEffort(sandboxID)
	}
	reconcileTick(ctx, p.config, p.config.StateStore, p.reconciler, p.config.Logger, createFn, deleteFn)
}

// Acquire takes or creates a sandbox from the pool.
func (p *DefaultSandboxPool) Acquire(ctx context.Context, opts AcquireOptions) (*Sandbox, error) {
	// Lifecycle guard + in-flight tracking (atomic under lock).
	p.mu.Lock()
	state := p.lifecycleState
	if state != PoolLifecycleRunning {
		p.mu.Unlock()
		return nil, &PoolNotRunningError{PoolName: p.config.PoolName, State: state}
	}
	atomic.AddInt32(&p.inFlight, 1)
	p.mu.Unlock()
	defer atomic.AddInt32(&p.inFlight, -1)

	// Resolve policy.
	policy := p.config.EmptyBehavior
	if opts.Policy != nil {
		policy = *opts.Policy
	}

	// A fenced namespace must not mint new sandboxes, so this has to run before the
	// direct-create fallthrough below and not only on the store write paths.
	if err := p.ensureNamespaceActiveForAcquire(ctx, policy); err != nil {
		return nil, err
	}

	// Resolve minTTL.
	minTTL := p.config.AcquireMinRemainingTTL
	if opts.MinRemainingTTL > 0 {
		minTTL = opts.MinRemainingTTL
	}

	// Bounded retry across up to `maxAttempts` idle candidates. FailFast / DirectCreate remain
	// single-shot (maxAttempts=1) to preserve their existing latency profile; the RetryNextIdle
	// variants use the configured MaxAcquireRetries (default 3).
	maxAttempts := effectiveMaxIdleAttempts(policy, p.config.MaxAcquireRetries)

	// Accumulate discarded-alive across all iterations so we schedule a single deferred cleanup.
	var pendingKill []string
	var lastIdleAttemptErr error
	var lastSandboxID string
	attemptedAny := false
	loopExhausted := true

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		takeResult, takeErr := p.tryTakeIdle(ctx, minTTL)
		if takeErr != nil {
			// Under FailFast / RetryNextIdle (no fallback), propagate the store error immediately.
			// Under DirectCreate / RetryNextIdleThenCreate, treat store outage as a cache miss and
			// fall through to direct create so the pool remains at least as available as raw SDK
			// usage during store outages (OSEP-0005 error-code matrix).
			if !policyFallsThroughToDirectCreate(policy) {
				go p.killDiscardedAliveSandboxes(pendingKill)
				return nil, &PoolStateStoreUnavailableError{Operation: "TryTakeIdle", Cause: takeErr}
			}
			p.config.Logger.Warn("acquire: state store unavailable, falling through to direct create",
				"pool_name", p.config.PoolName,
				"error", takeErr)
			loopExhausted = false
			break
		}
		if takeResult != nil && len(takeResult.DiscardedAliveSandboxIDs) > 0 {
			pendingKill = append(pendingKill, takeResult.DiscardedAliveSandboxIDs...)
		}
		if takeResult == nil || takeResult.SandboxID == "" {
			// Idle buffer drained mid-loop (or was empty from the start). Stop retrying — another
			// take round-trip is pure overhead.
			loopExhausted = false
			break
		}

		lastSandboxID = takeResult.SandboxID
		attemptedAny = true

		// Try to connect to the idle sandbox (health check is integrated into ready-poll).
		sb, connectErr := p.connectIdle(ctx, takeResult.SandboxID, opts)
		if connectErr != nil {
			// Connect / readiness / health-check failed — the idle candidate itself is unusable.
			// Remove it, best-effort kill, then either retry (RetryNextIdle*) or fall through
			// (single-shot policies).
			lastIdleAttemptErr = connectErr
			_ = p.config.StateStore.RemoveIdle(ctx, p.config.PoolName, takeResult.SandboxID)
			go p.killSandboxBestEffort(takeResult.SandboxID)
			p.config.Logger.Warn("acquire: idle sandbox connect/health check failed",
				"pool_name", p.config.PoolName,
				"sandbox_id", takeResult.SandboxID,
				"policy", policy,
				"attempt", attempt,
				"max_attempts", maxAttempts,
				"error", connectErr)

			// Respect the caller's cancellation between iterations so a long retry loop doesn't
			// keep paying AcquireReadyTimeout after the context has been cancelled.
			if err := ctx.Err(); err != nil {
				go p.killDiscardedAliveSandboxes(pendingKill)
				return nil, &PoolAcquireFailedError{PoolName: p.config.PoolName, Cause: err}
			}
			// Re-check pool lifecycle between iterations. Shutdown(ctx, true) uses its own ctx
			// to drive draining and does NOT cancel the caller's acquire ctx, so without this
			// check the loop could keep paying AcquireReadyTimeout per retry while shutdown
			// waits on inFlight.
			p.mu.Lock()
			currentState := p.lifecycleState
			p.mu.Unlock()
			if currentState != PoolLifecycleRunning {
				go p.killDiscardedAliveSandboxes(pendingKill)
				return nil, &PoolNotRunningError{PoolName: p.config.PoolName, State: currentState}
			}
			// A destroy may have landed since the preflight check. Stop retrying rather
			// than pop further idle IDs out from under the drain.
			if err := p.ensureNamespaceActiveForAcquire(ctx, policy); err != nil {
				go p.killDiscardedAliveSandboxes(pendingKill)
				return nil, err
			}
			continue
		}
		// Connect + readiness succeeded. From here on the sandbox is a healthy, borrowable idle:
		// any failure below (renew rejection, e.g. lifecycle API temporarily failing renew) is
		// NOT a candidate-specific problem, so we must not treat it as "stale idle" and burn
		// another retry. But TryTakeIdle already popped this ID out of the store, so if we only
		// Close() locally the remote sandbox stays alive on the server until its TTL expires and
		// is no longer tracked anywhere. Kill the remote sandbox best-effort, close local
		// resources, and surface the raw error.
		if opts.SandboxTimeout > 0 {
			if _, renewErr := sb.Renew(ctx, opts.SandboxTimeout); renewErr != nil {
				p.config.Logger.Warn("acquire: renew failed after idle connect; killing remote "+
					"sandbox and not retrying (renew errors are not candidate-specific)",
					"pool_name", p.config.PoolName,
					"sandbox_id", takeResult.SandboxID,
					"policy", policy,
					"error", renewErr)
				go p.killSandboxBestEffort(takeResult.SandboxID)
				_ = sb.Close()
				go p.killDiscardedAliveSandboxes(pendingKill)
				return nil, fmt.Errorf("opensandbox: pool acquire: renew after connect failed: %w", renewErr)
			}
		}
		// TryTakeIdle is unfenced so the destroy manager can drain, so this ID is
		// already out of the store and a destroy can no longer reach it. Re-check
		// before handing it over, fail-closed: if the store cannot confirm the
		// namespace is ACTIVE, kill the sandbox rather than leak it into a
		// namespace that may be retired.
		if err := p.ensureNamespaceActiveAfterCreate(ctx, sb, nil); err != nil {
			go p.killDiscardedAliveSandboxes(pendingKill)
			return nil, err
		}
		go p.killDiscardedAliveSandboxes(pendingKill)
		p.config.Logger.Debug("acquire: from idle",
			"pool_name", p.config.PoolName,
			"sandbox_id", takeResult.SandboxID,
			"policy", policy,
			"attempt", attempt,
			"max_attempts", maxAttempts)
		return sb, nil
	}

	// Reached end of loop without a successful acquire. Fire deferred cleanup asynchronously
	// so neither the error return nor the direct-create fallthrough waits on kill RPCs.
	go p.killDiscardedAliveSandboxes(pendingKill)

	if !policyFallsThroughToDirectCreate(policy) {
		if attemptedAny {
			return nil, &PoolAcquireFailedError{PoolName: p.config.PoolName, Cause: lastIdleAttemptErr}
		}
		return nil, &PoolEmptyError{PoolName: p.config.PoolName, Policy: policy}
	}

	// DIRECT_CREATE / RETRY_NEXT_IDLE_THEN_CREATE fallthrough.
	p.config.Logger.Debug("acquire: falling through to direct create",
		"pool_name", p.config.PoolName,
		"policy", policy,
		"attempted_any", attemptedAny,
		"loop_exhausted", loopExhausted,
		"last_sandbox_id", lastSandboxID)
	return p.directCreate(ctx, opts, policy)
}

// tryTakeIdle wraps the store's take primitives, returning a nil result on a legitimate empty
// (as opposed to an outage). This keeps the Acquire loop's control flow linear.
func (p *DefaultSandboxPool) tryTakeIdle(ctx context.Context, minTTL time.Duration) (*TakeIdleResult, error) {
	if minTTL > 0 {
		return p.config.StateStore.TryTakeIdleWithMinTTL(ctx, p.config.PoolName, minTTL)
	}
	sandboxID, err := p.config.StateStore.TryTakeIdle(ctx, p.config.PoolName)
	if err != nil {
		return nil, err
	}
	return &TakeIdleResult{SandboxID: sandboxID}, nil
}

// effectiveMaxIdleAttempts is the per-acquire cap on idle candidates. Single-shot policies always
// try exactly one; retry policies use the configured budget clamped to >= 1.
func effectiveMaxIdleAttempts(policy AcquirePolicy, maxAcquireRetries int) int {
	switch policy {
	case AcquirePolicyRetryNextIdle, AcquirePolicyRetryNextIdleThenCreate:
		if maxAcquireRetries < 1 {
			return 1
		}
		return maxAcquireRetries
	default:
		return 1
	}
}

// policyFallsThroughToDirectCreate reports whether the given policy, after exhausting its idle
// budget, should silently create a fresh sandbox instead of returning an error.
func policyFallsThroughToDirectCreate(policy AcquirePolicy) bool {
	switch policy {
	case AcquirePolicyDirectCreate, AcquirePolicyRetryNextIdleThenCreate:
		return true
	default:
		return false
	}
}

// connectIdle connects to an existing idle sandbox and waits for readiness (health check is
// integrated into the ready-poll). Deliberately does NOT call Renew: the caller must decide
// whether a renew failure should tear down the sandbox and retry (never — renew errors are
// not candidate-specific) or bubble up as a non-retryable acquire failure.
func (p *DefaultSandboxPool) connectIdle(ctx context.Context, sandboxID string, opts AcquireOptions) (*Sandbox, error) {
	if opts.SkipHealthCheck {
		return ConnectSandbox(ctx, p.config.ConnectionConfig, sandboxID)
	}
	return ConnectSandbox(ctx, p.config.ConnectionConfig, sandboxID, ReadyOptions{
		Timeout:         p.config.AcquireReadyTimeout,
		PollingInterval: p.config.AcquireHealthCheckPollingInterval,
		HealthCheck:     p.adaptAcquireHealthCheck(),
	})
}

func (p *DefaultSandboxPool) directCreate(ctx context.Context, opts AcquireOptions, policy AcquirePolicy) (*Sandbox, error) {
	var sb *Sandbox
	var err error

	if p.config.SandboxCreator != nil {
		createCtx := PooledSandboxCreateContext{
			PoolName:                   p.config.PoolName,
			OwnerID:                    p.config.OwnerID,
			IdleTimeout:                p.config.IdleTimeout,
			Reason:                     CreateReasonAcquire,
			ReadyTimeout:               p.config.AcquireReadyTimeout,
			HealthCheckPollingInterval: p.config.AcquireHealthCheckPollingInterval,
			SkipHealthCheck:            opts.SkipHealthCheck,
			HealthCheck:                p.config.AcquireHealthCheck,
			ConnectionConfig:           p.config.ConnectionConfig,
			CreationSpec:               p.config.CreationSpec,
		}
		sb, err = p.config.SandboxCreator.Create(ctx, createCtx)
	} else {
		sb, err = p.createSandboxFromSpec(ctx, p.config.AcquireReadyTimeout, p.config.AcquireHealthCheckPollingInterval, opts.SkipHealthCheck, p.adaptAcquireHealthCheck())
	}
	if err != nil {
		return nil, err
	}
	sb, err = p.postCreateChecks(ctx, sb, opts)
	if err != nil {
		return nil, err
	}
	// Re-check: a destroy may have landed while this sandbox was being created.
	if err := p.ensureNamespaceActiveAfterCreate(ctx, sb, &policy); err != nil {
		return nil, err
	}
	return sb, nil
}

// postCreateChecks applies renew to a freshly created sandbox.
// Health check is already integrated into CreateSandbox's ready-poll via HealthCheck option.
func (p *DefaultSandboxPool) postCreateChecks(ctx context.Context, sb *Sandbox, opts AcquireOptions) (*Sandbox, error) {
	if opts.SandboxTimeout > 0 {
		if _, err := sb.Renew(ctx, opts.SandboxTimeout); err != nil {
			go p.killSandboxBestEffort(sb.ID())
			_ = sb.Close()
			return nil, fmt.Errorf("opensandbox: pool direct create: renew failed: %w", err)
		}
	}
	return sb, nil
}

func (p *DefaultSandboxPool) createOneSandbox(ctx context.Context, reason PooledSandboxCreateReason) (string, error) {
	var sb *Sandbox
	var err error

	if p.config.SandboxCreator != nil {
		createCtx := PooledSandboxCreateContext{
			PoolName:                   p.config.PoolName,
			OwnerID:                    p.config.OwnerID,
			IdleTimeout:                p.config.IdleTimeout,
			Reason:                     reason,
			ReadyTimeout:               p.config.WarmupReadyTimeout,
			HealthCheckPollingInterval: p.config.WarmupHealthCheckPollingInterval,
			SkipHealthCheck:            p.config.WarmupSkipHealthCheck,
			HealthCheck:                p.config.WarmupHealthCheck,
			ConnectionConfig:           p.config.ConnectionConfig,
			CreationSpec:               p.config.CreationSpec,
		}
		sb, err = p.config.SandboxCreator.Create(ctx, createCtx)
	} else {
		sb, err = p.createSandboxFromSpec(ctx, p.config.WarmupReadyTimeout, p.config.WarmupHealthCheckPollingInterval, p.config.WarmupSkipHealthCheck, p.adaptWarmupHealthCheck())
	}
	if err != nil {
		return "", err
	}
	return p.finalizeWarmup(ctx, sb)
}

// finalizeWarmup runs warmup callbacks and renews the sandbox TTL.
// The sandbox connection is always closed; only the ID is returned.
func (p *DefaultSandboxPool) finalizeWarmup(ctx context.Context, sb *Sandbox) (string, error) {
	defer sb.Close()
	sandboxID := sb.ID()
	if err := p.applyWarmupCallbacks(ctx, sb); err != nil {
		go p.killSandboxBestEffort(sandboxID)
		return "", err
	}
	if _, err := sb.Renew(ctx, p.config.IdleTimeout); err != nil {
		go p.killSandboxBestEffort(sandboxID)
		return "", fmt.Errorf("opensandbox: pool warmup: renew failed: %w", err)
	}
	return sandboxID, nil
}

func (p *DefaultSandboxPool) createSandboxFromSpec(ctx context.Context, readyTimeout time.Duration, healthCheckInterval time.Duration, skipHealthCheck bool, healthCheck func(ctx context.Context, sb *Sandbox) (bool, error)) (*Sandbox, error) {
	spec := p.config.CreationSpec
	timeoutSec := int(p.config.IdleTimeout.Seconds())
	if timeoutSec < 1 {
		timeoutSec = 1
	}
	createOpts := SandboxCreateOptions{
		Image:               spec.Image,
		SnapshotID:          spec.SnapshotID,
		Entrypoint:          spec.Entrypoint,
		ResourceLimits:      spec.ResourceLimits,
		TimeoutSeconds:      &timeoutSec,
		Env:                 spec.Env,
		Metadata:            spec.Metadata,
		NetworkPolicy:       spec.NetworkPolicy,
		Volumes:             spec.Volumes,
		Extensions:          spec.Extensions,
		Platform:            spec.Platform,
		ManualCleanup:       spec.ManualCleanup,
		SecureAccess:        spec.SecureAccess,
		CredentialProxy:     spec.CredentialProxy,
		ImageAuth:           spec.ImageAuth,
		SkipHealthCheck:     skipHealthCheck,
		ReadyTimeout:        readyTimeout,
		HealthCheckInterval: healthCheckInterval,
		HealthCheck:         healthCheck,
	}
	return CreateSandbox(ctx, p.config.ConnectionConfig, createOpts)
}

func (p *DefaultSandboxPool) applyWarmupCallbacks(ctx context.Context, sb *Sandbox) error {
	// WarmupHealthCheck is now integrated into createSandboxFromSpec's ready-poll
	// via the HealthCheck option, so only the preparer callback remains here.
	if p.config.WarmupSandboxPreparer != nil {
		if err := p.config.WarmupSandboxPreparer(ctx, sb); err != nil {
			return err
		}
	}
	return nil
}

// adaptAcquireHealthCheck wraps the user's AcquireHealthCheck (func error)
// into the ReadyOptions.HealthCheck signature (func (bool, error)) so it
// can be retried during the ready-poll loop, matching Python/Kotlin semantics.
func (p *DefaultSandboxPool) adaptAcquireHealthCheck() func(context.Context, *Sandbox) (bool, error) {
	return adaptHealthCheck(p.config.AcquireHealthCheck)
}

// adaptWarmupHealthCheck wraps WarmupHealthCheck the same way.
func (p *DefaultSandboxPool) adaptWarmupHealthCheck() func(context.Context, *Sandbox) (bool, error) {
	return adaptHealthCheck(p.config.WarmupHealthCheck)
}

// adaptHealthCheck wraps a user-provided health check (func error) into the
// ReadyOptions.HealthCheck signature (func (bool, error)) so it can be retried
// during the ready-poll loop, matching Python/Kotlin semantics.
// Errors are propagated so WaitUntilReady records them as lastErr.
func adaptHealthCheck(userCheck func(context.Context, *Sandbox) error) func(context.Context, *Sandbox) (bool, error) {
	if userCheck == nil {
		return nil
	}
	return func(ctx context.Context, sb *Sandbox) (bool, error) {
		if err := userCheck(ctx, sb); err != nil {
			return false, err
		}
		return true, nil
	}
}

// ReleaseAllIdle drains all idle sandboxes and schedules a best-effort kill for each one.
func (p *DefaultSandboxPool) ReleaseAllIdle(ctx context.Context) (int, error) {
	count := 0
	for {
		if err := ctx.Err(); err != nil {
			return count, err
		}
		sandboxID, err := p.config.StateStore.TryTakeIdle(ctx, p.config.PoolName)
		if err != nil {
			return count, err
		}
		if sandboxID == "" {
			break
		}
		go p.killSandboxBestEffort(sandboxID)
		count++
	}
	return count, nil
}

// ReleaseAllIdleParallel drains all idle sandboxes and kills them with bounded
// concurrency. It blocks until every drained sandbox has received a best-effort
// kill attempt. maxWorkers must be positive.
//
// ctx only bounds the drain phase. Once an ID has been drained, its kill attempt
// uses an independent timeout and completes before this method returns, even if
// ctx is cancelled.
func (p *DefaultSandboxPool) ReleaseAllIdleParallel(ctx context.Context, maxWorkers int) (int, error) {
	if maxWorkers <= 0 {
		return 0, fmt.Errorf("opensandbox: pool release all idle parallel: maxWorkers must be positive, got %d", maxWorkers)
	}
	sandboxIDs := make([]string, 0)
	var drainErr error
	for {
		if err := ctx.Err(); err != nil {
			drainErr = err
			break
		}
		sandboxID, err := p.config.StateStore.TryTakeIdle(ctx, p.config.PoolName)
		if err != nil {
			drainErr = err
			break
		}
		if sandboxID == "" {
			break
		}
		sandboxIDs = append(sandboxIDs, sandboxID)
	}

	jobs := make(chan string)
	var workers sync.WaitGroup
	workerCount := len(sandboxIDs)
	if workerCount > maxWorkers {
		workerCount = maxWorkers
	}
	workers.Add(workerCount)
	for i := 0; i < workerCount; i++ {
		go func() {
			defer workers.Done()
			for sandboxID := range jobs {
				if err := p.killSandbox(sandboxID); err != nil {
					p.config.Logger.Warn("failed to kill sandbox (best-effort)",
						"pool_name", p.config.PoolName,
						"sandbox_id", sandboxID,
						"error", err)
				}
			}
		}()
	}
	for _, sandboxID := range sandboxIDs {
		jobs <- sandboxID
	}
	close(jobs)
	workers.Wait()
	return len(sandboxIDs), drainErr
}

// Resize dynamically changes the idle target.
// The new value is persisted to the state store and updated locally so that
// a subsequent Start() (after stop/restart) uses the latest value.
func (p *DefaultSandboxPool) Resize(ctx context.Context, newMaxIdle int) error {
	if newMaxIdle < 0 {
		return fmt.Errorf("opensandbox: pool resize: maxIdle must be >= 0, got %d", newMaxIdle)
	}
	if err := p.config.StateStore.SetMaxIdle(ctx, p.config.PoolName, newMaxIdle); err != nil {
		return err
	}
	p.mu.Lock()
	p.config.MaxIdle = newMaxIdle
	p.mu.Unlock()
	return nil
}

// Snapshot returns a point-in-time snapshot of pool state.
func (p *DefaultSandboxPool) Snapshot(ctx context.Context) (*PoolSnapshot, error) {
	p.mu.Lock()
	ls := p.lifecycleState
	hs := p.healthState
	recon := p.reconciler
	p.mu.Unlock()

	counters, err := p.config.StateStore.SnapshotCounters(ctx, p.config.PoolName)
	if err != nil {
		return nil, err
	}

	maxIdle, err := p.config.StateStore.GetMaxIdle(ctx, p.config.PoolName)
	if err != nil {
		return nil, err
	}

	var failureCount int
	var backoffActive bool
	var lastError string
	if recon != nil {
		_, failureCount, backoffActive, lastError = recon.snapshot()
	}

	return &PoolSnapshot{
		LifecycleState:     ls,
		HealthState:        hs,
		IdleCount:          counters.IdleCount,
		MaxIdle:            maxIdle,
		FailureCount:       failureCount,
		BackoffActive:      backoffActive,
		LastError:          lastError,
		InFlightOperations: int(atomic.LoadInt32(&p.inFlight)),
	}, nil
}

// SnapshotIdleEntries returns the current idle entries.
func (p *DefaultSandboxPool) SnapshotIdleEntries(ctx context.Context) ([]IdleEntry, error) {
	return p.config.StateStore.SnapshotIdleEntries(ctx, p.config.PoolName)
}

// Shutdown stops the pool and releases idle sandboxes.
func (p *DefaultSandboxPool) Shutdown(ctx context.Context, graceful bool) error {
	p.mu.Lock()
	if p.lifecycleState == PoolLifecycleStopped || p.lifecycleState == PoolLifecycleDraining {
		ch := p.shutdownDone
		p.mu.Unlock()
		if ch != nil {
			<-ch
		}
		return nil
	}
	if p.lifecycleState == PoolLifecycleNotStarted || p.lifecycleState == PoolLifecycleStarting {
		p.lifecycleState = PoolLifecycleStopped
		if p.ticker != nil {
			p.ticker.Stop()
		}
		if p.done != nil && !p.doneClosed {
			close(p.done)
			p.doneClosed = true
		}
		cancelFn := p.reconCancel
		sdCh := p.shutdownDone
		p.mu.Unlock()
		if cancelFn != nil {
			cancelFn()
		}
		p.wg.Wait()
		// Close shutdownDone so a concurrent Start() waiting on it unblocks.
		if sdCh != nil {
			select {
			case <-sdCh:
			default:
				close(sdCh)
			}
		}
		return nil
	}

	if !graceful {
		p.lifecycleState = PoolLifecycleStopped
		if p.ticker != nil {
			p.ticker.Stop()
		}
		if p.done != nil && !p.doneClosed {
			close(p.done)
			p.doneClosed = true
		}
		cancelFn := p.reconCancel
		sdCh := p.shutdownDone
		p.mu.Unlock()
		if cancelFn != nil {
			cancelFn()
		}
		p.wg.Wait()
		_ = p.config.StateStore.ReleasePrimaryLock(ctx, p.config.PoolName, p.config.OwnerID)
		p.config.Logger.Info("pool shutdown (non-graceful)",
			"pool_name", p.config.PoolName)
		if sdCh != nil {
			select {
			case <-sdCh:
			default:
				close(sdCh)
			}
		}
		return nil
	}

	// Graceful shutdown.
	p.lifecycleState = PoolLifecycleDraining
	if p.ticker != nil {
		p.ticker.Stop()
	}
	if p.done != nil && !p.doneClosed {
		close(p.done)
		p.doneClosed = true
	}
	cancelFn := p.reconCancel
	p.mu.Unlock()

	if cancelFn != nil {
		cancelFn()
	}
	p.wg.Wait()
	_ = p.config.StateStore.ReleasePrimaryLock(ctx, p.config.PoolName, p.config.OwnerID)

	// Wait for in-flight operations to drain.
	if p.config.DrainTimeout > 0 {
		deadline := time.After(p.config.DrainTimeout)
		pollTicker := time.NewTicker(100 * time.Millisecond)
		defer pollTicker.Stop()
		for atomic.LoadInt32(&p.inFlight) > 0 {
			select {
			case <-deadline:
				p.config.Logger.Warn("pool shutdown: drain timeout expired with in-flight operations",
					"pool_name", p.config.PoolName,
					"in_flight", atomic.LoadInt32(&p.inFlight))
				goto done
			case <-pollTicker.C:
			}
		}
	}

done:
	p.mu.Lock()
	p.lifecycleState = PoolLifecycleStopped
	sdCh := p.shutdownDone
	p.mu.Unlock()
	p.config.Logger.Info("pool shutdown (graceful)",
		"pool_name", p.config.PoolName)
	if sdCh != nil {
		select {
		case <-sdCh:
		default:
			close(sdCh)
		}
	}
	return nil
}

// ensureNamespaceActive returns *PoolDestroyedError when a destroy has fenced or
// tombstoned this pool's namespace, and *PoolStateStoreUnavailableError when the
// state store cannot answer.
func (p *DefaultSandboxPool) ensureNamespaceActive(ctx context.Context) error {
	state, err := p.config.StateStore.GetDestroyState(ctx, p.config.PoolName)
	if err != nil {
		var unavailable *PoolStateStoreUnavailableError
		if errors.As(err, &unavailable) {
			return err
		}
		return &PoolStateStoreUnavailableError{Operation: "GetDestroyState", Cause: err}
	}
	if state != PoolDestroyStateActive {
		return &PoolDestroyedError{PoolName: p.config.PoolName, State: state}
	}
	return nil
}

// ensureNamespaceActiveForAcquire is ensureNamespaceActive with the same
// store-outage degradation the take path already applies: policies that fall
// through to direct create treat an unreachable store as "state unknown" and
// proceed, so a full store outage does not make them less available than
// documented (OSEP-0005 error-code matrix). Fail-closed policies surface it.
func (p *DefaultSandboxPool) ensureNamespaceActiveForAcquire(ctx context.Context, policy AcquirePolicy) error {
	err := p.ensureNamespaceActive(ctx)
	if err == nil {
		return nil
	}
	var unavailable *PoolStateStoreUnavailableError
	if errors.As(err, &unavailable) && policyFallsThroughToDirectCreate(policy) {
		p.config.Logger.Warn("acquire: state store unavailable during namespace check, "+
			"assuming ACTIVE and degrading to direct create",
			"pool_name", p.config.PoolName,
			"policy", policy,
			"error", err)
		return nil
	}
	return err
}

// ensureNamespaceActiveAfterCreate re-checks the fence once the acquire path holds
// a live sandbox, so a destroy that landed mid-acquire does not leak one into a
// retired namespace. On a fence the sandbox is killed and closed.
//
// policy is non-nil only for the direct-create path, where a store outage degrades
// the same way the rest of that path does. The idle path passes nil and stays
// fail-closed: that sandbox is already out of the store, so an unconfirmed
// namespace has to be treated as retired.
func (p *DefaultSandboxPool) ensureNamespaceActiveAfterCreate(ctx context.Context, sb *Sandbox, policy *AcquirePolicy) error {
	err := p.ensureNamespaceActive(ctx)
	if err == nil {
		return nil
	}
	var unavailable *PoolStateStoreUnavailableError
	if errors.As(err, &unavailable) && policy != nil && policyFallsThroughToDirectCreate(*policy) {
		p.config.Logger.Warn("acquire: state store unavailable during post-create namespace check, "+
			"keeping sandbox and degrading per policy",
			"pool_name", p.config.PoolName,
			"sandbox_id", sb.ID(),
			"policy", *policy,
			"error", err)
		return nil
	}
	go p.killSandboxBestEffort(sb.ID())
	_ = sb.Close()
	return err
}

// stopAfterNamespaceDestroyed stops the pool once its namespace has been retired.
// It runs on the reconcile goroutine, so unlike Shutdown it must not wait on p.wg.
func (p *DefaultSandboxPool) stopAfterNamespaceDestroyed(state PoolDestroyState) {
	p.mu.Lock()
	if p.lifecycleState == PoolLifecycleStopped || p.lifecycleState == PoolLifecycleDraining {
		p.mu.Unlock()
		return
	}
	p.lifecycleState = PoolLifecycleStopped
	if p.ticker != nil {
		p.ticker.Stop()
	}
	if p.done != nil && !p.doneClosed {
		close(p.done)
		p.doneClosed = true
	}
	cancelFn := p.reconCancel
	sdCh := p.shutdownDone
	p.mu.Unlock()

	if cancelFn != nil {
		cancelFn()
	}
	if sdCh != nil {
		select {
		case <-sdCh:
		default:
			close(sdCh)
		}
	}
	p.config.Logger.Info("pool stopped: namespace destroyed",
		"pool_name", p.config.PoolName,
		"destroy_state", state)
}

const (
	killSandboxTimeout = 30 * time.Second
)

func (p *DefaultSandboxPool) killSandboxBestEffort(sandboxID string) {
	_ = p.killSandbox(sandboxID)
}

func (p *DefaultSandboxPool) killSandbox(sandboxID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), killSandboxTimeout)
	defer cancel()
	return p.manager.KillSandbox(ctx, sandboxID)
}

func (p *DefaultSandboxPool) killDiscardedAliveSandboxes(ids []string) {
	for _, id := range ids {
		p.killSandboxBestEffort(id)
	}
}
