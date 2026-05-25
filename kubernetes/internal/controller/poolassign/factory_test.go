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

package assign

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewPredicates(t *testing.T) {
	t.Run("default profile creates image, resource, nodeselector predicates", func(t *testing.T) {
		predicates, err := NewPredicates(DefaultProfile())
		require.NoError(t, err)
		assert.Len(t, predicates, 3)
		assert.IsType(t, &imagePredicate{}, predicates[0])
		assert.IsType(t, &resourcePredicate{}, predicates[1])
		assert.IsType(t, &nodeSelectorPredicate{}, predicates[2])
	})

	t.Run("profile with labelselector predicate", func(t *testing.T) {
		profile := &Profile{
			Name: "custom",
			Plugins: PluginsSpec{
				Predicate: []string{"image", "labelselector"},
			},
			PluginConf: []PluginConf{
				{Name: "labelselector", Args: map[string]interface{}{"keys": []interface{}{"env", "tier"}}},
			},
		}
		predicates, err := NewPredicates(profile)
		require.NoError(t, err)
		assert.Len(t, predicates, 2)
		assert.IsType(t, &imagePredicate{}, predicates[0])
		assert.IsType(t, &labelSelectorPredicate{}, predicates[1])
	})

	t.Run("empty profile creates no predicates", func(t *testing.T) {
		profile := &Profile{Name: "empty"}
		predicates, err := NewPredicates(profile)
		require.NoError(t, err)
		assert.Len(t, predicates, 0)
	})

	t.Run("unknown predicate name is skipped", func(t *testing.T) {
		profile := &Profile{
			Name:    "unknown",
			Plugins: PluginsSpec{Predicate: []string{"image", "unknown-plugin"}},
		}
		predicates, err := NewPredicates(profile)
		require.NoError(t, err)
		assert.Len(t, predicates, 1)
		assert.IsType(t, &imagePredicate{}, predicates[0])
	})

	t.Run("creator error propagates", func(t *testing.T) {
		profile := &Profile{
			Name: "bad-scorer-as-pred",
			Plugins: PluginsSpec{
				Predicate: []string{"image"},
			},
			PluginConf: []PluginConf{
				{Name: "image", Args: map[string]interface{}{}},
			},
		}
		_, err := NewPredicates(profile)
		assert.NoError(t, err)
	})
}

func TestNewScorers(t *testing.T) {
	t.Run("default profile creates resbalance scorer with weight", func(t *testing.T) {
		scorers, err := NewScorers(DefaultProfile())
		require.NoError(t, err)
		assert.Len(t, scorers, 1)
		assert.IsType(t, &resBalanceScorer{}, scorers[0].Scorer)
		assert.Equal(t, int64(100), scorers[0].weight)
	})

	t.Run("custom profile with MostAllocated strategy", func(t *testing.T) {
		profile := &Profile{
			Name: "custom",
			Plugins: PluginsSpec{
				Score: []ScoreSpec{{Name: "resbalance", Weight: 50}},
			},
			PluginConf: []PluginConf{
				{Name: "resbalance", Args: map[string]interface{}{"strategy": "MostAllocated"}},
			},
		}
		scorers, err := NewScorers(profile)
		require.NoError(t, err)
		assert.Len(t, scorers, 1)
		assert.IsType(t, &resBalanceScorer{}, scorers[0].Scorer)
		assert.Equal(t, int64(50), scorers[0].weight)
	})

	t.Run("empty profile creates no scorers", func(t *testing.T) {
		profile := &Profile{Name: "empty"}
		scorers, err := NewScorers(profile)
		require.NoError(t, err)
		assert.Len(t, scorers, 0)
	})

	t.Run("unknown scorer name is skipped", func(t *testing.T) {
		profile := &Profile{
			Name: "unknown",
			Plugins: PluginsSpec{
				Score: []ScoreSpec{{Name: "resbalance", Weight: 100}, {Name: "unknown-scorer", Weight: 50}},
			},
		}
		scorers, err := NewScorers(profile)
		require.NoError(t, err)
		assert.Len(t, scorers, 1)
	})

	t.Run("invalid scorer args returns error", func(t *testing.T) {
		profile := &Profile{
			Name: "bad-strategy",
			Plugins: PluginsSpec{
				Score: []ScoreSpec{{Name: "resbalance", Weight: 100}},
			},
			PluginConf: []PluginConf{
				{Name: "resbalance", Args: map[string]interface{}{"strategy": "InvalidStrategy"}},
			},
		}
		_, err := NewScorers(profile)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "scorer \"resbalance\"")
	})
}

func TestPluginArgs(t *testing.T) {
	t.Run("returns args for existing plugin", func(t *testing.T) {
		profile := &Profile{
			PluginConf: []PluginConf{
				{Name: "resbalance", Args: map[string]interface{}{"strategy": "MostAllocated"}},
			},
		}
		args := pluginArgs(profile, "resbalance")
		assert.Equal(t, "MostAllocated", args["strategy"])
	})

	t.Run("returns nil for missing plugin", func(t *testing.T) {
		profile := &Profile{}
		args := pluginArgs(profile, "nonexistent")
		assert.Nil(t, args)
	})
}
