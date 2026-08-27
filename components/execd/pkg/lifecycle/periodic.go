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

package lifecycle

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/robfig/cron/v3"

	"github.com/alibaba/opensandbox/execd/pkg/log"
)

type PeriodicManager struct {
	cron     *cron.Cron
	ctx      context.Context
	cancel   context.CancelFunc
	running  map[string]*atomic.Bool
	entryIDs map[string]cron.EntryID
}

func StartPeriodic(cfg *Config) (*PeriodicManager, error) {
	if cfg == nil || len(cfg.Periodic) == 0 {
		return nil, nil //nolint:nilnil // no manager is needed when periodic hooks are not configured
	}

	ctx, cancel := context.WithCancel(context.Background())
	manager := &PeriodicManager{
		cron:     cron.New(cron.WithChain(cron.Recover(cron.DefaultLogger))),
		ctx:      ctx,
		cancel:   cancel,
		running:  make(map[string]*atomic.Bool, len(cfg.Periodic)),
		entryIDs: make(map[string]cron.EntryID, len(cfg.Periodic)),
	}
	for _, configured := range cfg.Periodic {
		hook := configured
		manager.running[hook.Name] = &atomic.Bool{}
		entryID, err := manager.cron.AddFunc(hook.Schedule, func() { manager.run(hook) })
		if err != nil {
			cancel()
			return nil, fmt.Errorf("parse periodic hook %q schedule: %w", hook.Name, err)
		}
		manager.entryIDs[hook.Name] = entryID
	}
	manager.cron.Start()
	return manager, nil
}

func (m *PeriodicManager) run(periodic PeriodicHook) {
	running := m.running[periodic.Name]
	if !running.CompareAndSwap(false, true) {
		log.Warn("lifecycle: periodic hook %q skipped because its previous run is still active", periodic.Name)
		return
	}
	releaseRunning := true
	defer func() {
		if releaseRunning {
			running.Store(false)
		}
	}()

	log.Info("lifecycle: periodic hook %q started", periodic.Name)
	hook := periodic.hook()
	result := RunHook(m.ctx, hook)
	if result.Incomplete {
		releaseRunning = false
		m.cron.Remove(m.entryIDs[periodic.Name])
		log.Error("lifecycle: periodic hook %q did not exit after cancellation; future runs disabled", periodic.Name)
		return
	}
	if result.TimedOut {
		log.Warn("lifecycle: periodic hook %q timed out after %s", periodic.Name, hook.timeout())
		return
	}
	if result.Err != nil && m.ctx.Err() != nil {
		log.Info("lifecycle: periodic hook %q canceled during shutdown", periodic.Name)
		return
	}
	if result.Err != nil {
		log.Warn("lifecycle: periodic hook %q failed exit_code=%d duration=%s: %v", periodic.Name, result.ExitCode, result.Duration, result.Err)
		return
	}
	log.Info("lifecycle: periodic hook %q completed exit_code=0 duration=%s", periodic.Name, result.Duration)
}

func (m *PeriodicManager) Stop() {
	if m == nil {
		return
	}
	m.cancel()
	ctx := m.cron.Stop()
	<-ctx.Done()
}
