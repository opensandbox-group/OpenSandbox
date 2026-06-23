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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alibaba/opensandbox/execd/pkg/log"
)

const (
	initTimeout     = 30 * time.Second
	callTimeout     = 5 * time.Minute
	shutdownTimeout = 5 * time.Second
)

type stdioUpstream struct {
	name   string
	config UpstreamConfig

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser

	writeMu sync.Mutex
	nextID  atomic.Int64
	pending sync.Map // map[string]chan *Response — keyed by JSON-encoded ID

	tools        []Tool
	toolsFetched bool
	done         chan struct{}
}

func newStdioUpstream(config UpstreamConfig) *stdioUpstream {
	return &stdioUpstream{
		name:   config.Name,
		config: config,
		done:   make(chan struct{}),
	}
}

func (s *stdioUpstream) Name() string      { return s.name }
func (s *stdioUpstream) Transport() string { return "stdio" }

func (s *stdioUpstream) start() error {
	s.cmd = exec.Command(s.config.Command, s.config.Args...)
	setProcAttr(s.cmd)

	env := os.Environ()
	for k, v := range s.config.Env {
		env = append(env, k+"="+v)
	}
	s.cmd.Env = env

	var err error
	s.stdin, err = s.cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	s.stdout, err = s.cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	s.stderr, err = s.cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}

	if err := s.cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", s.config.Command, err)
	}

	go s.readLoop()
	go s.drainStderr()
	go s.waitLoop()

	return nil
}

func (s *stdioUpstream) readLoop() {
	scanner := bufio.NewScanner(s.stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var resp Response
		if err := json.Unmarshal(line, &resp); err != nil {
			log.Warning("mcp upstream %s: invalid JSON from stdout: %v", s.name, err)
			continue
		}

		if resp.ID == nil {
			log.Info("mcp upstream %s: notification: %s", s.name, string(line))
			continue
		}

		key := normalizeID(resp.ID)
		if ch, ok := s.pending.LoadAndDelete(key); ok {
			ch.(chan *Response) <- &resp
		} else {
			log.Warning("mcp upstream %s: unexpected response id=%v", s.name, resp.ID)
		}
	}

	if err := scanner.Err(); err != nil {
		log.Warning("mcp upstream %s: stdout read error: %v", s.name, err)
	}
}

func (s *stdioUpstream) drainStderr() {
	scanner := bufio.NewScanner(s.stderr)
	scanner.Buffer(make([]byte, 0, 4096), 1024*1024)
	for scanner.Scan() {
		log.Info("mcp upstream %s stderr: %s", s.name, scanner.Text())
	}
}

func (s *stdioUpstream) waitLoop() {
	_ = s.cmd.Wait()
	close(s.done)
}

func (s *stdioUpstream) sendRequest(ctx context.Context, method string, params any) (*Response, error) {
	id := s.nextID.Add(1)
	req := struct {
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

	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	key := normalizeID(id)
	ch := make(chan *Response, 1)
	s.pending.Store(key, ch)
	defer s.pending.Delete(key)

	s.writeMu.Lock()
	_, err = s.stdin.Write(append(data, '\n'))
	s.writeMu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("write to stdin: %w", err)
	}

	select {
	case resp := <-ch:
		return resp, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.done:
		return nil, fmt.Errorf("upstream %s process exited", s.name)
	}
}

func (s *stdioUpstream) sendNotification(method string, params any) error {
	msg := struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  any    `json:"params,omitempty"`
	}{
		JSONRPC: jsonrpcVersion,
		Method:  method,
		Params:  params,
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal notification: %w", err)
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err = s.stdin.Write(append(data, '\n'))
	return err
}

func (s *stdioUpstream) Initialize(ctx context.Context) error {
	if err := s.start(); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, initTimeout)
	defer cancel()

	params := initializeParams{
		ProtocolVersion: "2025-03-26",
		Capabilities:    map[string]any{},
		ClientInfo:      clientInfo{Name: "opensandbox-execd", Version: "1.0.0"},
	}

	resp, err := s.sendRequest(ctx, "initialize", params)
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	if resp.Error != nil {
		return fmt.Errorf("initialize error: %s", resp.Error.Message)
	}

	if err := s.sendNotification("notifications/initialized", nil); err != nil {
		return fmt.Errorf("send initialized notification: %w", err)
	}

	return nil
}

func (s *stdioUpstream) Tools(ctx context.Context) ([]Tool, error) {
	if s.toolsFetched {
		return s.tools, nil
	}

	ctx, cancel := context.WithTimeout(ctx, initTimeout)
	defer cancel()

	var allTools []Tool
	var cursor string
	for {
		params := toolsListParams{Cursor: cursor}
		resp, err := s.sendRequest(ctx, "tools/list", params)
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

	s.tools = allTools
	s.toolsFetched = true
	return s.tools, nil
}

func (s *stdioUpstream) CallTool(ctx context.Context, name string, arguments json.RawMessage) (*Response, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	params := callToolParams{
		Name:      name,
		Arguments: arguments,
	}

	resp, err := s.sendRequest(ctx, "tools/call", params)
	if err != nil {
		return nil, fmt.Errorf("tools/call %s: %w", name, err)
	}
	return resp, nil
}

func (s *stdioUpstream) Close() error {
	if s.cmd == nil || s.cmd.Process == nil {
		return nil
	}

	_ = s.stdin.Close()

	select {
	case <-s.done:
		return nil
	case <-time.After(shutdownTimeout):
	}

	forceKillProcess(s.cmd)

	<-s.done
	return nil
}

// normalizeID converts a JSON-RPC ID to a stable string key.
func normalizeID(id any) string {
	switch v := id.(type) {
	case float64:
		return strconv.FormatInt(int64(v), 10)
	case int64:
		return strconv.FormatInt(v, 10)
	case string:
		return v
	default:
		return fmt.Sprintf("%v", v)
	}
}
