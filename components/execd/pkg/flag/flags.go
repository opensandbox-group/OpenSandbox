// Copyright 2025 Alibaba Group Holding Ltd.
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
	// JupyterServerHost points to the target Jupyter instance.
	JupyterServerHost string

	// JupyterServerToken authenticates requests to the Jupyter server.
	JupyterServerToken string

	// ServerPort controls the HTTP listener port.
	ServerPort int

	// ServerLogLevel controls the server log verbosity.
	ServerLogLevel int

	// ServerAccessToken guards API entrypoints when set.
	ServerAccessToken string

	// ApiGracefulShutdownTimeout waits before tearing down SSE streams.
	ApiGracefulShutdownTimeout time.Duration

	// JupyterIdlePollInterval controls how often ExecuteCodeStream checks for
	// late execute_result/error messages after receiving idle status.
	JupyterIdlePollInterval time.Duration

	// IsolationUpperRoot is the parent directory for per-session overlay
	// upper directories.
	IsolationUpperRoot string

	// IsolationUpperMaxBytes is the hard limit on total upper directory
	// size across all sessions (8 GiB default).
	IsolationUpperMaxBytes int64

	// IsolationDiffMaxBytes limits the maximum tar.gz diff output size
	// (4 GiB default, Phase 2).
	IsolationDiffMaxBytes int64

	// IsolationAllowedWritable is a comma-separated allowlist of paths
	// that callers may request as extra_writable. Empty = reject all.
	IsolationAllowedWritable string
)
