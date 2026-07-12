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
	crypto_rand "crypto/rand"
	"fmt"
	"math"
	"os"
	"time"
)

// SandboxPoolBuilder configures and creates a DefaultSandboxPool.
type SandboxPoolBuilder struct {
	config              PoolConfig
	stateStoreSet       bool
	connectionConfigSet bool
}

// NewSandboxPoolBuilder creates a new builder with sensible defaults.
func NewSandboxPoolBuilder() *SandboxPoolBuilder {
	return &SandboxPoolBuilder{
		config: PoolConfig{
			PrimaryLockTTL:                    60 * time.Second,
			ReconcileInterval:                 30 * time.Second,
			DegradedThreshold:                 3,
			AcquireReadyTimeout:               30 * time.Second,
			WarmupReadyTimeout:                30 * time.Second,
			AcquireHealthCheckPollingInterval: 200 * time.Millisecond,
			WarmupHealthCheckPollingInterval:  200 * time.Millisecond,
			EmptyBehavior:                     AcquirePolicyDirectCreate,
			DrainTimeout:                      30 * time.Second,
		},
	}
}

// PoolName sets the pool name (required).
func (b *SandboxPoolBuilder) PoolName(name string) *SandboxPoolBuilder {
	b.config.PoolName = name
	return b
}

// OwnerID sets the owner identifier for leader election.
func (b *SandboxPoolBuilder) OwnerID(id string) *SandboxPoolBuilder {
	b.config.OwnerID = id
	return b
}

// MaxIdle sets the target number of idle sandboxes.
func (b *SandboxPoolBuilder) MaxIdle(n int) *SandboxPoolBuilder {
	b.config.MaxIdle = n
	return b
}

// StateStore sets a custom PoolStateStore implementation.
func (b *SandboxPoolBuilder) StateStore(s PoolStateStore) *SandboxPoolBuilder {
	b.config.StateStore = s
	b.stateStoreSet = true
	return b
}

// ConnectionConfig sets the connection configuration (required).
func (b *SandboxPoolBuilder) ConnectionConfig(c ConnectionConfig) *SandboxPoolBuilder {
	b.config.ConnectionConfig = c
	b.connectionConfigSet = true
	return b
}

// CreationSpec sets the sandbox creation parameters.
func (b *SandboxPoolBuilder) CreationSpec(s PoolCreationSpec) *SandboxPoolBuilder {
	b.config.CreationSpec = s
	return b
}

// WarmupConcurrency sets the maximum number of sandboxes created per reconcile tick.
func (b *SandboxPoolBuilder) WarmupConcurrency(n int) *SandboxPoolBuilder {
	b.config.WarmupConcurrency = n
	return b
}

// ReconcileInterval sets the interval between reconciliation ticks.
func (b *SandboxPoolBuilder) ReconcileInterval(d time.Duration) *SandboxPoolBuilder {
	b.config.ReconcileInterval = d
	return b
}

// PrimaryLockTTL sets the TTL for the primary (leader) lock.
func (b *SandboxPoolBuilder) PrimaryLockTTL(d time.Duration) *SandboxPoolBuilder {
	b.config.PrimaryLockTTL = d
	return b
}

// DegradedThreshold sets the number of consecutive failures before the pool
// is considered degraded.
func (b *SandboxPoolBuilder) DegradedThreshold(n int) *SandboxPoolBuilder {
	b.config.DegradedThreshold = n
	return b
}

// AcquireReadyTimeout sets the timeout for health checks during Acquire.
func (b *SandboxPoolBuilder) AcquireReadyTimeout(d time.Duration) *SandboxPoolBuilder {
	b.config.AcquireReadyTimeout = d
	return b
}

// WarmupReadyTimeout sets the timeout for health checks during warm-up creation.
func (b *SandboxPoolBuilder) WarmupReadyTimeout(d time.Duration) *SandboxPoolBuilder {
	b.config.WarmupReadyTimeout = d
	return b
}

// AcquireHealthCheckPollingInterval sets the interval between health check polls
// during acquire (default: 200ms).
func (b *SandboxPoolBuilder) AcquireHealthCheckPollingInterval(d time.Duration) *SandboxPoolBuilder {
	b.config.AcquireHealthCheckPollingInterval = d
	return b
}

// WarmupHealthCheckPollingInterval sets the interval between health check polls
// during warmup (default: 200ms).
func (b *SandboxPoolBuilder) WarmupHealthCheckPollingInterval(d time.Duration) *SandboxPoolBuilder {
	b.config.WarmupHealthCheckPollingInterval = d
	return b
}

// EmptyBehavior sets the default policy when the pool is empty during Acquire.
func (b *SandboxPoolBuilder) EmptyBehavior(p AcquirePolicy) *SandboxPoolBuilder {
	b.config.EmptyBehavior = p
	return b
}

// AcquireHealthCheck sets an optional health-check callback during Acquire.
func (b *SandboxPoolBuilder) AcquireHealthCheck(fn func(ctx context.Context, sb *Sandbox) error) *SandboxPoolBuilder {
	b.config.AcquireHealthCheck = fn
	return b
}

// WarmupHealthCheck sets an optional health-check callback after warmup creation.
func (b *SandboxPoolBuilder) WarmupHealthCheck(fn func(ctx context.Context, sb *Sandbox) error) *SandboxPoolBuilder {
	b.config.WarmupHealthCheck = fn
	return b
}

// WarmupSandboxPreparer sets an optional callback to prepare sandboxes after warmup.
func (b *SandboxPoolBuilder) WarmupSandboxPreparer(fn func(ctx context.Context, sb *Sandbox) error) *SandboxPoolBuilder {
	b.config.WarmupSandboxPreparer = fn
	return b
}

// SandboxCreator sets a custom sandbox creator. If nil, the default CreateSandbox is used.
func (b *SandboxPoolBuilder) SandboxCreator(creator PooledSandboxCreator) *SandboxPoolBuilder {
	b.config.SandboxCreator = creator
	return b
}

// WarmupSkipHealthCheck configures whether to skip health check during warmup.
func (b *SandboxPoolBuilder) WarmupSkipHealthCheck(skip bool) *SandboxPoolBuilder {
	b.config.WarmupSkipHealthCheck = skip
	return b
}

// AcquireMinRemainingTTL sets the default minimum remaining TTL for acquire.
func (b *SandboxPoolBuilder) AcquireMinRemainingTTL(d time.Duration) *SandboxPoolBuilder {
	b.config.AcquireMinRemainingTTL = d
	return b
}

// IdleTimeout sets the TTL applied to pool-created sandboxes (default: 24h).
func (b *SandboxPoolBuilder) IdleTimeout(d time.Duration) *SandboxPoolBuilder {
	b.config.IdleTimeout = d
	return b
}

// DrainTimeout sets the maximum time to wait during graceful shutdown.
func (b *SandboxPoolBuilder) DrainTimeout(d time.Duration) *SandboxPoolBuilder {
	b.config.DrainTimeout = d
	return b
}

// PoolLogger sets a custom structured logger for pool operations.
// Defaults to a no-op logger if not set.
func (b *SandboxPoolBuilder) PoolLogger(l PoolLogger) *SandboxPoolBuilder {
	b.config.Logger = l
	return b
}

// Build validates configuration and creates a DefaultSandboxPool.
// The pool is not started; call Start() to begin reconciliation.
func (b *SandboxPoolBuilder) Build() (*DefaultSandboxPool, error) {
	// Validate user-provided values before applying defaults.
	if b.config.PoolName == "" {
		return nil, fmt.Errorf("opensandbox: pool builder: PoolName is required")
	}
	if b.config.MaxIdle < 0 {
		return nil, fmt.Errorf("opensandbox: pool builder: MaxIdle must be >= 0, got %d", b.config.MaxIdle)
	}
	if !b.connectionConfigSet {
		return nil, fmt.Errorf("opensandbox: pool builder: ConnectionConfig is required")
	}
	if b.config.ReconcileInterval <= 0 {
		return nil, fmt.Errorf("opensandbox: pool builder: ReconcileInterval must be positive")
	}
	if b.config.PrimaryLockTTL <= 0 {
		return nil, fmt.Errorf("opensandbox: pool builder: PrimaryLockTTL must be positive")
	}
	if b.config.DegradedThreshold <= 0 {
		return nil, fmt.Errorf("opensandbox: pool builder: DegradedThreshold must be positive")
	}
	if b.config.SandboxCreator == nil && b.config.CreationSpec.Image == "" && b.config.CreationSpec.SnapshotID == "" {
		return nil, fmt.Errorf("opensandbox: pool builder: CreationSpec (with Image or SnapshotID) is required when no SandboxCreator is set")
	}
	if b.config.SandboxCreator == nil && b.config.CreationSpec.Image != "" && b.config.CreationSpec.SnapshotID != "" {
		return nil, fmt.Errorf("opensandbox: pool builder: CreationSpec cannot have both Image and SnapshotID set")
	}
	if b.config.CreationSpec.TimeoutSeconds != nil {
		return nil, fmt.Errorf("opensandbox: pool builder: CreationSpec.TimeoutSeconds is not supported; use IdleTimeout to control sandbox TTL in pools")
	}
	if b.config.CreationSpec.ManualCleanup {
		return nil, fmt.Errorf("opensandbox: pool builder: ManualCleanup is not supported for pooled sandboxes (would leak resources after idle TTL expiry)")
	}
	if b.config.AcquireReadyTimeout <= 0 {
		return nil, fmt.Errorf("opensandbox: pool builder: AcquireReadyTimeout must be positive")
	}
	if b.config.WarmupReadyTimeout <= 0 {
		return nil, fmt.Errorf("opensandbox: pool builder: WarmupReadyTimeout must be positive")
	}
	if b.config.AcquireHealthCheckPollingInterval < 0 {
		return nil, fmt.Errorf("opensandbox: pool builder: AcquireHealthCheckPollingInterval must be non-negative")
	}
	if b.config.WarmupHealthCheckPollingInterval < 0 {
		return nil, fmt.Errorf("opensandbox: pool builder: WarmupHealthCheckPollingInterval must be non-negative")
	}
	if b.config.AcquireMinRemainingTTL < 0 {
		return nil, fmt.Errorf("opensandbox: pool builder: AcquireMinRemainingTTL must be >= 0, got %v", b.config.AcquireMinRemainingTTL)
	}
	if b.config.DrainTimeout < 0 {
		return nil, fmt.Errorf("opensandbox: pool builder: DrainTimeout must be non-negative, got %v", b.config.DrainTimeout)
	}
	if b.config.WarmupConcurrency < 0 {
		return nil, fmt.Errorf("opensandbox: pool builder: WarmupConcurrency must be non-negative, got %d", b.config.WarmupConcurrency)
	}

	// Work on a local copy so Build() doesn't mutate the builder.
	cfg := b.config

	// Apply defaults on the copy.
	if !b.stateStoreSet {
		cfg.StateStore = NewInMemoryPoolStateStore()
	}
	if cfg.StateStore == nil {
		return nil, fmt.Errorf("opensandbox: pool builder: StateStore must not be nil")
	}
	if cfg.WarmupConcurrency == 0 {
		computed := int(math.Ceil(float64(cfg.MaxIdle) * 0.2))
		if computed < 1 {
			computed = 1
		}
		cfg.WarmupConcurrency = computed
	}
	if cfg.OwnerID == "" {
		hostname, err := os.Hostname()
		if err != nil || hostname == "" {
			hostname = "unknown"
		}
		var randBytes [4]byte
		if _, randErr := crypto_rand.Read(randBytes[:]); randErr != nil {
			return nil, fmt.Errorf("opensandbox: pool builder: failed to generate random owner ID: %w", randErr)
		}
		cfg.OwnerID = fmt.Sprintf("pool-owner-%s-%d-%d-%x", hostname, os.Getpid(), time.Now().UnixNano(), randBytes)
	}
	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = DefaultIdleTimeout
	}
	if cfg.IdleTimeout < 0 {
		return nil, fmt.Errorf("opensandbox: pool builder: IdleTimeout must be positive, got %v", cfg.IdleTimeout)
	}
	if cfg.Logger == nil {
		cfg.Logger = noopPoolLogger{}
	}
	if cfg.AcquireMinRemainingTTL > 0 && cfg.AcquireMinRemainingTTL >= cfg.IdleTimeout {
		return nil, fmt.Errorf("opensandbox: pool builder: AcquireMinRemainingTTL (%v) must be less than IdleTimeout (%v)", cfg.AcquireMinRemainingTTL, cfg.IdleTimeout)
	}
	if cfg.AcquireMinRemainingTTL == 0 {
		cfg.AcquireMinRemainingTTL = cfg.IdleTimeout / 2
		ttlCap := 60 * time.Second
		if ttlCap < cfg.AcquireMinRemainingTTL {
			cfg.AcquireMinRemainingTTL = ttlCap
		}
	}

	return &DefaultSandboxPool{
		config:         &cfg,
		manager:        NewSandboxManager(cfg.ConnectionConfig),
		lifecycleState: PoolLifecycleNotStarted,
		healthState:    PoolHealthy,
	}, nil
}
