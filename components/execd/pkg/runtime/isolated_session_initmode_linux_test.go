//go:build linux && bwrap

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

package runtime

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/execd/pkg/isolation"
)

// Integration test: isolated sessions (bwrap) under init-mode reaper dispatch
// (OSEP-0018 R-o). bwrap launches through launchManaged with
// withoutHardening and a pre-reap barrier that runs inside the reaper's drain
// (WNOWAIT observe -> consume). Covers the real bwrap lifecycle including
// delete racing a running workload.
func TestIsolatedSessionWithInitReaper(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("bwrap init-mode integration requires root")
	}
	startReaperForTest(t)

	ctrl := NewController("", "")
	cfg := isolation.Config{
		UpperRoot:       t.TempDir(),
		UpperMaxBytes:   1 << 30,
		AllowedWritable: []string{"/tmp"},
	}
	iso := isolation.NewBwrap(cfg)
	if !iso.Available() {
		t.Skip("bwrap isolator unavailable")
	}
	runner, err := NewIsolatedRunner(ctrl, iso, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := runner.Close(); err != nil {
			t.Errorf("close isolated runner: %v", err)
		}
	})

	opts := &IsolatedSessionOptions{
		Profile:       string(isolation.ProfileStrict),
		WorkspacePath: t.TempDir(),
		WorkspaceMode: string(isolation.WorkspaceRW),
	}
	id, err := runner.CreateIsolatedSession(opts)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Runs execute while the reaper owns wait4.
	var lines []string
	if err := runner.RunInIsolatedSession(ctx, id, "echo init_reaper_ok", nil, func(line string) {
		lines = append(lines, line)
	}); err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || lines[0] != "init_reaper_ok" {
		t.Fatalf("run output = %v, want [init_reaper_ok]", lines)
	}

	// Exit codes still propagate through the reaper-delivered status.
	if err := runner.RunInIsolatedSession(ctx, id, "bash -c 'exit 13'", nil, nil); err == nil ||
		!strings.Contains(err.Error(), "13") {
		t.Fatalf("exit-code run error = %v, want exit 13", err)
	}

	// Delete racing a running workload: stop signals the process group while
	// the reaper holds the pid (WNOWAIT observe -> pre-reap barrier -> consume),
	// so the PGID-reuse protection must serialize with reaper dispatch.
	runCtx, runCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer runCancel()
	runErrCh := make(chan error, 1)
	started := make(chan struct{}, 1)
	go func() {
		runErrCh <- runner.RunInIsolatedSession(
			runCtx,
			id,
			"echo run_started; sleep 30",
			nil,
			func(line string) {
				if strings.Contains(line, "run_started") {
					select {
					case started <- struct{}{}:
					default:
					}
				}
			},
		)
	}()
	select {
	case <-started:
	case err := <-runErrCh:
		t.Fatalf("run ended before start: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("run did not start")
	}

	begin := time.Now()
	if err := runner.DeleteIsolatedSession(id); err != nil {
		t.Fatalf("delete during run: %v", err)
	}
	if elapsed := time.Since(begin); elapsed > 10*time.Second {
		t.Fatalf("delete during run took %v", elapsed)
	}
	select {
	case err := <-runErrCh:
		if err == nil {
			t.Fatal("in-flight run unexpectedly succeeded after session deletion")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("in-flight run did not terminate after session deletion")
	}

	if _, err := runner.GetIsolatedSession(id); err == nil {
		t.Fatal("deleted session still present")
	}
}
