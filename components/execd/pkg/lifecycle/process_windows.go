//go:build windows

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
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/alibaba/opensandbox/execd/pkg/log"
	"golang.org/x/sys/windows"
)

const taskkillTimeout = 2 * time.Second

func prepareCommand(_ *exec.Cmd) {}

func terminateCommand(cmd *exec.Cmd) {
	if cmd.Process != nil {
		killCtx, killCancel := context.WithTimeout(context.Background(), taskkillTimeout)
		defer killCancel()
		if systemDirectory, err := windows.GetSystemDirectory(); err == nil {
			if err := exec.CommandContext(
				killCtx,
				filepath.Join(systemDirectory, "taskkill.exe"),
				"/T",
				"/F",
				"/PID",
				strconv.Itoa(cmd.Process.Pid),
			).Run(); err != nil {
				log.Warn("lifecycle: terminate Windows process tree for pid %d: %v", cmd.Process.Pid, err)
			}
		} else {
			log.Warn("lifecycle: resolve Windows system directory for taskkill: %v", err)
		}
		_ = cmd.Process.Kill()
	}
}
