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

package execute

import (
	"encoding/json"
	"testing"
	"time"
)

// TestResultMutexNotHeldAcrossBlockingChannelSend reproduces a former deadlock: the result
// handlers used to send into resultChan while holding state.resultMutex, so once the bounded
// channel filled, the blocked send starved finalizeExecution's poll loop -- the only thing that
// ever closes resultChan -- and since receiveMessages dispatches every kernel message
// synchronously through the same path, the kernel's completion signal could never be read and
// the execution hung until an external timeout.
//
// This test asserts the invariant directly: a goroutine needing only resultMutex (exactly what
// finalizeExecution's poll loop does) must not be starved while a handler blocks mid-send on a
// full channel.
func TestResultMutexNotHeldAcrossBlockingChannelSend(t *testing.T) {
	c := &Client{handlers: make(map[MessageType]func(*Message))}
	state := newStreamExecutionState(time.Now())

	// Capacity 1 makes the repro deterministic with a single extra send, instead of needing to
	// race 10+ messages against a websocket.
	resultChan := make(chan *ExecutionResult, 1)

	content, err := json.Marshal(StreamOutput{Name: StreamStdout, Text: "line"})
	if err != nil {
		t.Fatalf("failed to marshal stream output: %v", err)
	}
	msg := &Message{Content: json.RawMessage(content)}

	// Fill the only buffer slot -- this call returns immediately.
	c.handleStreamOutput(msg, state, resultChan)

	// Second call has nowhere to send: resultChan is full and nothing is draining it (simulating
	// a consumer that has fallen behind, e.g. runJupyterCode's HTTP relay stalling under load).
	// Under the bug this call never returns, so it must run in its own goroutine.
	go func() {
		c.handleStreamOutput(msg, state, resultChan)
	}()

	// Give the goroutine above time to reach (and block on) the channel send.
	time.Sleep(50 * time.Millisecond)

	// A logically unrelated operation that only needs resultMutex -- exactly what
	// finalizeExecution's poll loop does when deciding whether to close resultChan.
	lockAcquired := make(chan struct{})
	go func() {
		state.resultMutex.Lock()
		state.resultMutex.Unlock() //nolint:staticcheck // immediately released; only testing acquisition
		close(lockAcquired)
	}()

	select {
	case <-lockAcquired:
		// Fixed behavior: the mutex was released before/without the blocking send, so an
		// unrelated goroutine (standing in for finalizeExecution's completion check) was not
		// starved by a stalled consumer.
	case <-time.After(500 * time.Millisecond):
		t.Fatal("resultMutex was still held while handleStreamOutput was blocked sending on a " +
			"full, undrained channel -- a stalled consumer wedges the whole kernel connection's " +
			"reader goroutine (receiveMessages), permanently hanging the execution. See " +
			"handleStreamOutput / handleExecuteResult / handleExecutionError / finalizeExecution " +
			"in execute.go: each must release state.resultMutex before sending on resultChan.")
	}

	// Draining the channel unblocks the earlier goroutine either way; clean up so the test
	// doesn't leak a goroutine on failure.
	<-resultChan
}
