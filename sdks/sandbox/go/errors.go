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

package opensandbox

import "fmt"

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
