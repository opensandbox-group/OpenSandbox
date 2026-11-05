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
	"errors"
	"net/http"
	"sort"
	"sync"
	"time"
)

// Management errors returned by Add and Remove, mapped to HTTP status codes
// by the /mcp/servers routes.
var (
	// ErrDuplicateServer rejects a name that is already registered.
	ErrDuplicateServer = errors.New("mcpproxy: duplicate server name")
	// ErrUnknownServer reports a removal of a name that is not registered.
	ErrUnknownServer = errors.New("mcpproxy: unknown server name")
	// ErrRegistryClosed reports use of a closed registry (shutdown).
	ErrRegistryClosed = errors.New("mcpproxy: registry is closed")
)

// Registry holds the MCP bridges. Servers are registered at runtime through
// the /mcp/servers management API; registrations are in-memory only — they
// vanish when execd exits.
type Registry struct {
	mu           sync.Mutex
	closed       bool
	sseKeepalive time.Duration
	bridges      map[string]http.Handler
	servers      map[string]*ServerConfig
	stdio        map[string]*stdioBridge
}

// NewRegistry returns an empty registry. The keepalive interval is applied
// to every HTTP bridge registered later (see Add).
func NewRegistry(sseKeepalive time.Duration) *Registry {
	return &Registry{
		sseKeepalive: sseKeepalive,
		bridges:      make(map[string]http.Handler),
		servers:      make(map[string]*ServerConfig),
		stdio:        make(map[string]*stdioBridge),
	}
}

// addLocked builds and registers one bridge; r.mu must be held.
func (r *Registry) addLocked(s *ServerConfig) error {
	var handler http.Handler
	var err error
	switch s.Transport {
	case TransportHTTP:
		handler, err = newHTTPBridge(s, r.sseKeepalive)
	case TransportStdio:
		var bridge *stdioBridge
		bridge, err = newStdioBridge(s)
		if err == nil {
			r.stdio[s.Name] = bridge
			handler = bridge
		}
	default:
		err = errors.New("mcpproxy: unknown transport " + string(s.Transport))
	}
	if err != nil {
		return err
	}
	r.bridges[s.Name] = handler
	r.servers[s.Name] = s
	return nil
}

// Add validates and registers one server. Registrations are in-memory only
// and are lost when execd restarts.
func (r *Registry) Add(s *ServerConfig) error {
	if r == nil {
		return ErrRegistryClosed
	}
	if err := s.Validate(); err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrRegistryClosed
	}
	if _, exists := r.servers[s.Name]; exists {
		return ErrDuplicateServer
	}
	return r.addLocked(s)
}

// Remove unregisters one server and tears down its stdio sessions.
func (r *Registry) Remove(name string) error {
	if r == nil {
		return ErrRegistryClosed
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return ErrRegistryClosed
	}
	if _, exists := r.servers[name]; !exists {
		r.mu.Unlock()
		return ErrUnknownServer
	}
	delete(r.servers, name)
	delete(r.bridges, name)
	bridge := r.stdio[name]
	delete(r.stdio, name)
	r.mu.Unlock()

	if bridge != nil {
		bridge.Close()
	}
	return nil
}

// Empty reports whether any MCP server is registered.
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

// ServerInfo describes one registered server (for the /mcp index and the
// management API).
type ServerInfo struct {
	Name      string `json:"name"`
	Transport string `json:"transport"`
}

// List returns the registered servers sorted by name.
func (r *Registry) List() []ServerInfo {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]ServerInfo, 0, len(r.servers))
	for name, s := range r.servers {
		out = append(out, ServerInfo{Name: name, Transport: string(s.Transport)})
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
	bridges := make([]*stdioBridge, 0, len(r.stdio))
	for _, b := range r.stdio {
		bridges = append(bridges, b)
	}
	r.mu.Unlock()

	for _, b := range bridges {
		b.Close()
	}
}
