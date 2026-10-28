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

package opensandbox

import (
	"fmt"
	"time"
)

// SandboxReadyTimeoutError is returned when WaitUntilReady exceeds the deadline.
type SandboxReadyTimeoutError struct {
	SandboxID string
	Elapsed   string
	LastErr   error
}

func (e *SandboxReadyTimeoutError) Error() string {
	msg := fmt.Sprintf("sandbox %s did not become ready within %s", e.SandboxID, e.Elapsed)
	if e.LastErr != nil {
		msg += fmt.Sprintf(": last error: %v", e.LastErr)
	}
	return msg
}

func (e *SandboxReadyTimeoutError) Unwrap() error { return e.LastErr }

// SandboxRunningTimeoutError is returned when waiting for a sandbox to enter
// Running state exceeds the deadline.
type SandboxRunningTimeoutError struct {
	SandboxID string
	Elapsed   string
	LastErr   error
}

func (e *SandboxRunningTimeoutError) Error() string {
	msg := fmt.Sprintf("sandbox %s did not reach Running state within %s", e.SandboxID, e.Elapsed)
	if e.LastErr != nil {
		msg += fmt.Sprintf(": last error: %v", e.LastErr)
	}
	return msg
}

func (e *SandboxRunningTimeoutError) Unwrap() error { return e.LastErr }

// SandboxUnhealthyError is returned when a sandbox is determined to be unhealthy.
type SandboxUnhealthyError struct {
	SandboxID string
	Reason    string
}

func (e *SandboxUnhealthyError) Error() string {
	return fmt.Sprintf("sandbox %s is unhealthy: %s", e.SandboxID, e.Reason)
}

// InvalidArgumentError is returned for invalid SDK arguments.
type InvalidArgumentError struct {
	Field   string
	Message string
}

func (e *InvalidArgumentError) Error() string {
	return fmt.Sprintf("invalid argument %q: %s", e.Field, e.Message)
}

// SandboxWakeTimeoutError is returned when the OSEP-0024 wake retry budget is
// exhausted.
//
// The gateway answered 503 with the "OpenSandbox-Wake: expired" marker on
// every attempt: the sandbox restore did not finish within the wake retry
// budget. The request was never forwarded, so retrying later is safe — the
// server-side resume continues in the background regardless.
type SandboxWakeTimeoutError struct {
	// Attempts is the number of marked wake-timeout responses received.
	Attempts int
	// Waited is the total wall-clock time spent across waits, retries, and
	// request time.
	Waited time.Duration
	// LastErr is the last marked 503 APIError, carrying the status code,
	// request id, and parsed body.
	LastErr error
}

func (e *SandboxWakeTimeoutError) Error() string {
	return fmt.Sprintf(
		"sandbox wake timeout: restore did not finish within the wake retry budget after %d attempt(s) and %s; the request was never forwarded and is safe to retry",
		e.Attempts, e.Waited,
	)
}

func (e *SandboxWakeTimeoutError) Unwrap() error { return e.LastErr }
