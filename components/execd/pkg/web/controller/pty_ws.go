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

package controller

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/alibaba/opensandbox/internal/safego"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"github.com/alibaba/opensandbox/execd/pkg/log"
	"github.com/alibaba/opensandbox/execd/pkg/runtime"
	"github.com/alibaba/opensandbox/execd/pkg/web/model"
)

var wsUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	// Allow all origins — execd runs behind a trusted reverse proxy.
	CheckOrigin: func(r *http.Request) bool { return true },
}

const (
	wsPingInterval  = 30 * time.Second
	wsReadDeadline  = 60 * time.Second
	wsWriteDeadline = 10 * time.Second
	// wsTakeoverTimeout bounds how long a ?takeover=1 request waits for the current
	// holder to release after being evicted, before giving up with 409.
	wsTakeoverTimeout = 5 * time.Second
	// wsTakeoverCloseTimeout bounds the best-effort close-frame write sent to an
	// evicted holder, so a full/unresponsive client socket cannot stall the takeover.
	wsTakeoverCloseTimeout = 200 * time.Millisecond
)

// PTYSessionWebSocket handles GET /pty/:sessionId/ws.
//
//  1. Look up session → 404 before upgrade if missing
//  2. Acquire WS lock without eviction → 409 if held and not a ?takeover=1 request
//  3. Upgrade HTTP → WebSocket
//     3b. Takeover (if requested): evict the holder and acquire — only now that the
//     handshake is accepted, so a failed upgrade never evicts anyone
//  4. Start the shell if not already running
//     5+6. AtomicAttachOutputWithSnapshot (snapshot + attach under outMu — no loss window)
//  7. defer: detach → pumpWg.Wait → UnlockWS → ClearEvictHandler (hook live through cleanup)
//     Register close-only eviction hook (before initial writes, so a stalled replay can
//     be interrupted without a connMu race)
//  8. Send replay frame if snapshot non-empty
//  9. Send connected frame, then upgrade hook to full evictClose+cancelOnce
//     (initial writes done; all subsequent writes serialized by connMu)
//  10. Start RFC 6455 ping, streamPump(s), exitWatcher goroutines
//  11. Read loop: dispatch client frames
func PTYSessionWebSocket(ctx *gin.Context) {
	id := ctx.Param("sessionId")
	if id == "" {
		ctx.JSON(http.StatusBadRequest, model.ErrorResponse{
			Code:    model.ErrorCodeMissingQuery,
			Message: "missing path parameter 'sessionId'",
		})
		return
	}

	// 1. Look up session — must happen before upgrade so we can return HTTP errors.
	session := codeRunner.GetPTYSession(id)
	if session == nil {
		ctx.JSON(http.StatusNotFound, model.ErrorResponse{
			Code:    model.ErrorCodeContextNotFound,
			Message: "pty session " + id + " not found",
		})
		return
	}

	// 2. Decide how to acquire the exclusive WS lock. Try without evicting first; a
	//    plain "already connected" with no takeover is refused with HTTP 409 *before*
	//    the upgrade. A ?takeover=1 request (on a real WS handshake) instead evicts the
	//    current holder — but only AFTER the handshake is fully accepted (step 3b), so a
	//    request that announces an upgrade yet fails the handshake never evicts anyone.
	locked := session.LockWS()
	wantsTakeover := !locked && ctx.Query("takeover") == "1" && websocket.IsWebSocketUpgrade(ctx.Request)
	if !locked && !wantsTakeover {
		ctx.JSON(http.StatusConflict, model.ErrorResponse{
			Code:    model.WSErrCodeAlreadyConnected,
			Message: "another client is already connected to pty session " + id,
		})
		return
	}

	// 3. Upgrade HTTP connection to WebSocket. For a takeover this happens BEFORE
	//    evicting, so a bad or incomplete handshake cannot kill the current holder.
	conn, err := wsUpgrader.Upgrade(ctx.Writer, ctx.Request, nil)
	if err != nil {
		log.Warn("pty ws upgrade failed for session %s: %v", id, err)
		if locked {
			session.UnlockWS()
		}
		return
	}

	// 3b. Takeover: the handshake succeeded, so now evict the current holder and
	//     acquire the lock. The shell keeps running; this client reattaches with replay.
	if !locked {
		if !session.TakeoverWS(wsTakeoverTimeout) {
			writeErrFrame(conn, model.WSErrCodeAlreadyConnected,
				"takeover timed out for pty session "+id)
			_ = conn.Close()
			return
		}
	}
	// From here we hold the lock; it is released at the very end of this function (see
	// defer below), only after all pump goroutines have exited.

	// Resolve query parameters.
	pipeMode := ctx.Query("pty") == "0"
	since := queryInt64(ctx.Query("since"), 0)

	// 4. Start the shell if not already running.
	if !session.IsRunning() {
		var startErr error
		if pipeMode {
			startErr = session.StartPipe()
		} else {
			startErr = session.StartPTY()
		}
		if startErr != nil {
			log.Warn("pty start failed for session %s: %v", id, startErr)
			writeErrFrame(conn, model.WSErrCodeStartFailed, startErr.Error())
			_ = conn.Close()
			session.UnlockWS()
			return
		}
	}

	// 5+6. Atomically snapshot replay buffer and attach live pipe — eliminates the
	//      output-loss window where bytes written between ReadFrom and AttachOutput
	//      would be dropped by fanout (stdoutW still nil) yet missed by snapshot.
	stdoutR, stderrR, detach, snapshotBytes, snapshotOffset := session.AttachOutputWithSnapshot(since)

	// 7. Deferred cleanup order: detach writers → wait for pump goroutines → unlock WS
	//    → clear our eviction hook. The hook is cleared LAST (after UnlockWS) so it stays
	//    live throughout cleanup: a ?takeover=1 arriving while a pump is still blocked
	//    writing to a dead client can then fire it, and closing the conn unblocks that
	//    pump so pumpWg.Wait() returns. ClearEvictHandler is generation-guarded, so it
	//    never clears a successor's hook; a zero evictGen (hook never registered, e.g. an
	//    early return below) is a no-op.
	var pumpWg sync.WaitGroup
	var evictGen uint64
	defer func() {
		detach()
		pumpWg.Wait()
		session.UnlockWS()
		session.ClearEvictHandler(evictGen)
	}()

	// cancelCh is closed to signal all goroutines to stop.
	cancelCh := make(chan struct{})
	cancelOnce := sync.OnceFunc(func() { close(cancelCh) })

	// connMu serialises all writes to conn (gorilla/websocket requires single-writer).
	var connMu sync.Mutex

	writeJSON := func(v any) error {
		connMu.Lock()
		defer connMu.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(wsWriteDeadline))
		return conn.WriteJSON(v)
	}

	closeConn := func(code int, text string) {
		connMu.Lock()
		_ = conn.SetWriteDeadline(time.Now().Add(wsWriteDeadline))
		_ = conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(code, text))
		connMu.Unlock()
		_ = conn.Close()
	}

	// evictClose is the non-blocking close used by the takeover eviction hook. Unlike
	// closeConn it must not wait behind a stalled output writer: a takeover targets
	// exactly the slow/abandoned-client case, so blocking on connMu (held by a pump
	// stuck in WriteMessage) until wsWriteDeadline would defeat wsTakeoverTimeout. It
	// therefore sends the close frame only on a best-effort basis (TryLock, short
	// deadline) and always closes the conn, which unblocks any stuck writer. A client
	// too backed-up to receive the frame could not have received it anyway.
	evictClose := func(code int, text string) {
		if connMu.TryLock() {
			_ = conn.SetWriteDeadline(time.Now().Add(wsTakeoverCloseTimeout))
			_ = conn.WriteMessage(websocket.CloseMessage,
				websocket.FormatCloseMessage(code, text))
			connMu.Unlock()
		}
		_ = conn.Close()
	}

	// Set initial read deadline; pong handler resets it.
	_ = conn.SetReadDeadline(time.Now().Add(wsReadDeadline))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(wsReadDeadline))
	})

	// Register a close-only eviction hook BEFORE the initial writes so a concurrent
	// ?takeover=1 can interrupt a holder stalled during a large replay or the connected
	// frame. At this point the initial writes run without connMu (pumps not yet started),
	// so the hook must not acquire connMu or write to conn — only close it. Closing
	// unblocks any blocked WriteMessage and makes the subsequent read loop exit, which
	// triggers the deferred cleanup and releases the lock. cancelOnce is also called so
	// all goroutines stop once the pumps do start.
	evictGen = session.SetEvictHandler(func() {
		cancelOnce()
		_ = conn.Close()
	})

	// 8. Send replay frame if there is missed output.
	if len(snapshotBytes) > 0 {
		frame := make([]byte, 1+8+len(snapshotBytes))
		frame[0] = model.BinReplay
		binary.BigEndian.PutUint64(frame[1:9], uint64(snapshotOffset))
		copy(frame[9:], snapshotBytes)
		// No connMu needed — pump goroutines not yet started.
		_ = conn.SetWriteDeadline(time.Now().Add(wsWriteDeadline))
		if err2 := conn.WriteMessage(websocket.BinaryMessage, frame); err2 != nil {
			log.Warn("pty ws send replay for session %s: %v", id, err2)
			return
		}
	}

	// 9. Send connected frame.
	mode := "pty"
	if !session.IsPTY() {
		mode = "pipe"
	}
	if err2 := writeJSON(model.ServerFrame{
		Type:      "connected",
		SessionID: id,
		Mode:      mode,
	}); err2 != nil {
		log.Warn("pty ws send connected for session %s: %v", id, err2)
		return
	}

	// Upgrade the eviction hook now that the initial writes are done and all subsequent
	// writes will be serialized by connMu (pumps + evictClose's TryLock). The full hook
	// sends the WSCloseTakenOver close frame best-effort so the client can distinguish an
	// intentional handoff from a network drop, then closes the conn and cancels goroutines.
	// Generation-tokened: a tearing-down handler never clears a successor's hook.
	evictGen = session.SetEvictHandler(func() {
		evictClose(model.WSCloseTakenOver, model.WSErrCodeTakenOver)
		cancelOnce()
	})

	// 10a. RFC 6455 binary ping goroutine (30 s interval).
	safego.Go(func() { ptyPingLoop(conn, &connMu, cancelCh, cancelOnce) })

	// 10b. Launch stdout pump.
	pumpWg.Add(1)
	safego.Go(func() {
		ptyStreamPump(stdoutR, model.BinStdout, "stdout", id, conn, &connMu, &pumpWg, cancelCh, cancelOnce)
	})

	// 10c. Launch stderr pump (pipe mode only).
	if stderrR != nil {
		pumpWg.Add(1)
		safego.Go(func() {
			ptyStreamPump(stderrR, model.BinStderr, "stderr", id, conn, &connMu, &pumpWg, cancelCh, cancelOnce)
		})
	}

	// 10d. Exit watcher: waits for the process to exit, then sends exit frame
	// and closes the WS connection immediately (unblocks ReadJSON in the read loop).
	safego.Go(func() { ptyExitWatcher(session, writeJSON, closeConn, cancelCh, cancelOnce) })

	// 11. Client read loop.
	ptyClientReadLoop(conn, session, id, writeJSON, cancelCh, cancelOnce)
}

// ptyPingLoop sends periodic WebSocket pings until cancelCh is closed.
func ptyPingLoop(conn *websocket.Conn, connMu *sync.Mutex, cancelCh <-chan struct{}, cancelOnce func()) {
	t := time.NewTicker(wsPingInterval)
	defer t.Stop()
	for {
		select {
		case <-cancelCh:
			return
		case <-t.C:
			connMu.Lock()
			_ = conn.SetWriteDeadline(time.Now().Add(wsWriteDeadline))
			pingErr := conn.WriteMessage(websocket.PingMessage, nil)
			connMu.Unlock()
			if pingErr != nil {
				cancelOnce()
				return
			}
		}
	}
}

// ptyStreamPump reads raw chunks from r and sends them as binary frames over WS.
func ptyStreamPump(r io.Reader, typeByte byte, name, id string, conn *websocket.Conn, connMu *sync.Mutex, pumpWg *sync.WaitGroup, cancelCh <-chan struct{}, cancelOnce func()) {
	defer pumpWg.Done()
	const chunkSize = 32 * 1024
	frame := make([]byte, 1+chunkSize) // single allocation for session lifetime
	frame[0] = typeByte
	for {
		select {
		case <-cancelCh:
			return
		default:
		}
		n, readErr := r.Read(frame[1:])
		if n > 0 {
			connMu.Lock()
			_ = conn.SetWriteDeadline(time.Now().Add(wsWriteDeadline))
			writeErr := conn.WriteMessage(websocket.BinaryMessage, frame[:1+n])
			connMu.Unlock()
			if writeErr != nil {
				log.Warn("pty ws write %s for session %s: %v", name, id, writeErr)
				cancelOnce()
				return
			}
		}
		if readErr != nil {
			// io.EOF or io.ErrClosedPipe when detach() closes the PipeWriter.
			return
		}
	}
}

// ptyExitWatcher waits for the session process to exit, then sends an exit frame
// and closes the WS connection.
func ptyExitWatcher(session runtime.PTYSession, writeJSON func(any) error, closeConn func(int, string), cancelCh <-chan struct{}, cancelOnce func()) {
	doneCh := session.Done()
	if doneCh == nil {
		return
	}
	select {
	case <-doneCh:
	case <-cancelCh:
		return
	}
	exitCode := session.ExitCode()
	_ = writeJSON(model.ServerFrame{
		Type:     "exit",
		ExitCode: &exitCode,
	})
	closeConn(websocket.CloseNormalClosure, "process exited")
	cancelOnce()
}

// ptyHandleBinaryMsg processes an incoming binary WebSocket frame from the client.
// Returns true if the connection should be terminated.
func ptyHandleBinaryMsg(session runtime.PTYSession, data []byte, writeJSON func(any) error, cancelOnce func()) bool {
	if len(data) == 0 {
		return false
	}
	if data[0] != model.BinStdin {
		return false // only stdin expected C→S
	}
	if _, writeErr := session.WriteStdin(data[1:]); writeErr != nil {
		_ = writeJSON(model.ServerFrame{Type: "error", Code: model.WSErrCodeStdinWriteFailed,
			Error: writeErr.Error()})
		cancelOnce()
		return true
	}
	return false
}

// ptyHandleTextMsg processes an incoming text WebSocket frame from the client.
// Returns true if the connection should be terminated.
func ptyHandleTextMsg(session runtime.PTYSession, id string, data []byte, writeJSON func(any) error, cancelOnce func()) bool {
	var frame model.ClientFrame
	if json.Unmarshal(data, &frame) != nil {
		return false
	}
	switch frame.Type {
	case "stdin":
		// wscat / debug fallback: plain UTF-8 text, no base64.
		if _, writeErr := session.WriteStdin([]byte(frame.Data)); writeErr != nil {
			_ = writeJSON(model.ServerFrame{Type: "error", Code: model.WSErrCodeStdinWriteFailed,
				Error: writeErr.Error()})
			cancelOnce()
			return true
		}
	case "signal":
		session.SendSignal(frame.Signal)
	case "resize":
		if frame.Cols > 0 && frame.Rows > 0 {
			if resErr := session.ResizePTY(uint16(frame.Cols), uint16(frame.Rows)); resErr != nil {
				log.Warn("pty resize session %s: %v", id, resErr)
			}
		}
	case "ping":
		_ = writeJSON(model.ServerFrame{Type: "pong"})
	default:
		_ = writeJSON(model.ServerFrame{Type: "error", Code: model.WSErrCodeInvalidFrame,
			Error: fmt.Sprintf("unknown frame type %q", frame.Type)})
	}
	return false
}

// ptyClientReadLoop processes incoming WebSocket messages until the connection closes.
func ptyClientReadLoop(conn *websocket.Conn, session runtime.PTYSession, id string, writeJSON func(any) error, cancelCh <-chan struct{}, cancelOnce func()) {
	for {
		select {
		case <-cancelCh:
			return
		default:
		}

		msgType, data, err := conn.ReadMessage()
		if err != nil {
			cancelOnce()
			return
		}

		// Any incoming frame resets the read deadline.
		_ = conn.SetReadDeadline(time.Now().Add(wsReadDeadline))

		switch msgType {
		case websocket.BinaryMessage:
			if ptyHandleBinaryMsg(session, data, writeJSON, cancelOnce) {
				return
			}
		case websocket.TextMessage:
			if ptyHandleTextMsg(session, id, data, writeJSON, cancelOnce) {
				return
			}
		}
	}
}

// writeErrFrame sends a JSON error frame. Safe to call before pump goroutines start.
func writeErrFrame(conn *websocket.Conn, code, message string) {
	_ = conn.SetWriteDeadline(time.Now().Add(wsWriteDeadline))
	_ = conn.WriteJSON(model.ServerFrame{
		Type:  "error",
		Error: message,
		Code:  code,
	})
}

// queryInt64 parses a decimal query string value, returning defaultVal on error.
func queryInt64(s string, defaultVal int64) int64 {
	if s == "" {
		return defaultVal
	}
	var n int64
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		return defaultVal
	}
	return n
}
