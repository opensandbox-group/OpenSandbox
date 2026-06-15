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

using OpenSandbox.Core;
using OpenSandbox.Models;

namespace OpenSandbox.Services;

/// <summary>
/// Service interface for interactive PTY (pseudo-terminal) session lifecycle on the execd service.
/// </summary>
/// <remarks>
/// Manages the session lifecycle over execd's REST API (create / status / delete). Attaching to the
/// interactive <c>/pty/{sessionId}/ws</c> WebSocket stream is a separate concern. PTY is only
/// supported on Unix-like platforms.
/// </remarks>
public interface IExecdPty
{
    /// <summary>
    /// Creates a new PTY session. The shell starts on the first WebSocket attach.
    /// </summary>
    /// <param name="cwd">Optional working directory for the shell.</param>
    /// <param name="command">Optional command to run instead of the default login shell.</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <returns>The created session.</returns>
    /// <exception cref="SandboxException">Thrown when the execd service request fails.</exception>
    Task<PtySession> CreateSessionAsync(
        string? cwd = null,
        string? command = null,
        CancellationToken cancellationToken = default);

    /// <summary>
    /// Retrieves the current status of a PTY session.
    /// </summary>
    /// <param name="sessionId">Identifier of the PTY session.</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <returns>Session status, including the output offset usable for replay.</returns>
    /// <exception cref="SandboxException">Thrown when the execd service request fails.</exception>
    Task<PtySessionStatus> GetSessionAsync(string sessionId, CancellationToken cancellationToken = default);

    /// <summary>
    /// Tears down a PTY session on the server side.
    /// </summary>
    /// <param name="sessionId">Identifier of the PTY session.</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <exception cref="SandboxException">Thrown when the execd service request fails.</exception>
    Task DeleteSessionAsync(string sessionId, CancellationToken cancellationToken = default);
}
