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
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// SandboxPoolManager performs namespace-level maintenance on a shared sandbox
// pool. It does not acquire sandboxes: it exists so an operator can retire a
// pool namespace without reconstructing the SandboxPool object that originally
// owned it.
type SandboxPoolManager struct {
	stateStore PoolStateStore
	manager    *SandboxManager
	ownerID    string
	logger     PoolLogger
}

// Destroy retires a pool namespace.
//
// FORCE destroy writes a shared DESTROYING fence first, which every peer sharing
// the state store observes: fenced pools cannot hold the primary lock or publish
// idle sandboxes, so replenishment stops instead of racing the destroy. The
// manager then drains the visible idle IDs and kills them best-effort, clears the
// persistent coordination state, and writes a DESTROYED tombstone so later
// callers cannot silently rebind the namespace.
//
// If the drain or the cleanup cannot finish, the namespace stays DESTROYING and
// the error is a *PoolDestroyIncompleteError; retrying Destroy is safe. Calling
// Destroy on an already-tombstoned namespace is a no-op that reports DESTROYED.
func (m *SandboxPoolManager) Destroy(ctx context.Context, poolName string, options PoolDestroyOptions) (*PoolDestroyResult, error) {
	if strings.TrimSpace(poolName) == "" {
		return nil, fmt.Errorf("opensandbox: pool manager: poolName must not be blank")
	}
	if options.Strategy != PoolDestroyForce {
		return nil, fmt.Errorf("opensandbox: pool manager: only FORCE destroy strategy is supported, got %s", options.Strategy)
	}

	drainTimeout := DefaultPoolDrainTimeout
	if options.DrainTimeout != nil {
		drainTimeout = *options.DrainTimeout
		if drainTimeout < 0 {
			return nil, fmt.Errorf("opensandbox: pool manager: DrainTimeout must not be negative, got %v", drainTimeout)
		}
	}
	tombstoneTTL := DefaultPoolTombstoneTTL
	if options.TombstoneTTL != nil {
		tombstoneTTL = *options.TombstoneTTL
		if tombstoneTTL < 0 {
			return nil, fmt.Errorf("opensandbox: pool manager: TombstoneTTL must not be negative, got %v", tombstoneTTL)
		}
	}

	state, err := m.stateStore.GetDestroyState(ctx, poolName)
	if err != nil {
		return nil, err
	}
	if state == PoolDestroyStateDestroyed {
		return alreadyDestroyedResult(poolName), nil
	}

	if err := m.stateStore.BeginDestroy(ctx, poolName, m.ownerID); err != nil {
		// Lost the race to a concurrent destroy that already tombstoned the
		// namespace; the outcome the caller asked for already holds.
		var destroyed *PoolDestroyedError
		if errors.As(err, &destroyed) {
			return alreadyDestroyedResult(poolName), nil
		}
		return nil, err
	}

	drained := 0
	killed := 0
	deadline := time.Now().Add(drainTimeout)
	for {
		sandboxID, err := m.stateStore.TryTakeIdle(ctx, poolName)
		if err != nil {
			return nil, &PoolDestroyIncompleteError{
				PoolName: poolName,
				Reason:   fmt.Sprintf("failed to drain idle sandboxes after %d drained", drained),
				Cause:    err,
			}
		}
		if sandboxID == "" {
			break
		}
		drained++
		if err := m.manager.KillSandbox(ctx, sandboxID); err != nil {
			m.logger.Warn("pool destroy failed to kill idle sandbox (best-effort)",
				"pool_name", poolName,
				"sandbox_id", sandboxID,
				"error", err)
		} else {
			killed++
		}
		if drainTimeout > 0 && time.Now().After(deadline) {
			return nil, &PoolDestroyIncompleteError{
				PoolName: poolName,
				Reason:   fmt.Sprintf("drain timed out after %v with %d idle sandboxes drained", drainTimeout, drained),
			}
		}
	}

	if err := m.stateStore.ClearPoolState(ctx, poolName); err != nil {
		return nil, &PoolDestroyIncompleteError{
			PoolName: poolName,
			Reason:   "failed to clear persistent state",
			Cause:    err,
		}
	}
	if err := m.stateStore.MarkDestroyed(ctx, poolName, m.ownerID, tombstoneTTL); err != nil {
		return nil, &PoolDestroyIncompleteError{
			PoolName: poolName,
			Reason:   "failed to write destroyed tombstone",
			Cause:    err,
		}
	}

	m.logger.Info("pool namespace destroyed",
		"pool_name", poolName,
		"drained_idle_count", drained,
		"killed_idle_count", killed)

	return &PoolDestroyResult{
		PoolName:               poolName,
		State:                  PoolDestroyStateDestroyed,
		DrainedIdleCount:       drained,
		KilledIdleCount:        killed,
		PersistentStateCleared: true,
	}, nil
}

func alreadyDestroyedResult(poolName string) *PoolDestroyResult {
	return &PoolDestroyResult{
		PoolName:               poolName,
		State:                  PoolDestroyStateDestroyed,
		DrainedIdleCount:       0,
		KilledIdleCount:        0,
		PersistentStateCleared: false,
	}
}

// SandboxPoolManagerBuilder configures and creates a SandboxPoolManager.
type SandboxPoolManagerBuilder struct {
	stateStore          PoolStateStore
	connectionConfig    ConnectionConfig
	connectionConfigSet bool
	ownerID             string
	logger              PoolLogger
}

// NewSandboxPoolManagerBuilder creates a new builder.
func NewSandboxPoolManagerBuilder() *SandboxPoolManagerBuilder {
	return &SandboxPoolManagerBuilder{}
}

// StateStore sets the pool state store to operate on (required). It must be the
// same store the pool being retired coordinates through.
func (b *SandboxPoolManagerBuilder) StateStore(s PoolStateStore) *SandboxPoolManagerBuilder {
	b.stateStore = s
	return b
}

// ConnectionConfig sets the connection configuration used to kill drained
// sandboxes (required).
func (b *SandboxPoolManagerBuilder) ConnectionConfig(c ConnectionConfig) *SandboxPoolManagerBuilder {
	b.connectionConfig = c
	b.connectionConfigSet = true
	return b
}

// OwnerID sets the identifier recorded alongside the fence and the tombstone.
// Defaults to a generated per-process value.
func (b *SandboxPoolManagerBuilder) OwnerID(id string) *SandboxPoolManagerBuilder {
	b.ownerID = id
	return b
}

// PoolLogger sets a custom structured logger. Defaults to a no-op logger.
func (b *SandboxPoolManagerBuilder) PoolLogger(l PoolLogger) *SandboxPoolManagerBuilder {
	b.logger = l
	return b
}

// Build validates configuration and creates a SandboxPoolManager.
func (b *SandboxPoolManagerBuilder) Build() (*SandboxPoolManager, error) {
	if b.stateStore == nil {
		return nil, fmt.Errorf("opensandbox: pool manager builder: StateStore is required")
	}
	if !b.connectionConfigSet {
		return nil, fmt.Errorf("opensandbox: pool manager builder: ConnectionConfig is required")
	}

	ownerID := b.ownerID
	if ownerID == "" {
		generated, err := generatePoolManagerOwnerID()
		if err != nil {
			return nil, err
		}
		ownerID = generated
	}
	if strings.TrimSpace(ownerID) == "" {
		return nil, fmt.Errorf("opensandbox: pool manager builder: OwnerID must not be blank")
	}

	logger := b.logger
	if logger == nil {
		logger = noopPoolLogger{}
	}

	return &SandboxPoolManager{
		stateStore: b.stateStore,
		manager:    NewSandboxManager(b.connectionConfig),
		ownerID:    ownerID,
		logger:     logger,
	}, nil
}

func generatePoolManagerOwnerID() (string, error) {
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		hostname = "unknown"
	}
	var randBytes [4]byte
	if _, randErr := crypto_rand.Read(randBytes[:]); randErr != nil {
		return "", fmt.Errorf("opensandbox: pool manager builder: failed to generate random owner ID: %w", randErr)
	}
	return fmt.Sprintf("pool-manager-%s-%d-%d-%x", hostname, os.Getpid(), time.Now().UnixNano(), randBytes), nil
}
