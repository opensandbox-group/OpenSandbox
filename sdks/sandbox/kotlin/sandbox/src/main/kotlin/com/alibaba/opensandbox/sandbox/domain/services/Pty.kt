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

import com.alibaba.opensandbox.sandbox.domain.models.execd.pty.PtySession
import com.alibaba.opensandbox.sandbox.domain.models.execd.pty.PtySessionStatus

/**
 * Interactive pseudo-terminal (PTY) session lifecycle for a sandbox.
 *
 * A PTY session is a long-lived shell driven over a WebSocket. This service manages the session
 * lifecycle over execd's REST API ([createSession] / [getSession] / [deleteSession]). Attaching to
 * the interactive stream (the `/pty/{sessionId}/ws` WebSocket) is a separate concern and is not
 * part of this service.
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
}
