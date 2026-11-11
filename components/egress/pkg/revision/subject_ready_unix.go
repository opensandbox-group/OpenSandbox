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

package revision

import (
	"context"
	"encoding/json"
	"net/http"
)

// CheckFreshSubjectSession authenticates a new child without allocating a
// lifecycle sequence or inventing a probe subject. Call only before publication.
func CheckFreshSubjectSession(ctx context.Context, socketPath, token, control string) error {
	if !validSubjectString(control, 128) {
		return ErrInvalidTransport
	}
	ipc, err := newSubjectIPCClient(socketPath, token)
	if err != nil {
		return err
	}
	defer ipc.client.CloseIdleConnections()
	body, _ := json.Marshal(struct {
		Control string `json:"controlGeneration"`
	}{control})
	status, data, err := ipc.post(ctx, "/v2/subjects/lifecycle-status", body)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return ErrTransportRejected
	}
	record, err := decodeLifecycleRecord(data, "lastSequence", true)
	if err != nil {
		return err
	}
	if record.LifecycleSequence != 0 {
		return ErrIndeterminate
	}
	return nil
}
