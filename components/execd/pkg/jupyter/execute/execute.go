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
	"sync/atomic"
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

	// onConnectionLost is called when the kernel connection fails while it is
	// still in use, i.e. not after Disconnect.
	onConnectionLost func(error)
	onDisconnect     func()

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

	if c.onDisconnect != nil {
		c.onDisconnect()
		c.onDisconnect = nil
	}
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
	requestID    string
	result       *ExecutionResult
	executeDone  bool
	executeMutex sync.Mutex
	resultMutex  sync.Mutex
	cancelled    chan struct{}

	// Protected by resultMutex. An execute_result is output, not an execute_reply.
	replyReceived bool
	terminated    bool

	// chanMutex guards resultChan's close-vs-send race. The finalizer can close resultChan
	// independently of a straggling send (e.g. output from a background thread after idle).
	// Output senders take RLock for their send so the closer's Lock() cannot proceed
	// until any in-flight send completes; once closed is true, later senders skip the send
	// instead of racing a closed channel.
	chanMutex sync.RWMutex
	closed    bool

	// outputActivity counts delivered output results (stream and execute_result).
	// The reply deadline uses it to expire on output silence instead of on
	// absolute wall-clock time, so a slow relay cannot be cut off mid-delivery.
	outputActivity atomic.Int64
}

func newStreamExecutionState(startTime time.Time) *streamExecutionState {
	return &streamExecutionState{
		startTime: startTime,
		cancelled: make(chan struct{}),
		result: &ExecutionResult{
			Status:        "ok",
			Stream:        make([]*StreamOutput, 0),
			ExecutionTime: 0,
		},
	}
}

// trySend delivers notify on resultChan unless cancelled or already closed. Nonterminal
// sends use this guard; closeResultChan holds the exclusive lock for the terminal send and close.
func (state *streamExecutionState) trySend(resultChan chan *ExecutionResult, notify *ExecutionResult) {
	state.chanMutex.RLock()
	defer state.chanMutex.RUnlock()
	if state.closed {
		return
	}
	select {
	case <-state.cancelled:
		return
	default:
	}
	select {
	case resultChan <- notify:
	case <-state.cancelled:
	}
}

// closeResultChan waits for in-flight sends, sends an optional terminal result last, and closes
// resultChan. The exclusive lock prevents a straggling sender from following the terminal result.
func (state *streamExecutionState) closeResultChan(resultChan chan *ExecutionResult, terminal *ExecutionResult) {
	state.chanMutex.Lock()
	defer state.chanMutex.Unlock()
	state.closed = true
	select {
	case <-state.cancelled:
		terminal = nil
	default:
	}
	if terminal != nil {
		select {
		case resultChan <- terminal:
		case <-state.cancelled:
		}
	}
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
	state.requestID = msg.Header.MessageID

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
		c.handleExecuteReply(msg, state, resultChan)
	})
	c.registerHandler(MsgExecuteResult, func(msg *Message) {
		c.handleExecuteResult(msg, state, resultChan)
	})
	c.registerHandler(MsgStream, func(msg *Message) {
		c.handleStreamOutput(msg, state, resultChan)
	})
	c.registerHandler(MsgError, func(msg *Message) {
		c.handleExecutionError(msg, state)
	})
	c.registerHandler(MsgStatus, func(msg *Message) {
		c.handleExecutionStatus(msg, state, resultChan)
	})
	c.mu.Lock()
	c.onConnectionLost = func(err error) {
		c.failExecution(state, resultChan, ErrorOutput{
			EName:  "KernelConnectionLost",
			EValue: err.Error(),
		})
	}
	c.onDisconnect = func() {
		close(state.cancelled)
		// Let in-flight sends stop before Disconnect returns. Only the finalizer
		// closes resultChan, so cancellation cannot cause a second close.
		state.chanMutex.Lock()
		state.chanMutex.Unlock()
	}
	c.mu.Unlock()
}

func (c *Client) handleExecuteReply(msg *Message, state *streamExecutionState, resultChan chan *ExecutionResult) {
	if msg.ParentHeader.MessageID != state.requestID {
		return
	}

	var execReply ExecuteReply
	if err := json.Unmarshal(msg.Content, &execReply); err != nil {
		return
	}

	state.resultMutex.Lock()
	state.replyReceived = true
	state.result.ExecutionCount = execReply.ExecutionCount
	if execReply.Status != "ok" || execReply.EName != "" {
		errOutput := execReply.ErrorOutput
		if errOutput.EName == "" {
			errOutput.EName = "ExecutionError"
			errOutput.EValue = "kernel execution returned status: " + execReply.Status
		}
		if state.result.Error == nil {
			state.result.Error = &errOutput
		}
	}
	state.resultMutex.Unlock()

	// The reply means the kernel has finished the cell; everything after it is
	// output delivery. Finalization still waits for the matching idle, but that
	// wait must not be unbounded: if the idle is lost or relayed without the
	// matching parent header, nothing else would ever release this execution --
	// the results channel never closes, the caller's SSE stream stays open with
	// keepalive pings, and the session stays locked. Arm a bounded deadline that
	// finalizes with whatever output has been collected. It coexists with the
	// idle-triggered finalization in handleExecutionStatus; whoever flips
	// executeDone first finalizes and the loser exits. The timeout is read here,
	// on the dispatch path, so the deadline goroutine never touches the flag.
	replyDeadline := execdflag.JupyterReplyFinalizeTimeout
	safego.Go(func() { c.finalizeOnReplyDeadline(state, resultChan, replyDeadline) })
}

// finalizeOnReplyDeadline releases an execution whose matching idle status
// never arrived although execute_reply did. The kernel sends the reply after it
// finished the cell, so the outcome is already known and only the delivery of
// remaining IOPub output is pending. Finalizing here closes the results channel
// with the output collected so far; a late idle is a no-op because executeDone
// is already set.
//
// The bound expires on output silence, not on absolute wall-clock time: while
// output keeps arriving, each delivery restarts the grace period, so a slow
// relay cannot be cut off mid-delivery; the execution is finalized only once
// the output has gone quiet for a full grace period.
func (c *Client) finalizeOnReplyDeadline(state *streamExecutionState, resultChan chan *ExecutionResult, timeout time.Duration) {
	if timeout <= 0 {
		return
	}

	delivered := state.outputActivity.Load()
	for {
		timer := time.NewTimer(timeout)
		select {
		case <-state.cancelled:
			timer.Stop()
			return
		case <-timer.C:
		}
		latest := state.outputActivity.Load()
		if latest == delivered {
			break
		}
		delivered = latest
	}

	state.executeMutex.Lock()
	alreadyFinalizing := state.executeDone
	state.executeDone = true
	state.executeMutex.Unlock()
	if alreadyFinalizing {
		return
	}
	c.finalizeExecution(state, resultChan)
}

func (c *Client) handleExecuteResult(msg *Message, state *streamExecutionState, resultChan chan *ExecutionResult) {
	if msg.ParentHeader.MessageID != state.requestID {
		return
	}

	var execResult ExecuteResult
	if err := json.Unmarshal(msg.Content, &execResult); err != nil {
		return
	}

	// resultChan send happens outside resultMutex: sending can block indefinitely on a full,
	// undrained channel, and holding resultMutex across that block would starve any other
	// goroutine that needs it -- including finalizeExecution's poll loop. trySend separately
	// makes this send race-free against finalizeExecution closing resultChan.
	notify := &ExecutionResult{
		ExecutionCount: execResult.ExecutionCount,
		ExecutionData:  execResult.Data,
	}
	state.trySend(resultChan, notify)
	state.outputActivity.Add(1)

	state.resultMutex.Lock()
	state.result.ExecutionCount = execResult.ExecutionCount
	state.resultMutex.Unlock()
}

func (c *Client) handleStreamOutput(msg *Message, state *streamExecutionState, resultChan chan *ExecutionResult) {
	if msg.ParentHeader.MessageID != state.requestID {
		return
	}

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
	state.outputActivity.Add(1)
}

func (c *Client) handleExecutionError(msg *Message, state *streamExecutionState) {
	if msg.ParentHeader.MessageID != state.requestID {
		return
	}

	var errOutput ErrorOutput
	if err := json.Unmarshal(msg.Content, &errOutput); err != nil {
		return
	}

	// Keep the error until finalization so output before idle precedes the terminal event.
	state.resultMutex.Lock()
	state.result.Error = &errOutput
	state.resultMutex.Unlock()
}

func (c *Client) handleExecutionStatus(msg *Message, state *streamExecutionState, resultChan chan *ExecutionResult) {
	var status StatusUpdate
	if err := json.Unmarshal(msg.Content, &status); err != nil {
		return
	}
	switch status.ExecutionState {
	case StateIdle:
		// Startup and other requests can also emit idle. Only this request's idle
		// marks the end of its IOPub output, handled synchronously by receiveMessages.
		if msg.ParentHeader.MessageID != state.requestID {
			return
		}
	case StateRestarting, StateDead:
		// The kernel died, so this execution will never get a reply.
		c.failExecution(state, resultChan, ErrorOutput{
			EName:  "KernelDied",
			EValue: "the kernel died while executing the code (kernel state: " + string(status.ExecutionState) + ")",
		})
		return
	default:
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

// failExecution ends an execution that lost its kernel or connection. It preserves
// an outcome already received and releases finalization without waiting for missing
// protocol messages. It runs on the receive goroutine, after earlier messages.
func (c *Client) failExecution(state *streamExecutionState, resultChan chan *ExecutionResult, errOutput ErrorOutput) {
	state.resultMutex.Lock()
	if !state.replyReceived && state.result.Error == nil {
		state.result.Error = &errOutput
	}
	state.terminated = true
	state.resultMutex.Unlock()

	state.executeMutex.Lock()
	finalizing := state.executeDone
	state.executeDone = true
	state.executeMutex.Unlock()

	if !finalizing {
		c.finalizeExecution(state, resultChan)
	}
}

func (c *Client) finalizeExecution(state *streamExecutionState, resultChan chan *ExecutionResult) {
	pollInterval := execdflag.JupyterIdlePollInterval
	if pollInterval <= 0 {
		pollInterval = 100 * time.Millisecond
	}

	for {
		state.resultMutex.Lock()
		done := state.replyReceived || state.terminated
		state.resultMutex.Unlock()
		if done {
			break
		}
		select {
		case <-state.cancelled:
			state.closeResultChan(resultChan, nil)
			return
		case <-time.After(pollInterval):
		}
	}

	// Normal completion requires both the matching IOPub idle and execute_reply,
	// in either order. Kernel death/disconnect can also release this wait, preserving
	// an outcome already received. Never send while holding resultMutex.
	state.resultMutex.Lock()
	state.result.ExecutionTime = time.Since(state.startTime)
	executionTime := state.result.ExecutionTime
	errOutput := state.result.Error
	state.resultMutex.Unlock()

	var terminal *ExecutionResult
	if errOutput == nil {
		terminal = &ExecutionResult{ExecutionTime: executionTime}
	} else {
		terminal = &ExecutionResult{Error: errOutput, Status: "error"}
	}

	// closeResultChan waits for in-flight output before the terminal result and drops later output.
	state.closeResultChan(resultChan, terminal)
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
	c.onConnectionLost = nil
	c.onDisconnect = nil
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
			// After Disconnect, or a new Connect, the connection was closed on
			// purpose; otherwise tell the execution waiting on it.
			c.mu.Lock()
			var onConnectionLost func(error)
			if c.conn == conn {
				onConnectionLost = c.onConnectionLost
			}
			c.mu.Unlock()
			if onConnectionLost != nil {
				onConnectionLost(err)
			}
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
