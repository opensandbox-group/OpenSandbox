// Copyright 2025 The OpenSandbox Authors
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

package mcpproxy

import (
	"net/http"
	"sort"
	"sync"
)

// Registry holds the configured MCP bridges. It is built once at startup
// from flag/env configuration and consulted by the /mcp routes.
type Registry struct {
	mu      sync.Mutex
	closed  bool
	config  *Config
	bridges map[string]http.Handler
	order   []string
	stdio   []*stdioBridge
}

// NewRegistry builds one bridge per configured server. The config must
// already be validated (see ParseConfig).
func NewRegistry(cfg *Config) (*Registry, error) {
	r := &Registry{
		config:  cfg,
		bridges: make(map[string]http.Handler, len(cfg.Servers)),
	}
	for _, s := range cfg.Servers {
		var handler http.Handler
		var err error
		switch s.Transport {
		case TransportHTTP:
			handler, err = newHTTPBridge(s, cfg.SSEKeepalive)
		case TransportStdio:
			var bridge *stdioBridge
			bridge, err = newStdioBridge(s)
			if err == nil {
				r.stdio = append(r.stdio, bridge)
				handler = bridge
			}
		}
		if err != nil {
			r.Close()
			return nil, err
		}
		r.bridges[s.Name] = handler
		r.order = append(r.order, s.Name)
	}
	sort.Strings(r.order)
	return r, nil
}

// Empty reports whether any MCP server is configured.
func (r *Registry) Empty() bool { return r == nil || len(r.bridges) == 0 }

// Handler returns the bridge handler for a server name.
func (r *Registry) Handler(name string) (http.Handler, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, false
	}
	h, ok := r.bridges[name]
	return h, ok
}

// ServerInfo describes one configured server (for the /mcp index).
type ServerInfo struct {
	Name      string `json:"name"`
	Transport string `json:"transport"`
}

// List returns the configured servers sorted by name.
func (r *Registry) List() []ServerInfo {
	if r == nil {
		return nil
	}
	out := make([]ServerInfo, 0, len(r.config.Servers))
	for _, s := range r.config.Servers {
		out = append(out, ServerInfo{Name: s.Name, Transport: string(s.Transport)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Close terminates every stdio subprocess. HTTP bridges hold no global
// state and need no shutdown.
func (r *Registry) Close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	r.mu.Unlock()
	for _, b := range r.stdio {
		b.Close()
	}
}
