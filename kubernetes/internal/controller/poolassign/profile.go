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

type Profile struct {
	Name       string       `json:"name"`
	Plugins    PluginsSpec  `json:"plugins"`
	PluginConf []PluginConf `json:"pluginConf,omitempty"`
}

type PluginsSpec struct {
	Predicate []string    `json:"predicate"`
	Score     []ScoreSpec `json:"score"`
}

type ScoreSpec struct {
	Name   string `json:"name"`
	Weight int64  `json:"weight"`
}

type PluginConf struct {
	Name string                 `json:"name"`
	Args map[string]interface{} `json:"args,omitempty"`
}

const DefaultProfileName = "default"

func DefaultProfile() *Profile {
	return &Profile{
		Name: DefaultProfileName,
		Plugins: PluginsSpec{
			Predicate: []string{"image", "resource", "nodeselector"},
			Score:     []ScoreSpec{{Name: "resbalance", Weight: 100}},
		},
		PluginConf: []PluginConf{
			{Name: "resbalance", Args: map[string]interface{}{"strategy": "LeastAllocated"}},
		},
	}
}
