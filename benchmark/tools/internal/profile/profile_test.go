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

package profile

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEmbeddedReferenceProfileValid(t *testing.T) {
	p, err := LoadEmbedded(DefaultProfileName)
	require.NoError(t, err)
	require.Equal(t, DefaultProfileName, p.Metadata.Name)
	require.Equal(t, RuntimeFastSandboxOverlayBD, p.Runtime)
	require.Equal(t, 2, p.Stack.Template.DiskGiB)
	require.Equal(t, 2, p.Stack.FastSandbox.Runtime.OCI.PoolSizeGB)
	require.Equal(t, 0.05, p.EffectiveSamplerArg("steady-churn"))
	require.Equal(t, 0.02, p.EffectiveSamplerArg("high-density-soak"))
	require.Equal(t, 1.0, p.EffectiveSamplerArg("schedule-burst"))
}

func TestValidateRejectsPoolSizeMismatch(t *testing.T) {
	p, err := LoadEmbedded(DefaultProfileName)
	require.NoError(t, err)

	p.Stack.Template.DiskGiB = 4
	p.Stack.FastSandbox.Runtime.OCI.PoolSizeGB = 2
	err = p.Validate()
	require.Error(t, err)
	require.Contains(t, err.Error(), "poolSizeGB")
}

func TestValidateRejectsLegacySlotPoolInPhase1(t *testing.T) {
	p, err := LoadEmbedded(DefaultProfileName)
	require.NoError(t, err)

	p.Stack.FastSandbox.Pool.Mode = "LegacySlot"
	err = p.Validate()
	require.Error(t, err)
	require.Contains(t, err.Error(), "ResourceAware")
}

func TestValidateRejectsWakeStormWithoutRedis(t *testing.T) {
	p, err := LoadEmbedded(DefaultProfileName)
	require.NoError(t, err)

	p.Environment.Redis.Enabled = false
	err = p.Validate()
	require.Error(t, err)
	require.Contains(t, err.Error(), "redis")
}

func TestParseRejectsUnknownKeys(t *testing.T) {
	raw, err := EmbeddedRaw(DefaultProfileName)
	require.NoError(t, err)

	bad := []byte(string(raw) + "\nunknownTopLevelKey: true\n")
	_, err = Parse(bad)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknownTopLevelKey")
}
