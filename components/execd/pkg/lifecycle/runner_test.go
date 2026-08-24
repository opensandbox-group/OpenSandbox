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
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/execd/pkg/isolation"
	"github.com/alibaba/opensandbox/execd/pkg/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	log.Init(6)
	os.Exit(m.Run())
}

func TestRunPreStartExecutesConfiguredCommand(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "pre-started")
	t.Setenv(ConfigPathEnv, filepath.Join(t.TempDir(), "lifecycle.toml"))
	t.Setenv(ConfigEnv, `{"preStart":{"command":["sh","-c","touch `+marker+`"]}}`)

	cfg, err := LoadConfig()
	require.NoError(t, err)
	require.NoError(t, RunPreStart(context.Background(), cfg))
	require.FileExists(t, marker)
}

func TestRunHookReturnsExitCode(t *testing.T) {
	result := RunHook(context.Background(), Hook{Command: []string{"sh", "-c", "exit 17"}})

	require.Error(t, result.Err)
	assert.Equal(t, 17, result.ExitCode)
	assert.False(t, result.TimedOut)
}

func TestRunHookStripsExecdConfigurationEnvironment(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "hook-env")
	t.Setenv("LIFECYCLE_HOOK_ENV_HELPER", "1")
	t.Setenv("LIFECYCLE_HOOK_ENV_MARKER", marker)
	for _, name := range isolation.ExecdConfigEnvBlacklist() {
		t.Setenv(name, "secret")
	}

	result := RunHook(context.Background(), Hook{
		Command: []string{os.Args[0], "-test.run=TestLifecycleHookEnvironmentHelper"},
	})

	require.NoError(t, result.Err)
	raw, err := os.ReadFile(marker)
	require.NoError(t, err)
	assert.Empty(t, string(raw))
}

func TestLifecycleHookEnvironmentHelper(t *testing.T) {
	if os.Getenv("LIFECYCLE_HOOK_ENV_HELPER") != "1" {
		return
	}
	marker := os.Getenv("LIFECYCLE_HOOK_ENV_MARKER")
	require.NotEmpty(t, marker)

	leaked := make([]string, 0)
	for _, name := range isolation.ExecdConfigEnvBlacklist() {
		if _, ok := os.LookupEnv(name); ok {
			leaked = append(leaked, name)
		}
	}
	require.NoError(t, os.WriteFile(marker, []byte(strings.Join(leaked, "\n")), 0o600))
}

func TestRunHookKillsTimedOutProcess(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "descendant-survived")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	result := RunHook(ctx, Hook{Command: []string{"sh", "-c", `(sleep 0.2; touch "$1") & wait`, "_", marker}})

	require.Error(t, result.Err)
	assert.True(t, result.TimedOut)
	assert.Less(t, result.Duration, time.Second)
	time.Sleep(300 * time.Millisecond)
	_, err := os.Stat(marker)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestPeriodicManagerSkipsOverlappingRun(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "runs")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager := &PeriodicManager{
		ctx:     ctx,
		cancel:  cancel,
		running: map[string]*atomic.Bool{"sync": {}},
	}
	hook := PeriodicHook{
		Name:     "sync",
		Schedule: "@every 1m",
		Command:  []string{"sh", "-c", "echo start >> " + marker + "; sleep 0.2; echo end >> " + marker},
	}

	done := make(chan struct{})
	go func() {
		manager.run(hook)
		close(done)
	}()
	require.Eventually(t, func() bool { return manager.running["sync"].Load() }, time.Second, 10*time.Millisecond)
	manager.run(hook)
	<-done

	raw, err := os.ReadFile(marker)
	require.NoError(t, err)
	assert.Equal(t, "start\nend\n", string(raw))
}

func TestStartPeriodicRunsConfiguredSchedule(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "scheduled")
	t.Setenv(ConfigPathEnv, filepath.Join(t.TempDir(), "lifecycle.toml"))
	t.Setenv(ConfigEnv, `{"periodic":[{"name":"sync","schedule":"@every 1s","command":["sh","-c","touch `+marker+`"]}]}`)

	cfg, err := LoadConfig()
	require.NoError(t, err)
	manager, err := StartPeriodic(cfg)
	require.NoError(t, err)
	require.NotNil(t, manager)
	defer manager.Stop()

	require.Eventually(t, func() bool {
		_, err := os.Stat(marker)
		return err == nil
	}, 2500*time.Millisecond, 50*time.Millisecond)
}
