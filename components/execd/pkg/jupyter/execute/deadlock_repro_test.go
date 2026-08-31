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

// TestResultMutexNotHeldAcrossBlockingChannelSend is a minimal reproduction of a deadlock in
// handleStreamOutput (and, identically, handleExecuteResult / handleExecutionError /
// finalizeExecution): each sends into resultChan while still holding state.resultMutex.
//
// resultChan is a bounded buffer (capacity 10 in production, via runtime.runJupyterCode). If its
// consumer falls behind -- entirely realistic under bursty stdout (module imports, logging
// setup, an LLM streaming its output) compounded by node/network contention from concurrent
// sandboxes -- a handler's send blocks. Because that send happens *while holding the mutex*, it
// wedges every other goroutine that needs the same mutex, including finalizeExecution's poll
// loop, which is the only thing that ever closes resultChan and lets the request complete.
// receiveMessages() dispatches every incoming kernel message through this same synchronous path,
// so once one handler is stuck, no further message -- including the kernel's real completion
// signal -- can ever be read off the websocket again. The kernel connection is now permanently
// wedged for the rest of that execution, exactly matching the intermittent stuck-sandbox
// behavior this reproduces: the run just goes silent forever until an external timeout tears the
// pod down.
//
// This test proves the defect directly and deterministically, without a real websocket: a
// second, logically independent goroutine that only needs the mutex to check completion state
// (exactly what finalizeExecution's poll loop does) must not be blocked by a handler stuck
// mid-send on a full channel.
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
