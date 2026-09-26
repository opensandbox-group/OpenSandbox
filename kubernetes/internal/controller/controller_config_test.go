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

package controller

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestFeatureConfigDuration(t *testing.T) {
	cfg := NewFeatureConfig()
	assert.Equal(t, defaultPodRecoveryStuckThreshold, cfg.duration(featureConfigKeyPodRecoveryStuckThreshold, defaultPodRecoveryStuckThreshold))

	cfg.Load(map[string]string{
		featureConfigKeyPodRecoveryStuckThreshold: "2m",
	})
	assert.Equal(t, 2*time.Minute, cfg.duration(featureConfigKeyPodRecoveryStuckThreshold, defaultPodRecoveryStuckThreshold))

	// Values with surrounding whitespace and invalid or non-positive entries
	// fall back to the default.
	cfg.Load(map[string]string{
		featureConfigKeyPodRecoveryStuckThreshold: " 90s ",
	})
	assert.Equal(t, 90*time.Second, cfg.duration(featureConfigKeyPodRecoveryStuckThreshold, defaultPodRecoveryStuckThreshold))
	cfg.Load(map[string]string{
		featureConfigKeyPodRecoveryStuckThreshold: "not-a-duration",
	})
	assert.Equal(t, defaultPodRecoveryStuckThreshold, cfg.duration(featureConfigKeyPodRecoveryStuckThreshold, defaultPodRecoveryStuckThreshold))
	cfg.Load(map[string]string{
		featureConfigKeyPodRecoveryStuckThreshold: "-5m",
	})
	assert.Equal(t, defaultPodRecoveryStuckThreshold, cfg.duration(featureConfigKeyPodRecoveryStuckThreshold, defaultPodRecoveryStuckThreshold))
}

func TestFeatureConfigPositiveInt(t *testing.T) {
	cfg := NewFeatureConfig()
	assert.Equal(t, defaultPodRecoveryMaxAttempts, cfg.positiveInt(featureConfigKeyPodRecoveryMaxAttempts, defaultPodRecoveryMaxAttempts))

	cfg.Load(map[string]string{featureConfigKeyPodRecoveryMaxAttempts: "5"})
	assert.Equal(t, 5, cfg.positiveInt(featureConfigKeyPodRecoveryMaxAttempts, defaultPodRecoveryMaxAttempts))

	cfg.Load(map[string]string{featureConfigKeyPodRecoveryMaxAttempts: "0"})
	assert.Equal(t, defaultPodRecoveryMaxAttempts, cfg.positiveInt(featureConfigKeyPodRecoveryMaxAttempts, defaultPodRecoveryMaxAttempts))

	cfg.Load(map[string]string{featureConfigKeyPodRecoveryMaxAttempts: "many"})
	assert.Equal(t, defaultPodRecoveryMaxAttempts, cfg.positiveInt(featureConfigKeyPodRecoveryMaxAttempts, defaultPodRecoveryMaxAttempts))
}

func TestFeatureConfigReloadAndClear(t *testing.T) {
	cfg := NewFeatureConfig()
	cfg.Load(map[string]string{featureConfigKeyPodRecoveryMaxAttempts: "7"})
	assert.Equal(t, 7, cfg.positiveInt(featureConfigKeyPodRecoveryMaxAttempts, defaultPodRecoveryMaxAttempts))

	// A reload replaces the whole set, and clearing it restores defaults.
	cfg.Load(nil)
	_, ok := cfg.get(featureConfigKeyPodRecoveryMaxAttempts)
	assert.False(t, ok)
}
