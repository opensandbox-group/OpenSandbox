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

package mitmproxy

import (
	"fmt"
	"os/exec"
	"syscall"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/log"
)

// GracefulShutdown sends SIGTERM to mitmdump, waits up to timeout, then SIGKILL if still running.
func GracefulShutdown(r *Running, timeout time.Duration) {
	if r == nil || r.Cmd == nil {
		return
	}
	if r.Cmd.Process == nil {
		return
	}

	select {
	case err := <-r.done:
		if err != nil {
			log.Warnf("[mitmproxy] mitmdump already exited: %v", err)
		}
		return
	default:
	}

	if err := r.Cmd.Process.Signal(syscall.SIGTERM); err != nil {
		log.Warnf("[mitmproxy] SIGTERM mitmdump: %v", err)
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case err := <-r.done:
		if err != nil && !isSigKillOrTerm(err) {
			log.Warnf("[mitmproxy] mitmdump exited: %v", err)
		} else {
			log.Infof("[mitmproxy] mitmdump stopped")
		}
		return
	case <-timer.C:
	}

	log.Warnf("[mitmproxy] mitmdump did not exit within %v; sending SIGKILL", timeout)
	if err := r.Cmd.Process.Kill(); err != nil {
		log.Warnf("[mitmproxy] kill mitmdump: %v", err)
	}
	if err := <-r.done; err != nil && !isSigKillOrTerm(err) {
		log.Warnf("[mitmproxy] mitmdump exited after kill: %v", err)
	}
}

func isSigKillOrTerm(err error) bool {
	if err == nil {
		return true
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok {
			return ws.Signaled() && (ws.Signal() == syscall.SIGKILL || ws.Signal() == syscall.SIGTERM)
		}
	}

	return fmt.Sprint(err) == "signal: killed" || fmt.Sprint(err) == "signal: terminated"
}
