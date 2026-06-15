// Copyright 2026 Alibaba Group Holding Ltd.
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

import type { PtySession, PtySessionStatus } from "../models/execd.js";

/**
 * Interactive PTY session lifecycle for a sandbox.
 *
 * Manages the session lifecycle over execd's REST API (create / status / delete). Attaching to
 * the interactive `/pty/{sessionId}/ws` WebSocket stream is a separate concern. PTY is only
 * supported on Unix-like platforms.
 */
export interface ExecdPty {
  /** Create a new PTY session. The shell starts on the first WebSocket attach. */
  createSession(opts?: { cwd?: string; command?: string }): Promise<PtySession>;
  /** Retrieve the current status of a PTY session. */
  getSession(sessionId: string): Promise<PtySessionStatus>;
  /** Tear down a PTY session on the server side. */
  deleteSession(sessionId: string): Promise<void>;
}
