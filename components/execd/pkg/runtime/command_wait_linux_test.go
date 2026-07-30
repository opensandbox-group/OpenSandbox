//go:build linux

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
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestWaitCommandExitBarrierRunsBeforeProcessReap(t *testing.T) {
	command := exec.Command("/bin/sh", "-c", "exit 0")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	pid := command.Process.Pid

	barrierEntered := make(chan struct{})
	releaseBarrier := make(chan struct{})
	waitDone := make(chan error, 1)
	go func() {
		waitDone <- waitCommandWithExitBarrier(
			command,
			func(barrierErr error) {
				if barrierErr != nil {
					t.Errorf("exit barrier: %v", barrierErr)
				}
				close(barrierEntered)
				<-releaseBarrier
			},
		)
	}()

	select {
	case <-barrierEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for WNOWAIT exit barrier")
	}
	if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); err != nil {
		t.Fatalf("process was reaped before exit barrier callback: %v", err)
	}

	close(releaseBarrier)
	select {
	case err := <-waitDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out reaping command after exit barrier")
	}
}
