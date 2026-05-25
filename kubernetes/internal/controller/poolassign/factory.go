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

import "fmt"

type weightedScorer struct {
	Scorer
	weight int64
}

type predicateCreator func(args map[string]interface{}) (Predicate, error)
type scorerCreator func(args map[string]interface{}) (Scorer, error)

var (
	predicateRegistry = map[string]predicateCreator{}
	scorerRegistry    = map[string]scorerCreator{}
)

func registerPredicate(name string, creator predicateCreator) {
	predicateRegistry[name] = creator
}

func registerScorer(name string, creator scorerCreator) {
	scorerRegistry[name] = creator
}

func NewPredicates(profile *Profile) ([]Predicate, error) {
	var predicates []Predicate
	for _, name := range profile.Plugins.Predicate {
		creator, ok := predicateRegistry[name]
		if !ok {
			continue
		}
		p, err := creator(pluginArgs(profile, name))
		if err != nil {
			return nil, fmt.Errorf("predicate %q: %w", name, err)
		}
		predicates = append(predicates, p)
	}
	return predicates, nil
}

func NewScorers(profile *Profile) ([]weightedScorer, error) {
	var scorers []weightedScorer
	for _, spec := range profile.Plugins.Score {
		creator, ok := scorerRegistry[spec.Name]
		if !ok {
			continue
		}
		s, err := creator(pluginArgs(profile, spec.Name))
		if err != nil {
			return nil, fmt.Errorf("scorer %q: %w", spec.Name, err)
		}
		scorers = append(scorers, weightedScorer{
			Scorer: s,
			weight: spec.Weight,
		})
	}
	return scorers, nil
}

func pluginArgs(profile *Profile, pluginName string) map[string]interface{} {
	for _, conf := range profile.PluginConf {
		if conf.Name == pluginName {
			return conf.Args
		}
	}
	return nil
}
