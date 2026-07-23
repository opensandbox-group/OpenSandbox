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

// PoolEmptyError is returned when Acquire is called with FAIL_FAST policy
// and no idle sandbox is available.
type PoolEmptyError struct {
	PoolName string
	Policy   AcquirePolicy
}

func (e *PoolEmptyError) Error() string {
	return fmt.Sprintf("opensandbox: pool %q is empty (policy=%s)", e.PoolName, e.Policy)
}

// PoolAcquireFailedError is returned when Acquire finds an idle sandbox but
// fails to connect or health-check it under FAIL_FAST policy.
type PoolAcquireFailedError struct {
	PoolName string
	Cause    error
}

func (e *PoolAcquireFailedError) Error() string {
	return fmt.Sprintf("opensandbox: pool %q acquire failed: %v", e.PoolName, e.Cause)
}

func (e *PoolAcquireFailedError) Unwrap() error { return e.Cause }

// PoolNotRunningError is returned when an operation is attempted on a pool
// that is not in the RUNNING state.
type PoolNotRunningError struct {
	PoolName string
	State    PoolLifecycleState
}

func (e *PoolNotRunningError) Error() string {
	return fmt.Sprintf("opensandbox: pool %q is not running (state=%s)", e.PoolName, e.State)
}

// PoolStateStoreUnavailableError is returned when the pool state store is
// unreachable or otherwise unavailable during idle take/put/lock operations.
type PoolStateStoreUnavailableError struct {
	Operation string
	Cause     error
}

func (e *PoolStateStoreUnavailableError) Error() string {
	return fmt.Sprintf("opensandbox: pool state store unavailable in %s: %v", e.Operation, e.Cause)
}

func (e *PoolStateStoreUnavailableError) Unwrap() error { return e.Cause }
