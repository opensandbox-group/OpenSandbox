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

package server

import (
	api "github.com/alibaba/OpenSandbox/sandbox-k8s/pkg/task-executor"
)

// ownerMismatch reports whether a task sync asserts a Pod identity that does
// not match this executor's own Pod. It never trusts the network peer: the
// sender's assertion is compared against the identity the executor learned
// about itself (downward API env), never against the source address.
//
// The comparison is best-effort to stay backward compatible. A Pod UID match is
// authoritative when both sides assert one; otherwise the Pod names are
// compared. When either side asserts no identity, or the two sides assert
// different identity kinds, the request is accepted so that rolling upgrades
// (old senders, or deployments that do not project Pod identity) keep working.
func ownerMismatch(local, claimed api.TaskOwner) bool {
	if local.PodName == "" && local.PodUID == "" {
		// This executor does not know its own identity; fencing is disabled.
		return false
	}
	if claimed.PodName == "" && claimed.PodUID == "" {
		// The sender did not assert an identity (an older sender).
		return false
	}
	if local.PodUID != "" && claimed.PodUID != "" {
		return local.PodUID != claimed.PodUID
	}
	if local.PodName != "" && claimed.PodName != "" {
		return local.PodName != claimed.PodName
	}
	return false
}
