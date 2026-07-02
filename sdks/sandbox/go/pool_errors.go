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

// PoolEmptyError is returned when Acquire is called with FailFast policy
// and no idle sandbox is available.
type PoolEmptyError struct {
	PoolName string
}

func (e *PoolEmptyError) Error() string {
	return fmt.Sprintf("pool %q has no idle sandboxes available", e.PoolName)
}

// PoolAcquireFailedError is returned when an idle sandbox was found but
// could not be connected or failed health check.
type PoolAcquireFailedError struct {
	PoolName  string
	SandboxID string
	Err       error
}

func (e *PoolAcquireFailedError) Error() string {
	return fmt.Sprintf("pool %q: failed to acquire sandbox %s: %v", e.PoolName, e.SandboxID, e.Err)
}

func (e *PoolAcquireFailedError) Unwrap() error { return e.Err }

// PoolNotRunningError is returned when an operation is attempted on a pool
// that is not in RUNNING state.
type PoolNotRunningError struct {
	PoolName string
	State    PoolState
}

func (e *PoolNotRunningError) Error() string {
	return fmt.Sprintf("pool %q is not running (current state: %s)", e.PoolName, e.State)
}
