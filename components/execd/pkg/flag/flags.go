// Copyright 2025 The OpenSandbox Authors
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

package flag

import "time"

var (
	JupyterServerHost string

	JupyterServerToken string

	ServerPort int

	ServerLogLevel int

	// ServerAccessToken guards API entrypoints when set.
	ServerAccessToken string

	// ApiGracefulShutdownTimeout waits before tearing down SSE streams.
	ApiGracefulShutdownTimeout time.Duration

	// JupyterIdlePollInterval controls how often ExecuteCodeStream checks for
	// late execute_result/error messages after receiving idle status.
	JupyterIdlePollInterval time.Duration

	// IsolationConfigPath points to the TOML isolation config file.
	// Empty means use built-in defaults.
	IsolationConfigPath string

	// InitMode runs execd as the sandbox init: reap children, forward
	// signals, and own the container lifecycle. Topology (PID 1 vs
	// subreaper) is decided by bootstrap.sh via EXECD_INIT, which passes
	// this flag when it execs into execd.
	InitMode bool

	// LifecycleStartupStatusFile is an internal bootstrap synchronization file.
	// Execd writes the preStart result after its HTTP server is available.
	LifecycleStartupStatusFile string

	// RuntimeInit gates user workload startup on POST /internal/init: when enabled,
	// execd skips the legacy template-driven startup (preStart + entrypoint)
	// and waits for the control plane to apply the RuntimeBinding. When
	// disabled, /internal/init is still accepted and becomes authoritative on apply,
	// preserving legacy behavior for control planes that never call it.
	RuntimeInit bool

	// SseLegacyCompat omits the exit_code field from execution_complete and
	// error SSE events so that clients whose EventNode schemas predate the
	// field never observe an unknown key. Successful foreground commands
	// carry no information in the field (it is a constant 0; failures are
	// reported via the error event), and newer SDKs already infer 0 from a
	// complete event without exit_code, so enabling this is lossless for
	// successful commands. Enable it while legacy SDK versions are still
	// deployed; turn it off once the fleet has upgraded.
	SseLegacyCompat bool
)
