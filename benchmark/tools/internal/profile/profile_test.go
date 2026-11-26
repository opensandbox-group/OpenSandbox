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
	require.True(t, p.Environment.Registry.EphemeralInCluster)
	require.Equal(t, DefaultRegistryHostPath, p.Environment.Registry.HostPath)
	require.Equal(t, 2, p.Stack.Template.VCPUs)
	require.Equal(t, 4096, p.Stack.Template.MemoryMiB)
	require.Equal(t, 2, p.Stack.Template.DiskGiB)
	require.Equal(t, 64, p.Stack.FastSandbox.Runtime.OCI.CacheSizeGB)
	require.Equal(t, 32, p.Stack.FastSandbox.Runtime.OCI.PoolLow)
	require.Equal(t, 64, p.Stack.FastSandbox.Runtime.OCI.PoolHigh)
	require.Equal(t, 2, p.Stack.FastSandbox.Runtime.OCI.PoolSizeGB)

	pool := p.Stack.FastSandbox.Pool
	require.Equal(t, PoolModeResourceAware, pool.Mode)
	require.Equal(t, 2, pool.MinFastlets)
	require.Equal(t, 2, pool.MaxFastlets)
	require.Equal(t, 500, pool.MaxSandboxesPerPod)
	require.Equal(t, 8.0, pool.OvercommitRatio.CPU)
	require.Equal(t, 4.0, pool.OvercommitRatio.Memory)
	require.True(t, pool.EgressEnabled)
	require.NotNil(t, pool.EgressResources)
	require.Equal(t, "2", pool.EgressResources.Requests.CPU)
	require.Equal(t, "4Gi", pool.EgressResources.Requests.Memory)
	require.Equal(t, "4", pool.EgressResources.Limits.CPU)
	require.Equal(t, "16Gi", pool.EgressResources.Limits.Memory)
	require.NotNil(t, pool.FastletProxyResources)
	require.Equal(t, "2", pool.FastletProxyResources.Requests.CPU)
	require.Equal(t, "4Gi", pool.FastletProxyResources.Requests.Memory)
	require.Equal(t, "2", pool.FastletProxyResources.Limits.CPU)
	require.Equal(t, "4Gi", pool.FastletProxyResources.Limits.Memory)
	require.NotNil(t, pool.PodAntiAffinity)
	require.True(t, *pool.PodAntiAffinity)
	require.Equal(t, int64(68719476736), pool.BlobCacheMaxBytes)

	require.Equal(t, 0.05, p.EffectiveSamplerArg("steady-churn"))
	require.Equal(t, 0.02, p.EffectiveSamplerArg("high-density-soak"))
	require.Equal(t, 1.0, p.EffectiveSamplerArg("schedule-burst"))
}

func TestValidatePoolSizingConstraints(t *testing.T) {
	p, err := LoadEmbedded(DefaultProfileName)
	require.NoError(t, err)

	p.Stack.FastSandbox.Pool.MaxSandboxesPerPod = -1
	err = p.Validate()
	require.Error(t, err)
	require.Contains(t, err.Error(), "maxSandboxesPerPod")

	p.Stack.FastSandbox.Pool.MaxSandboxesPerPod = 500
	p.Stack.FastSandbox.Pool.OvercommitRatio.CPU = 0.5
	err = p.Validate()
	require.Error(t, err)
	require.Contains(t, err.Error(), "overcommitRatio.cpu")

	p.Stack.FastSandbox.Pool.OvercommitRatio.CPU = 1
	p.Stack.FastSandbox.Pool.OvercommitRatio.Memory = 0.5
	err = p.Validate()
	require.Error(t, err)
	require.Contains(t, err.Error(), "overcommitRatio.memory")

	p.Stack.FastSandbox.Pool.OvercommitRatio.Memory = 1
	require.NoError(t, p.Validate())
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

func TestValidatePoolModes(t *testing.T) {
	p, err := LoadEmbedded(DefaultProfileName)
	require.NoError(t, err)

	p.Stack.FastSandbox.Pool.Mode = PoolModeLegacySlot
	require.NoError(t, p.Validate())

	p.Stack.FastSandbox.Pool.Mode = "UnknownMode"
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

func TestApplyDefaultsRegistryHostPath(t *testing.T) {
	t.Run("defaults to local NVMe hostPath when ephemeralInCluster and storageClass empty", func(t *testing.T) {
		p := &BenchmarkProfile{
			Environment: EnvironmentSpec{
				Registry: RegistrySpec{
					EphemeralInCluster: true,
				},
			},
		}
		p.ApplyDefaults()
		require.Equal(t, DefaultRegistryHostPath, p.Environment.Registry.HostPath)
	})

	t.Run("preserves empty hostPath when storageClass is set", func(t *testing.T) {
		p := &BenchmarkProfile{
			Environment: EnvironmentSpec{
				Registry: RegistrySpec{
					EphemeralInCluster: true,
					StorageClass:       "alicloud-disk-essd",
				},
			},
		}
		p.ApplyDefaults()
		require.Empty(t, p.Environment.Registry.HostPath)
	})

	t.Run("preserves empty hostPath when ephemeralInCluster is false", func(t *testing.T) {
		p := &BenchmarkProfile{
			Environment: EnvironmentSpec{
				Registry: RegistrySpec{
					EphemeralInCluster: false,
				},
			},
		}
		p.ApplyDefaults()
		require.Empty(t, p.Environment.Registry.HostPath)
	})
}

func TestParseRejectsUnknownKeys(t *testing.T) {
	raw, err := EmbeddedRaw(DefaultProfileName)
	require.NoError(t, err)

	bad := []byte(string(raw) + "\nunknownTopLevelKey: true\n")
	_, err = Parse(bad)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknownTopLevelKey")
}
