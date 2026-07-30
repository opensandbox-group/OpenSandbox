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

package runtime

import "errors"

var ErrContextNotFound = errors.New("context not found")

// ErrUidModeUnavailable indicates that the requested isolation uid mode did
// not pass the startup capability probe in the current environment.
var ErrUidModeUnavailable = errors.New("requested uid mode is unavailable")

// ErrSessionTeardownTimeout indicates that a session workload could not be
// reaped within the bounded teardown window. The caller must retain ownership
// and retry cleanup rather than treating deletion as successful.
var ErrSessionTeardownTimeout = errors.New("isolated session teardown timed out")

// ErrSessionLifecycleUnavailable indicates that the selected isolator cannot
// provide a fail-closed startup gate and verified host workload identity.
var ErrSessionLifecycleUnavailable = errors.New("isolated session lifecycle is unavailable")

// ErrSessionNotActive indicates that a terminal or stopping session no longer
// accepts run or filesystem operations. Cleanup may still be retried through
// Delete while the controller retains ownership of the session resources.
var ErrSessionNotActive = errors.New("isolated session is not active")
