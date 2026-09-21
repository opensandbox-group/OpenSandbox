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

// Package execute provides functionality for executing Jupyter kernel code via WebSocket
package execute

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/alibaba/opensandbox/internal/safego"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	execdflag "github.com/alibaba/opensandbox/execd/pkg/flag"
)

type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

type Client struct {
	httpClient HTTPClient

	conn *websocket.Conn

	handlers map[MessageType]func(*Message)

	session string

	msgCounter int

	mu sync.Mutex

	wsURL string
}

func NewClient(baseURL string, httpClient HTTPClient) *Client {
	return &Client{
		httpClient: httpClient,
		handlers:   make(map[MessageType]func(*Message)),
		session:    uuid.New().String(),
		msgCounter: 0,
	}
}

func (c *Client) Connect(wsURL string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.wsURL = wsURL

	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if resp != nil && err != nil {
		resp.Body.Close()
	}
	if err != nil {
		return fmt.Errorf("failed to connect to kernel: %w", err)
	}
	c.conn = conn

	c.registerDefaultHandlers()

	safego.Go(func() { c.receiveMessages() })

	return nil
}

func (c *Client) Disconnect() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
}

func (c *Client) IsConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn != nil
}

type streamExecutionState struct {
	startTime    time.Time
	result       *ExecutionResult
	executeDone  bool
	executeMutex sync.Mutex
	resultMutex  sync.Mutex

	// chanMutex guards resultChan's close-vs-send race. state.result.ExecutionCount/Error can be
	// set by handleExecuteReply with no corresponding resultChan send at all (e.g. plain print()
	// cells never emit execute_result), so finalizeExecution's poll loop can observe "done" and
	// close resultChan at any time -- independent of whether some other handler is concurrently
	// mid-send (e.g. a late stream message from a background thread that outlives the reply).
	// Senders take RLock for the duration of their send so the closer's Lock() cannot proceed
	// until any in-flight send completes; once closed is true, later senders skip the send
	// instead of racing a closed channel.
	chanMutex sync.RWMutex
	closed    bool
}

func newStreamExecutionState(startTime time.Time) *streamExecutionState {
	return &streamExecutionState{
		startTime: startTime,
		result: &ExecutionResult{
			Status:        "ok",
			Stream:        make([]*StreamOutput, 0),
			ExecutionTime: 0,
		},
	}
}

// trySend delivers notify on resultChan unless the channel has already been closed. It must be
// used for every send on resultChan so sends can never race closeResultChan.
func (state *streamExecutionState) trySend(resultChan chan *ExecutionResult, notify *ExecutionResult) {
	state.chanMutex.RLock()
	defer state.chanMutex.RUnlock()
	if state.closed {
		return
	}
	resultChan <- notify
}

// closeResultChan closes resultChan after waiting for any in-flight trySend calls to finish, and
// prevents later trySend calls from sending on the now-closed channel. Must only be called once,
// from finalizeExecution.
func (state *streamExecutionState) closeResultChan(resultChan chan *ExecutionResult) {
	state.chanMutex.Lock()
	defer state.chanMutex.Unlock()
	state.closed = true
	close(resultChan)
}

func (c *Client) ExecuteCodeStream(code string, resultChan chan *ExecutionResult) error {
	if !c.IsConnected() {
		return errors.New("not connected to kernel, please call Connect method")
	}

	msg, err := c.buildExecuteMessage(code)
	if err != nil {
		return err
	}

	state := newStreamExecutionState(time.Now())

	c.clearTemporaryHandlers()
	c.registerExecuteCodeStreamHandlers(state, resultChan)

	if err := c.writeMessage(msg); err != nil {
		return fmt.Errorf("failed to send execution request: %w", err)
	}

	return nil
}

func (c *Client) buildExecuteMessage(code string) (*Message, error) {
	msgID := c.nextMessageID()
	request := &ExecuteRequest{
		Code:            code,
		Silent:          false,
		StoreHistory:    true,
		UserExpressions: make(map[string]string),
		AllowStdin:      false,
		StopOnError:     true,
	}

	content, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize request: %w", err)
	}

	msg := &Message{
		Header: Header{
			MessageID:   msgID,
			Username:    "go-client",
			Session:     c.session,
			Date:        time.Now().Format(time.RFC3339),
			MessageType: string(MsgExecuteRequest),
			Version:     "5.3",
		},
		ParentHeader: Header{},
		Metadata:     make(map[string]interface{}),
		Content:      content,
		Channel:      "shell",
	}

	return msg, nil
}

func (c *Client) registerExecuteCodeStreamHandlers(state *streamExecutionState, resultChan chan *ExecutionResult) {
	c.registerHandler(MsgExecuteReply, func(msg *Message) {
		c.handleExecuteReply(msg, state)
	})
	c.registerHandler(MsgExecuteResult, func(msg *Message) {
		c.handleExecuteResult(msg, state, resultChan)
	})
	c.registerHandler(MsgStream, func(msg *Message) {
		c.handleStreamOutput(msg, state, resultChan)
	})
	c.registerHandler(MsgError, func(msg *Message) {
		c.handleExecutionError(msg, state, resultChan)
	})
	c.registerHandler(MsgStatus, func(msg *Message) {
		c.handleExecutionStatus(msg, state, resultChan)
	})
}

func (c *Client) handleExecuteReply(msg *Message, state *streamExecutionState) {
	var execReply ExecuteReply
	if err := json.Unmarshal(msg.Content, &execReply); err != nil {
		return
	}

	state.resultMutex.Lock()
	defer state.resultMutex.Unlock()
	state.result.ExecutionCount = execReply.ExecutionCount
	if execReply.EName != "" {
		state.result.Error = &execReply.ErrorOutput
	}
}

func (c *Client) handleExecuteResult(msg *Message, state *streamExecutionState, resultChan chan *ExecutionResult) {
	var execResult ExecuteResult
	if err := json.Unmarshal(msg.Content, &execResult); err != nil {
		return
	}

	// resultChan send happens outside resultMutex: sending can block indefinitely on a full,
	// undrained channel, and holding resultMutex across that block would starve any other
	// goroutine that needs it -- including finalizeExecution's poll loop. trySend (see
	// streamExecutionState) separately makes this send race-free against finalizeExecution
	// closing resultChan, since ExecutionCount/Error can also be set by handleExecuteReply with
	// no send of its own.
	notify := &ExecutionResult{
		ExecutionCount: execResult.ExecutionCount,
		ExecutionData:  execResult.Data,
	}
	state.trySend(resultChan, notify)

	state.resultMutex.Lock()
	state.result.ExecutionCount = execResult.ExecutionCount
	state.resultMutex.Unlock()
}

func (c *Client) handleStreamOutput(msg *Message, state *streamExecutionState, resultChan chan *ExecutionResult) {
	var stream StreamOutput
	if err := json.Unmarshal(msg.Content, &stream); err != nil {
		return
	}

	// See handleExecuteResult: resultChan send must stay outside the lock.
	state.resultMutex.Lock()
	state.result.Stream = append(state.result.Stream, &stream)
	state.resultMutex.Unlock()

	notify := &ExecutionResult{
		Stream: []*StreamOutput{&stream},
	}
	state.trySend(resultChan, notify)
}

func (c *Client) handleExecutionError(msg *Message, state *streamExecutionState, resultChan chan *ExecutionResult) {
	var errOutput ErrorOutput
	if err := json.Unmarshal(msg.Content, &errOutput); err != nil {
		return
	}

	// See handleExecuteResult: resultChan send must stay outside resultMutex, and goes through
	// trySend so it can't race finalizeExecution closing resultChan.
	notify := &ExecutionResult{
		Error:  &errOutput,
		Status: "error",
	}
	state.trySend(resultChan, notify)

	state.resultMutex.Lock()
	state.result.Status = "error"
	state.result.Error = &errOutput
	state.resultMutex.Unlock()
}

func (c *Client) handleExecutionStatus(msg *Message, state *streamExecutionState, resultChan chan *ExecutionResult) {
	var status StatusUpdate
	if err := json.Unmarshal(msg.Content, &status); err != nil {
		return
	}
	if status.ExecutionState != StateIdle {
		return
	}

	state.executeMutex.Lock()
	defer state.executeMutex.Unlock()
	if state.executeDone {
		return
	}
	state.executeDone = true
	safego.Go(func() { c.finalizeExecution(state, resultChan) })
}

func (c *Client) finalizeExecution(state *streamExecutionState, resultChan chan *ExecutionResult) {
	// See handleExecuteResult: resultChan send must stay outside resultMutex.
	state.resultMutex.Lock()
	state.result.ExecutionTime = time.Since(state.startTime)
	executionTime := state.result.ExecutionTime
	state.resultMutex.Unlock()

	notify := &ExecutionResult{
		ExecutionTime: executionTime,
	}
	state.trySend(resultChan, notify)

	pollInterval := execdflag.JupyterIdlePollInterval
	if pollInterval <= 0 {
		pollInterval = 100 * time.Millisecond
	}

	for {
		state.resultMutex.Lock()
		done := state.result.ExecutionCount > 0 || state.result.Error != nil
		state.resultMutex.Unlock()
		if done {
			break
		}
		time.Sleep(pollInterval)
	}

	// closeResultChan waits for any send from handleExecuteResult/handleStreamOutput/
	// handleExecutionError still in flight to finish before closing, and marks resultChan closed
	// so any later, straggling message for this execution (e.g. output from a background thread
	// that outlives execute_reply) is dropped by trySend instead of racing this close.
	state.closeResultChan(resultChan)
}

func (c *Client) writeMessage(msg *Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.WriteJSON(msg)
}

func (c *Client) ExecuteCodeWithCallback(code string, handler CallbackHandler) error {
	if !c.IsConnected() {
		return errors.New("not connected to kernel, please call Connect method")
	}

	msgID := c.nextMessageID()
	request := &ExecuteRequest{
		Code:            code,
		Silent:          false,
		StoreHistory:    true,
		UserExpressions: make(map[string]string),
		AllowStdin:      false,
		StopOnError:     true,
	}

	content, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("failed to serialize request: %w", err)
	}

	msg := &Message{
		Header: Header{
			MessageID:   msgID,
			Username:    "go-client",
			Session:     c.session,
			Date:        time.Now().Format(time.RFC3339),
			MessageType: string(MsgExecuteRequest),
			Version:     "5.3",
		},
		ParentHeader: Header{},
		Metadata:     make(map[string]interface{}),
		Content:      content,
		Channel:      "shell",
	}

	if handler.OnExecuteResult != nil {
		c.registerHandler(MsgExecuteResult, func(msg *Message) {
			var execResult ExecuteResult
			if err := json.Unmarshal(msg.Content, &execResult); err != nil {
				return
			}
			handler.OnExecuteResult(&execResult)
		})
	}

	if handler.OnStream != nil {
		c.registerHandler(MsgStream, func(msg *Message) {
			var stream StreamOutput
			if err := json.Unmarshal(msg.Content, &stream); err != nil {
				return
			}
			handler.OnStream(&stream)
		})
	}

	if handler.OnDisplayData != nil {
		c.registerHandler(MsgDisplayData, func(msg *Message) {
			var display DisplayData
			if err := json.Unmarshal(msg.Content, &display); err != nil {
				return
			}
			handler.OnDisplayData(&display)
		})
	}

	if handler.OnError != nil {
		c.registerHandler(MsgError, func(msg *Message) {
			var errOutput ErrorOutput
			if err := json.Unmarshal(msg.Content, &errOutput); err != nil {
				return
			}
			handler.OnError(&errOutput)
		})
	}

	if handler.OnStatus != nil {
		c.registerHandler(MsgStatus, func(msg *Message) {
			var status StatusUpdate
			if err := json.Unmarshal(msg.Content, &status); err != nil {
				return
			}
			handler.OnStatus(&status)
		})
	}

	c.mu.Lock()
	err = c.conn.WriteJSON(msg)
	c.mu.Unlock()
	if err != nil {
		return fmt.Errorf("failed to send execution request: %w", err)
	}

	return nil
}

func (c *Client) registerDefaultHandlers() {}

func (c *Client) registerHandler(msgType MessageType, handler func(*Message)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.handlers[msgType] = handler
}

func (c *Client) clearTemporaryHandlers() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.handlers = make(map[MessageType]func(*Message))
	c.registerDefaultHandlers()
}

func (c *Client) receiveMessages() {
	for {
		c.mu.Lock()
		conn := c.conn
		c.mu.Unlock()

		if conn == nil {
			break
		}

		var msg Message
		err := conn.ReadJSON(&msg)
		if err != nil {
			// connection may already be closed
			break
		}

		c.handleMessage(&msg)
	}
}

func (c *Client) handleMessage(msg *Message) {
	msgType := MessageType(msg.Header.MessageType)

	c.mu.Lock()
	handler, ok := c.handlers[msgType]
	c.mu.Unlock()

	if ok && handler != nil {
		handler(msg)
	}
}

func (c *Client) nextMessageID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgCounter++
	return fmt.Sprintf("%s-%d", c.session, c.msgCounter)
}
