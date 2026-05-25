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
)
