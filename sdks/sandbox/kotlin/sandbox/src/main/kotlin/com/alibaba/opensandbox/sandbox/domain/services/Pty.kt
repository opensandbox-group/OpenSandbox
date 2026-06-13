/*
 * Copyright 2025 Alibaba Group Holding Ltd.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package com.alibaba.opensandbox.sandbox.domain.services

import com.alibaba.opensandbox.sandbox.domain.models.execd.pty.PtyMode
import com.alibaba.opensandbox.sandbox.domain.models.execd.pty.PtySession
import com.alibaba.opensandbox.sandbox.domain.models.execd.pty.PtySessionStatus
import com.alibaba.opensandbox.sandbox.domain.models.execd.pty.PtyWebSocket

/**
 * Interactive pseudo-terminal (PTY) session lifecycle for a sandbox.
 *
 * A PTY session is a long-lived shell driven over a WebSocket. This service manages the
 * session lifecycle over execd's HTTP API ([createSession] / [getSession] / [deleteSession])
 * and resolves the WebSocket target ([webSocket]) that a client connects to in order to stream
 * the interactive session. Driving the WebSocket itself (binary stdin/stdout frames, resize,
 * takeover) is left to the caller, which can use any WebSocket client.
 *
 * Explicit overloads (rather than Kotlin default arguments) are provided so the API stays
 * ergonomic from Java, where interface default arguments are not emitted as overloads.
 *
 * PTY is only supported on Unix-like platforms (Linux/macOS).
 */
interface Pty {
    /**
     * Creates a new PTY session. The shell does not start until the first WebSocket attaches.
     *
     * @param cwd Optional working directory for the shell
     * @param command Optional command to run instead of the default login shell
     * @return The created session
     */
    fun createSession(
        cwd: String?,
        command: String?,
    ): PtySession

    /** Creates a new PTY session in the given working directory with the default shell. */
    fun createSession(cwd: String?): PtySession = createSession(cwd, null)

    /** Creates a new PTY session with the default working directory and shell. */
    fun createSession(): PtySession = createSession(null, null)

    /**
     * Retrieves the current status of a PTY session.
     *
     * @param sessionId Identifier of the PTY session
     * @return Session status, including the output offset usable for replay
     */
    fun getSession(sessionId: String): PtySessionStatus

    /**
     * Tears down a PTY session on the server side.
     *
     * @param sessionId Identifier of the PTY session
     */
    fun deleteSession(sessionId: String)

    /**
     * Resolves the WebSocket target used to attach to a PTY session.
     *
     * The returned [PtyWebSocket] carries both the `ws`/`wss` URL and the routing/auth headers
     * resolved for the sandbox endpoint; callers must send those headers on the WebSocket
     * handshake (header-mode ingress and secure-access endpoints rely on them), just as the REST
     * calls in this service do.
     *
     * @param sessionId Identifier of the PTY session
     * @param mode Streaming mode ([PtyMode.PTY]; [PtyMode.PIPE] adds `pty=0`)
     * @param since Optional byte offset to replay buffered output from on reconnect
     * @param takeover When true, evict the current holder (`takeover=1`) and attach to the same shell
     * @return The WebSocket URL together with the headers to send on the handshake
     */
    fun webSocket(
        sessionId: String,
        mode: PtyMode,
        since: Long?,
        takeover: Boolean,
    ): PtyWebSocket

    /** Resolves the WebSocket target for the given mode, without replay or takeover. */
    fun webSocket(
        sessionId: String,
        mode: PtyMode,
    ): PtyWebSocket = webSocket(sessionId, mode, null, false)

    /** Resolves the WebSocket target in PTY mode, without replay or takeover. */
    fun webSocket(sessionId: String): PtyWebSocket = webSocket(sessionId, PtyMode.PTY, null, false)
}
