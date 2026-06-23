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
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const mcpSessionHeader = "Mcp-Session-Id"

type httpUpstream struct {
	name    string
	url     string
	headers map[string]string // custom headers sent on every request
	client  *http.Client

	mu        sync.RWMutex
	sessionID string

	nextID       atomic.Int64
	tools        []Tool
	toolsFetched bool
}

func newHTTPUpstream(config UpstreamConfig) *httpUpstream {
	return &httpUpstream{
		name:    config.Name,
		url:     config.URL,
		headers: config.Headers,
		client: &http.Client{
			Timeout: callTimeout,
		},
	}
}

func (h *httpUpstream) Name() string      { return h.name }
func (h *httpUpstream) Transport() string { return "http" }

func (h *httpUpstream) post(ctx context.Context, method string, params any) (*Response, error) {
	id := h.nextID.Add(1)
	reqBody := struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int64  `json:"id"`
		Method  string `json:"method"`
		Params  any    `json:"params,omitempty"`
	}{
		JSONRPC: jsonrpcVersion,
		ID:      id,
		Method:  method,
		Params:  params,
	}

	data, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, h.url, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range h.headers {
		httpReq.Header.Set(k, v)
	}
	h.mu.RLock()
	if h.sessionID != "" {
		httpReq.Header.Set(mcpSessionHeader, h.sessionID)
	}
	h.mu.RUnlock()

	httpResp, err := h.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http post: %w", err)
	}
	defer httpResp.Body.Close()

	if sid := httpResp.Header.Get(mcpSessionHeader); sid != "" {
		h.mu.Lock()
		h.sessionID = sid
		h.mu.Unlock()
	}

	if httpResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(httpResp.Body)
		return nil, fmt.Errorf("upstream %s returned %d: %s", h.name, httpResp.StatusCode, string(body))
	}

	ct := httpResp.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "text/event-stream") {
		return h.parseSSEResponse(httpResp.Body, id)
	}

	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	var resp Response
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}
	return &resp, nil
}

// parseSSEResponse reads an SSE stream and returns the first JSON-RPC
// response that matches the given request ID.
func (h *httpUpstream) parseSSEResponse(r io.Reader, requestID int64) (*Response, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		var resp Response
		if err := json.Unmarshal([]byte(data), &resp); err != nil {
			continue
		}
		if resp.ID != nil {
			return &resp, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read SSE stream: %w", err)
	}
	return nil, fmt.Errorf("SSE stream ended without a response for request %d", requestID)
}

func (h *httpUpstream) Initialize(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, initTimeout)
	defer cancel()

	params := initializeParams{
		ProtocolVersion: "2025-03-26",
		Capabilities:    map[string]any{},
		ClientInfo:      clientInfo{Name: "opensandbox-execd", Version: "1.0.0"},
	}

	resp, err := h.post(ctx, "initialize", params)
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	if resp.Error != nil {
		return fmt.Errorf("initialize error: %s", resp.Error.Message)
	}

	// Send initialized notification (no response expected).
	notif := struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
	}{
		JSONRPC: jsonrpcVersion,
		Method:  "notifications/initialized",
	}
	data, _ := json.Marshal(notif)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, h.url, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("initialized notification request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range h.headers {
		httpReq.Header.Set(k, v)
	}
	h.mu.RLock()
	if h.sessionID != "" {
		httpReq.Header.Set(mcpSessionHeader, h.sessionID)
	}
	h.mu.RUnlock()
	notifResp, err := h.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("initialized notification: %w", err)
	}
	notifResp.Body.Close()

	return nil
}

func (h *httpUpstream) Tools(ctx context.Context) ([]Tool, error) {
	if h.toolsFetched {
		return h.tools, nil
	}

	ctx, cancel := context.WithTimeout(ctx, initTimeout)
	defer cancel()

	var allTools []Tool
	var cursor string
	for {
		params := toolsListParams{Cursor: cursor}
		resp, err := h.post(ctx, "tools/list", params)
		if err != nil {
			return nil, fmt.Errorf("tools/list: %w", err)
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("tools/list error: %s", resp.Error.Message)
		}

		var result toolsListResult
		if err := json.Unmarshal(resp.Result, &result); err != nil {
			return nil, fmt.Errorf("parse tools/list result: %w", err)
		}
		allTools = append(allTools, result.Tools...)
		if result.NextCursor == "" {
			break
		}
		cursor = result.NextCursor
	}

	h.tools = allTools
	h.toolsFetched = true
	return h.tools, nil
}

func (h *httpUpstream) CallTool(ctx context.Context, name string, arguments json.RawMessage) (*Response, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	params := callToolParams{
		Name:      name,
		Arguments: arguments,
	}
	return h.post(ctx, "tools/call", params)
}

func (h *httpUpstream) Close() error {
	h.mu.RLock()
	sid := h.sessionID
	h.mu.RUnlock()
	if sid == "" {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, h.url, nil)
	if err != nil {
		return err
	}
	for k, v := range h.headers {
		req.Header.Set(k, v)
	}
	req.Header.Set(mcpSessionHeader, sid)

	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}
