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
