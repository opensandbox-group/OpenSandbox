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

package mitmproxy

import (
	"testing"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/stretchr/testify/require"
)

func TestHealthGate(t *testing.T) {
	t.Run("transparent", func(t *testing.T) {
		t.Setenv(constants.EnvMitmproxyTransparent, "1")
		on := NewHealthGate()
		require.True(t, on.MitmPending())
		on.MarkStackReady()
		require.False(t, on.MitmPending())
	})
	t.Run("not transparent", func(t *testing.T) {
		t.Setenv(constants.EnvMitmproxyTransparent, "")
		off := NewHealthGate()
		require.False(t, off.MitmPending())
	})
}

func TestHealthGateAddonDown(t *testing.T) {
	t.Setenv(constants.EnvMitmproxyTransparent, "1")
	gate := NewHealthGate()

	_, down := gate.DownReason()
	require.False(t, down, "fresh gate is not down")

	gate.MarkDown("pinned CA unavailable: missing seed")
	require.True(t, gate.MitmPending(), "addon-down must clear readiness")
	reason, down := gate.DownReason()
	require.True(t, down)
	require.Contains(t, reason, "pinned CA unavailable")

	// First failure wins: later calls cannot clear or replace the latch.
	gate.MarkDown("different reason")
	reason, down = gate.DownReason()
	require.True(t, down)
	require.Contains(t, reason, "missing seed")

	// The latch is type-enforced: a down addon cannot be resurrected by a
	// late SetReady(true) (e.g. a future restart/watch path) — that would
	// surface ready:true + mitmCaDown:true. Only a fresh process clears it.
	gate.SetReady(true)
	gate.MarkStackReady()
	require.True(t, gate.MitmPending(), "SetReady(true) must be a no-op while latched down")
}

func TestHealthGateCAFingerprint(t *testing.T) {
	var nilGate *HealthGate
	require.Equal(t, "", nilGate.CAFingerprint(), "nil gate must be safe")

	t.Setenv(constants.EnvMitmproxyTransparent, "")
	gate := NewHealthGate()
	require.Equal(t, "", gate.CAFingerprint())
	gate.SetCAFingerprint("abc123")
	require.Equal(t, "abc123", gate.CAFingerprint())
}
