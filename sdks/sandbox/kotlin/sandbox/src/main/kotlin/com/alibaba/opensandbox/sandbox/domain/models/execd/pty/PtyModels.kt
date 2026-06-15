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

package com.alibaba.opensandbox.sandbox.domain.models.execd.pty

/**
 * A created PTY session.
 *
 * The shell is not started until the first WebSocket attaches; this only
 * identifies the server-side session.
 *
 * @property sessionId Server-assigned identifier of the PTY session
 */
class PtySession(
    val sessionId: String,
)

/**
 * Current status of a PTY session.
 *
 * @property sessionId Identifier of the PTY session
 * @property running Whether the underlying shell process is alive
 * @property outputOffset Byte offset of the buffered output; pass it as `since`
 * when reconnecting to replay scrollback from that point
 */
class PtySessionStatus(
    val sessionId: String,
    val running: Boolean,
    val outputOffset: Long,
)
