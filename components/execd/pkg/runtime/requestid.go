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

package runtime

import (
	"runtime"
	"strings"
)

// RequestIDEnvVar is the environment variable carrying the ingress request
// id in sandbox subprocesses (exported by WrapCommandWithRequestID and
// injected directly into cmd.Env by prepareCommand).
const RequestIDEnvVar = "SANDBOX_REQUEST_ID"

// WrapCommandWithRequestID prepends a shell export of the ingress request id
// to the command text, so that:
//
//   - the request id appears in the launched shell's argv, captured by
//     runtime audit (e.g. Tetragon process_exec arguments);
//   - the request id is inherited by descendant processes via the
//     environment (the shell exports it before running the user code).
//
// An empty requestID returns code unchanged. The POSIX `export ...;` prefix
// is only valid for POSIX shells: on Windows (cmd /C) the command is returned
// unchanged and the request id propagates via the injected environment
// instead (see prepareCommand).
func WrapCommandWithRequestID(code, requestID string) string {
	return wrapCommandForShell(runtime.GOOS, code, requestID)
}

// wrapCommandForShell is the GOOS-parametrized core of
// WrapCommandWithRequestID, kept pure for cross-platform testing.
func wrapCommandForShell(goos, code, requestID string) string {
	if requestID == "" || goos == goosWindows {
		return code
	}
	return "export " + RequestIDEnvVar + "=" + shellQuote(requestID) + "; " + code
}

// shellQuote wraps s in single quotes for POSIX shell, escaping embedded
// single quotes with the standard '\” form.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}
