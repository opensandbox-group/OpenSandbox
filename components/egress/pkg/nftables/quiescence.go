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

package nftables

import "errors"

// ErrQuiesced reports that this Manager no longer admits runtime nft writes.
var ErrQuiesced = errors.New("nftables manager is quiesced")

// Quiesce permanently closes admission before waiting for the current lock
// holder to finish. Returning guarantees admitted runtime writes have drained.
// It does not remove enforcement or cancel an already admitted write.
func (m *Manager) Quiesce() {
	m.quiesced.Store(true)
	m.mu.Lock()
	m.mu.Unlock()
}

func (m *Manager) isQuiesced() bool {
	return m.quiesced.Load()
}
