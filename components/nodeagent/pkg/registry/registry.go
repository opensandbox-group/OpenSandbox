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
	"strings"

	"github.com/alibaba/opensandbox/internal/logger"
	"github.com/alibaba/opensandbox/nodeagent/pkg/api"
	"github.com/alibaba/opensandbox/nodeagent/pkg/config"
	"github.com/alibaba/opensandbox/nodeagent/pkg/state"
	"github.com/alibaba/opensandbox/nodeagent/pkg/store"
)

// SourceState is the private persistent namespace owned by one Source.
// Source implementations cannot use it to inspect Pipeline, Sink, or another
// Source's state.
type SourceState interface {
	View(func(state.SourceStateReader) error) error
	Update(func(state.SourceStateWriter) error) error
}

type SourceDependencies struct {
	Config config.Config
	// Store is an isolated view of the node-local sandbox Pod cache. A Source
	// must call Store.Forget for each terminated Pod after it no longer needs
	// that identity; the shared Store retains the Pod until every Source does so.
	Store   store.View
	State   SourceState
	Logger  logger.Logger
	OnError func(error)
}

type SinkDependencies struct {
	Config config.Config
	State  *state.DB
}

type sourceFactory func(SourceDependencies) (api.Source, error)
type sinkFactory func(SinkDependencies) (api.Sink, error)
type sinkTargetID func(config.Config) (string, error)

type sinkRegistration struct {
	targetID sinkTargetID
	factory  sinkFactory
}

var (
	sources = make(map[string]sourceFactory)
	sinks   = make(map[string]sinkRegistration)
)

func RegisterSource(name string, factory sourceFactory) {
	if name == "" || strings.Contains(name, "/") || factory == nil {
		panic("nodeagent: invalid Source factory registration")
	}
	if _, exists := sources[name]; exists {
		panic("nodeagent: duplicate Source factory " + name)
	}
	sources[name] = factory
}

func RegisterSink(name string, targetID sinkTargetID, factory sinkFactory) {
	if name == "" || targetID == nil || factory == nil {
		panic("nodeagent: invalid Sink factory registration")
	}
	if _, exists := sinks[name]; exists {
		panic("nodeagent: duplicate Sink factory " + name)
	}
	sinks[name] = sinkRegistration{targetID: targetID, factory: factory}
}

func BuildSource(name string, dependencies SourceDependencies) (api.Source, error) {
	if dependencies.State == nil || dependencies.Store == nil || dependencies.Logger == nil {
		return nil, errors.New("source dependencies require private State, Store, and Logger")
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

func BuildSink(name string, dependencies SinkDependencies) (api.Sink, error) {
	if dependencies.State == nil {
		return nil, errors.New("sink dependencies require State")
	}
	registration, ok := sinks[name]
	if !ok {
		return nil, errors.New("sink is not compiled into Node Agent: " + name)
	}
	return registration.factory(dependencies)
}
