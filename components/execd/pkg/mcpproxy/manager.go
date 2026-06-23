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

package mcpproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/alibaba/opensandbox/execd/pkg/log"
)

// Manager aggregates multiple upstream MCP servers and proxies requests.
type Manager struct {
	mu        sync.RWMutex
	upstreams map[string]Upstream // name → upstream
	toolIndex map[string]string   // tool name → upstream name
	allTools  []Tool              // cached aggregated list
	sessions  map[string]*Session // Mcp-Session-Id → session
}

// NewManager creates an empty proxy manager.
func NewManager() *Manager {
	return &Manager{
		upstreams: make(map[string]Upstream),
		toolIndex: make(map[string]string),
		sessions:  make(map[string]*Session),
	}
}

// AddUpstream connects to an upstream MCP server, initializes it, fetches
// its tool list, and registers the tools. Returns error if any tool name
// conflicts with an already-registered tool.
func (m *Manager) AddUpstream(ctx context.Context, config UpstreamConfig) (*UpstreamInfo, error) {
	// Quick check under lock — reject duplicates before expensive IO.
	m.mu.RLock()
	_, exists := m.upstreams[config.Name]
	m.mu.RUnlock()
	if exists {
		return nil, fmt.Errorf("upstream %q already registered", config.Name)
	}

	var up Upstream
	switch config.Transport {
	case "stdio":
		up = newStdioUpstream(config)
	case "http":
		up = newHTTPUpstream(config)
	default:
		return nil, fmt.Errorf("unsupported transport: %s", config.Transport)
	}

	// Initialize and fetch tools outside the lock to avoid blocking
	// all proxy traffic while dialing a slow or unreachable upstream.
	if err := up.Initialize(ctx); err != nil {
		_ = up.Close()
		return nil, fmt.Errorf("initialize upstream %q: %w", config.Name, err)
	}

	tools, err := up.Tools(ctx)
	if err != nil {
		_ = up.Close()
		return nil, fmt.Errorf("fetch tools from %q: %w", config.Name, err)
	}

	// Now lock to register — re-check for races.
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.upstreams[config.Name]; exists {
		_ = up.Close()
		return nil, fmt.Errorf("upstream %q already registered", config.Name)
	}

	for _, t := range tools {
		if owner, exists := m.toolIndex[t.Name]; exists {
			_ = up.Close()
			return nil, fmt.Errorf("tool %q conflicts: already registered by upstream %q", t.Name, owner)
		}
	}

	m.upstreams[config.Name] = up
	toolNames := make([]string, 0, len(tools))
	for _, t := range tools {
		m.toolIndex[t.Name] = config.Name
		toolNames = append(toolNames, t.Name)
	}
	m.rebuildToolList()

	log.Info("mcp proxy: registered upstream %q (%s) with %d tools", config.Name, config.Transport, len(tools))

	return &UpstreamInfo{
		Name:      config.Name,
		Transport: config.Transport,
		Status:    "running",
		Tools:     toolNames,
	}, nil
}

// RemoveUpstream closes an upstream and removes its tools.
func (m *Manager) RemoveUpstream(name string) error {
	m.mu.Lock()
	up, exists := m.upstreams[name]
	if !exists {
		m.mu.Unlock()
		return fmt.Errorf("upstream %q not found", name)
	}

	delete(m.upstreams, name)
	for toolName, owner := range m.toolIndex {
		if owner == name {
			delete(m.toolIndex, toolName)
		}
	}
	m.rebuildToolList()
	m.mu.Unlock()

	// Close outside the lock — may block on process/network IO.
	if err := up.Close(); err != nil {
		log.Warning("mcp proxy: error closing upstream %q: %v", name, err)
	}

	log.Info("mcp proxy: removed upstream %q", name)
	return nil
}

// ListUpstreams returns status of all registered upstreams.
func (m *Manager) ListUpstreams() []UpstreamInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()

	infos := make([]UpstreamInfo, 0, len(m.upstreams))
	for name, up := range m.upstreams {
		var toolNames []string
		for toolName, owner := range m.toolIndex {
			if owner == name {
				toolNames = append(toolNames, toolName)
			}
		}
		infos = append(infos, UpstreamInfo{
			Name:      name,
			Transport: up.Transport(),
			Status:    "running",
			Tools:     toolNames,
		})
	}
	return infos
}

// GetUpstream returns info for a single upstream.
func (m *Manager) GetUpstream(name string) (*UpstreamInfo, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	up, exists := m.upstreams[name]
	if !exists {
		return nil, fmt.Errorf("upstream %q not found", name)
	}

	var toolNames []string
	for toolName, owner := range m.toolIndex {
		if owner == name {
			toolNames = append(toolNames, toolName)
		}
	}

	return &UpstreamInfo{
		Name:      name,
		Transport: up.Transport(),
		Status:    "running",
		Tools:     toolNames,
	}, nil
}

// HandleRequest routes a downstream MCP JSON-RPC request to the appropriate handler.
// Returns the response and optionally a new session ID (for initialize requests).
func (m *Manager) HandleRequest(ctx context.Context, req *Request, sessionID string) (*Response, string, error) {
	// JSON-RPC notifications (no id) must never receive a response.
	if req.IsNotification() {
		return nil, "", nil
	}

	switch req.Method {
	case "initialize":
		return m.handleInitialize(req)
	case "ping":
		return m.handlePing(req)
	case "tools/list":
		return m.handleToolsList(req, sessionID)
	case "tools/call":
		return m.handleToolsCall(ctx, req, sessionID)
	default:
		return newErrorResponse(req.ID, CodeMethodNotFound, fmt.Sprintf("method %q not supported", req.Method)), "", nil
	}
}

func (m *Manager) handleInitialize(req *Request) (*Response, string, error) {
	session := newSession()

	m.mu.Lock()
	m.sessions[session.ID] = session
	m.mu.Unlock()

	result := map[string]any{
		"protocolVersion": "2025-03-26",
		"capabilities": map[string]any{
			"tools": map[string]any{
				"listChanged": false,
			},
		},
		"serverInfo": map[string]any{
			"name":    "opensandbox-execd-mcp-proxy",
			"version": "1.0.0",
		},
	}

	data, _ := json.Marshal(result)
	return newResultResponse(req.ID, data), session.ID, nil
}

func (m *Manager) handlePing(req *Request) (*Response, string, error) {
	data, _ := json.Marshal(struct{}{})
	return newResultResponse(req.ID, data), "", nil
}

func (m *Manager) handleToolsList(req *Request, sessionID string) (*Response, string, error) {
	m.mu.RLock()
	tools := m.allTools
	m.mu.RUnlock()

	if tools == nil {
		tools = []Tool{}
	}

	result := toolsListResult{Tools: tools}
	data, _ := json.Marshal(result)
	return newResultResponse(req.ID, data), "", nil
}

func (m *Manager) handleToolsCall(ctx context.Context, req *Request, sessionID string) (*Response, string, error) {
	var params callToolParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return newErrorResponse(req.ID, CodeInvalidParams, "invalid tools/call params"), "", nil //nolint:nilerr // JSON-RPC error is in the response, not the Go error return
	}

	m.mu.RLock()
	upstreamName, exists := m.toolIndex[params.Name]
	var up Upstream
	if exists {
		up = m.upstreams[upstreamName]
	}
	m.mu.RUnlock()

	if !exists || up == nil {
		return newErrorResponse(req.ID, CodeMethodNotFound, fmt.Sprintf("tool %q not found", params.Name)), "", nil
	}

	resp, err := up.CallTool(ctx, params.Name, params.Arguments)
	if err != nil {
		return newErrorResponse(req.ID, CodeInternalError, err.Error()), "", nil
	}

	// Rewrite the response ID to match the downstream request.
	resp.ID = req.ID
	return resp, "", nil
}

// CreateSession creates and registers a new downstream session.
func (m *Manager) CreateSession() *Session {
	session := newSession()
	m.mu.Lock()
	m.sessions[session.ID] = session
	m.mu.Unlock()
	return session
}

// GetSession looks up a session by ID.
func (m *Manager) GetSession(id string) *Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sessions[id]
}

// DeleteSession removes a session.
func (m *Manager) DeleteSession(id string) {
	m.mu.Lock()
	delete(m.sessions, id)
	m.mu.Unlock()
}

// Close shuts down all upstreams.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for name, up := range m.upstreams {
		if err := up.Close(); err != nil {
			log.Warning("mcp proxy: error closing upstream %q: %v", name, err)
		}
	}
	m.upstreams = make(map[string]Upstream)
	m.toolIndex = make(map[string]string)
	m.allTools = nil
	m.sessions = make(map[string]*Session)
}

func (m *Manager) rebuildToolList() {
	var tools []Tool
	for _, up := range m.upstreams {
		if cached, _ := up.Tools(context.Background()); cached != nil {
			tools = append(tools, cached...)
		}
	}
	m.allTools = tools
}
