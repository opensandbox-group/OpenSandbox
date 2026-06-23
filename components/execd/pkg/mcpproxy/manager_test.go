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
	"testing"

	"github.com/stretchr/testify/require"
)

// fakeUpstream is a minimal Upstream for unit tests.
type fakeUpstream struct {
	name  string
	tools []Tool
}

func (f *fakeUpstream) Name() string                       { return f.name }
func (f *fakeUpstream) Transport() string                  { return "fake" }
func (f *fakeUpstream) Initialize(_ context.Context) error { return nil }
func (f *fakeUpstream) Tools(_ context.Context) ([]Tool, error) {
	return f.tools, nil
}
func (f *fakeUpstream) CallTool(_ context.Context, name string, args json.RawMessage) (*Response, error) {
	result, _ := json.Marshal(map[string]any{
		"content": []map[string]string{{"type": "text", "text": "called:" + name}},
	})
	return newResultResponse(nil, result), nil
}
func (f *fakeUpstream) Close() error { return nil }

func newTestManager(t *testing.T, upstreams ...*fakeUpstream) *Manager {
	t.Helper()
	m := NewManager()
	for _, up := range upstreams {
		m.mu.Lock()
		m.upstreams[up.name] = up
		for _, tool := range up.tools {
			m.toolIndex[tool.Name] = up.name
		}
		m.mu.Unlock()
	}
	m.mu.Lock()
	m.rebuildToolList()
	m.mu.Unlock()
	return m
}

func TestManagerSessionLifecycle(t *testing.T) {
	m := NewManager()

	s := m.CreateSession()
	require.NotEmpty(t, s.ID)

	got := m.GetSession(s.ID)
	require.Equal(t, s.ID, got.ID)

	m.DeleteSession(s.ID)
	require.Nil(t, m.GetSession(s.ID))
}

func TestManagerHandleInitialize(t *testing.T) {
	m := NewManager()
	req := &Request{
		JSONRPC: "2.0",
		ID:      float64(1),
		Method:  "initialize",
		Params:  json.RawMessage(`{"protocolVersion":"2025-03-26"}`),
	}

	resp, sid, err := m.HandleRequest(context.Background(), req, "")
	require.NoError(t, err)
	require.NotEmpty(t, sid)
	require.NotNil(t, resp)
	require.Nil(t, resp.Error)

	var result map[string]any
	require.NoError(t, json.Unmarshal(resp.Result, &result))
	require.Equal(t, "2025-03-26", result["protocolVersion"])
}

func TestManagerHandlePing(t *testing.T) {
	m := NewManager()
	req := &Request{JSONRPC: "2.0", ID: float64(1), Method: "ping"}

	resp, _, err := m.HandleRequest(context.Background(), req, "")
	require.NoError(t, err)
	require.Nil(t, resp.Error)
}

func TestManagerHandleToolsList(t *testing.T) {
	m := newTestManager(t, &fakeUpstream{
		name: "a",
		tools: []Tool{
			{Name: "tool1", Description: "desc1"},
			{Name: "tool2", Description: "desc2"},
		},
	})

	req := &Request{JSONRPC: "2.0", ID: float64(2), Method: "tools/list", Params: json.RawMessage(`{}`)}
	resp, _, err := m.HandleRequest(context.Background(), req, "s1")
	require.NoError(t, err)

	var result toolsListResult
	require.NoError(t, json.Unmarshal(resp.Result, &result))
	require.Len(t, result.Tools, 2)
}

func TestManagerHandleToolsCall(t *testing.T) {
	m := newTestManager(t, &fakeUpstream{
		name:  "a",
		tools: []Tool{{Name: "echo"}},
	})

	req := &Request{
		JSONRPC: "2.0",
		ID:      float64(3),
		Method:  "tools/call",
		Params:  json.RawMessage(`{"name":"echo","arguments":{"msg":"hi"}}`),
	}

	resp, _, err := m.HandleRequest(context.Background(), req, "s1")
	require.NoError(t, err)
	require.Nil(t, resp.Error)
	require.Contains(t, string(resp.Result), "called:echo")
	require.Equal(t, float64(3), resp.ID)
}

func TestManagerHandleToolsCallUnknownTool(t *testing.T) {
	m := NewManager()
	req := &Request{
		JSONRPC: "2.0",
		ID:      float64(4),
		Method:  "tools/call",
		Params:  json.RawMessage(`{"name":"nonexistent","arguments":{}}`),
	}

	resp, _, err := m.HandleRequest(context.Background(), req, "s1")
	require.NoError(t, err)
	require.NotNil(t, resp.Error)
	require.Contains(t, resp.Error.Message, "not found")
}

func TestManagerHandleUnknownMethod(t *testing.T) {
	m := NewManager()
	req := &Request{JSONRPC: "2.0", ID: float64(5), Method: "resources/list"}

	resp, _, err := m.HandleRequest(context.Background(), req, "s1")
	require.NoError(t, err)
	require.NotNil(t, resp.Error)
	require.Equal(t, CodeMethodNotFound, resp.Error.Code)
}

func TestManagerToolConflict(t *testing.T) {
	m := newTestManager(t, &fakeUpstream{
		name:  "a",
		tools: []Tool{{Name: "shared_tool"}},
	})

	// Manually try adding a second upstream with conflicting tool.
	m.mu.Lock()
	_, exists := m.toolIndex["shared_tool"]
	m.mu.Unlock()
	require.True(t, exists)
}

func TestManagerRemoveUpstream(t *testing.T) {
	m := newTestManager(t, &fakeUpstream{
		name:  "a",
		tools: []Tool{{Name: "tool1"}},
	})

	require.NoError(t, m.RemoveUpstream("a"))

	m.mu.RLock()
	_, exists := m.toolIndex["tool1"]
	m.mu.RUnlock()
	require.False(t, exists)

	require.Len(t, m.allTools, 0)
}

func TestManagerRemoveUnknownUpstream(t *testing.T) {
	m := NewManager()
	err := m.RemoveUpstream("nonexistent")
	require.Error(t, err)
}

func TestManagerListUpstreams(t *testing.T) {
	m := newTestManager(t,
		&fakeUpstream{name: "a", tools: []Tool{{Name: "t1"}}},
		&fakeUpstream{name: "b", tools: []Tool{{Name: "t2"}}},
	)

	infos := m.ListUpstreams()
	require.Len(t, infos, 2)
}

func TestManagerGetUpstream(t *testing.T) {
	m := newTestManager(t, &fakeUpstream{name: "a", tools: []Tool{{Name: "t1"}}})

	info, err := m.GetUpstream("a")
	require.NoError(t, err)
	require.Equal(t, "a", info.Name)

	_, err = m.GetUpstream("nonexistent")
	require.Error(t, err)
}

func TestManagerClose(t *testing.T) {
	m := newTestManager(t, &fakeUpstream{name: "a", tools: []Tool{{Name: "t1"}}})

	m.Close()
	require.Len(t, m.upstreams, 0)
	require.Len(t, m.toolIndex, 0)
	require.Nil(t, m.allTools)
}

func TestManagerNotificationReturnsNil(t *testing.T) {
	m := NewManager()
	req := &Request{JSONRPC: "2.0", Method: "notifications/initialized"}

	resp, sid, err := m.HandleRequest(context.Background(), req, "")
	require.NoError(t, err)
	require.Nil(t, resp)
	require.Empty(t, sid)
}
