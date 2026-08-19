//go:build !linux

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

// Init mode is Linux-only (OSEP-0018). Off Linux, execd keeps today's
// behavior; the managedProcess fallback for Unix platforms lives in
// initmode_other_unix.go.

package runtime

import "github.com/alibaba/opensandbox/execd/pkg/log"

// StartInitMode is unsupported off Linux; execd keeps today's behavior.
func StartInitMode(entryArgs []string) {
	log.Warn("init mode is unsupported on this platform; continuing without init duties")
}

// InitModeReport reports the init mode actually in effect for the
// capabilities endpoint.
func InitModeReport() (mode string, signalShield bool) {
	return "none", false
}
