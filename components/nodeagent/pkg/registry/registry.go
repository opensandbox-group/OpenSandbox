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

// Package registry provides the compile-time Source and Sink factory registry.
// Implementations register from explicit imports; Node Agent does not load
// runtime plugins.
package registry

import (
	"errors"

	"github.com/alibaba/opensandbox/internal/logger"
	"github.com/alibaba/opensandbox/nodeagent/pkg/api"
	"github.com/alibaba/opensandbox/nodeagent/pkg/config"
	"github.com/alibaba/opensandbox/nodeagent/pkg/state"
	"github.com/alibaba/opensandbox/nodeagent/pkg/store"
)

type Dependencies struct {
	Config  config.Config
	Store   store.View
	State   *state.DB
	Logger  logger.Logger
	OnError func(error)
}

type SourceFactory func(Dependencies) (api.Source, error)
type SinkFactory func(Dependencies) (api.Sink, error)
type SinkTargetID func(config.Config) (string, error)

type sinkRegistration struct {
	targetID SinkTargetID
	factory  SinkFactory
}

var (
	sources = make(map[string]SourceFactory)
	sinks   = make(map[string]sinkRegistration)
)

func RegisterSource(name string, factory SourceFactory) {
	if name == "" || factory == nil {
		panic("nodeagent: invalid Source factory registration")
	}
	if _, exists := sources[name]; exists {
		panic("nodeagent: duplicate Source factory " + name)
	}
	sources[name] = factory
}

func RegisterSink(name string, targetID SinkTargetID, factory SinkFactory) {
	if name == "" || targetID == nil || factory == nil {
		panic("nodeagent: invalid Sink factory registration")
	}
	if _, exists := sinks[name]; exists {
		panic("nodeagent: duplicate Sink factory " + name)
	}
	sinks[name] = sinkRegistration{targetID: targetID, factory: factory}
}

func BuildSource(name string, dependencies Dependencies) (api.Source, error) {
	if dependencies.State == nil || dependencies.Store == nil || dependencies.Logger == nil {
		return nil, errors.New("source dependencies require State, Store, and Logger")
	}
	factory := sources[name]
	if factory == nil {
		return nil, errors.New("source is not compiled into Node Agent: " + name)
	}
	return factory(dependencies)
}

func TargetID(name string, cfg config.Config) (string, error) {
	registration, ok := sinks[name]
	if !ok {
		return "", errors.New("sink is not compiled into Node Agent: " + name)
	}
	return registration.targetID(cfg)
}

func BuildSink(name string, dependencies Dependencies) (api.Sink, error) {
	if dependencies.State == nil {
		return nil, errors.New("sink dependencies require State")
	}
	registration, ok := sinks[name]
	if !ok {
		return nil, errors.New("sink is not compiled into Node Agent: " + name)
	}
	return registration.factory(dependencies)
}
