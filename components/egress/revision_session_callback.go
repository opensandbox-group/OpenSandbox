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

package main

import (
	"context"

	"github.com/alibaba/opensandbox/egress/pkg/revision"
)

// withRevisionMutationSession pins the current live generation and session for
// the full callback, preventing lifecycle writers from replacing or closing
// either while a revision mutation is unresolved. Future callers must provide
// a bounded context with a deadline that is canceled when sidecar shutdown
// begins, first hold policyServer.mu, and complete Update/ReconcileUpdate plus
// local Store candidate finalization in one callback. The callback must pass
// the same context to Update/ReconcileUpdate and return promptly when canceled.
// This helper cannot terminate an uncooperative callback; do not detach it to a
// goroutine and release the read lock on cancellation. The callback must not
// reenter a lifecycle method that needs m.mu's write lock.
func (m *mitmTransparent) withRevisionMutationSession(
	ctx context.Context,
	callback func(context.Context, revisionMutationSession) error,
) error {
	if m == nil || ctx == nil || callback == nil {
		return revision.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	if err := ctx.Err(); err != nil {
		return err
	}
	if m.stopping {
		return revision.ErrClosed
	}
	if m.running == nil || m.revisionSession == nil {
		return revision.ErrTransportUnavailable
	}
	return callback(ctx, m.revisionSession)
}
