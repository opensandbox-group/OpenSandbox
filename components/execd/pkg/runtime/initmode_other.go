//go:build !linux

// Copyright 2026 The OpenSandbox Authors
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

// PrepareInitMode is unsupported off Linux; execd keeps today's behavior.
func PrepareInitMode() func([]string) error {
	log.Warn("init: mode unsupported on this platform; continuing without init duties")
	return LaunchUserEntrypoint
}

// LaunchUserEntrypoint is a no-op off Linux: there is no supervised
// entrypoint to (re)start, and POST /internal/init only applies the RuntimeBinding.
func LaunchUserEntrypoint([]string) error { return nil }

// EntrypointRunning is always false off Linux: no supervised entrypoint.
func EntrypointRunning() bool { return false }

// RetireEntrypoint is a no-op off Linux: there is no supervised entrypoint.
func RetireEntrypoint() {}

// StopUserProcesses is a no-op off Linux; sessions tear themselves down
// through Controller.Reset.
func StopUserProcesses(keepEntrypoint bool) {}

// InitModeReport reports the init mode actually in effect for the
// capabilities endpoint.
func InitModeReport() (mode string, signalShield bool) {
	return "none", false
}
