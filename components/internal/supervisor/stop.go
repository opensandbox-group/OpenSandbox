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

package supervisor

import (
	"os"
	"syscall"
	"time"
)

// gracefulStop sends SIGTERM, then either waits for the worker to exit
// (signalled via done) or escalates to SIGKILL after grace. cmd.Wait in the
// caller is what actually reaps the process; this only signals.
//
// Wall-clock time is used deliberately (rather than the injected Clock):
// the worker is a real OS process, so the grace timeout must elapse in
// real time regardless of how tests fast-forward Spec.Clock.
func gracefulStop(p *os.Process, grace time.Duration, done <-chan struct{}) {
	if p == nil {
		return
	}
	_ = p.Signal(syscall.SIGTERM)
	select {
	case <-time.After(grace):
		_ = p.Kill()
	case <-done:
		// Worker exited within grace; no kill needed.
	}
}
