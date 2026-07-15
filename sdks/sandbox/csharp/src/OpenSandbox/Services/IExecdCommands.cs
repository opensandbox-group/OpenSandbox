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

using OpenSandbox.Models;
using OpenSandbox.Core;

namespace OpenSandbox.Services;

/// <summary>
/// Service interface for executing commands in a sandbox.
/// </summary>
public interface IExecdCommands
{
    /// <summary>
    /// Runs a command and streams server events (SSE).
    /// This is the lowest-level API for command execution.
    /// </summary>
    /// <param name="command">The command to run.</param>
    /// <param name="options">Optional command options.</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <returns>An async enumerable of server stream events.</returns>
    /// <exception cref="InvalidArgumentException">Thrown when request values are invalid.</exception>
    /// <exception cref="SandboxException">Thrown when the execd service request fails.</exception>
    IAsyncEnumerable<ServerStreamEvent> RunStreamAsync(
        string command,
        RunCommandOptions? options = null,
        CancellationToken cancellationToken = default);

    /// <summary>
    /// Runs a command, consumes the stream, and builds a structured execution result.
    /// </summary>
    /// <param name="command">The command to run.</param>
    /// <param name="options">Optional command options.</param>
    /// <param name="handlers">Optional event handlers for real-time processing.</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <returns>The command execution result.</returns>
    /// <exception cref="InvalidArgumentException">Thrown when request values are invalid.</exception>
    /// <exception cref="SandboxException">Thrown when the execd service request fails.</exception>
    Task<Execution> RunAsync(
        string command,
        RunCommandOptions? options = null,
        ExecutionHandlers? handlers = null,
        CancellationToken cancellationToken = default);

    /// <summary>
    /// Interrupts the current execution in the given session.
    /// </summary>
    /// <param name="sessionId">The session ID to interrupt.</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <exception cref="InvalidArgumentException">Thrown when <paramref name="sessionId"/> is null or empty.</exception>
    /// <exception cref="SandboxException">Thrown when the execd service request fails.</exception>
    Task InterruptAsync(
        string sessionId,
        CancellationToken cancellationToken = default);

    /// <summary>
    /// Gets the current running status of a command.
    /// </summary>
    /// <param name="executionId">The command execution ID.</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <returns>The command status.</returns>
    /// <exception cref="InvalidArgumentException">Thrown when <paramref name="executionId"/> is null or empty.</exception>
    /// <exception cref="SandboxException">Thrown when the execd service request fails.</exception>
    Task<CommandStatus> GetCommandStatusAsync(
        string executionId,
        CancellationToken cancellationToken = default);

    /// <summary>
    /// Gets logs for a background command.
    /// </summary>
    /// <param name="executionId">The command execution ID.</param>
    /// <param name="cursor">Optional line cursor for incremental reads.</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <returns>Background command logs and latest cursor.</returns>
    /// <exception cref="InvalidArgumentException">Thrown when <paramref name="executionId"/> is null or empty.</exception>
    /// <exception cref="SandboxException">Thrown when the execd service request fails.</exception>
    Task<CommandLogs> GetBackgroundCommandLogsAsync(
        string executionId,
        long? cursor = null,
        CancellationToken cancellationToken = default);

    /// <summary>
    /// Creates a new bash session with optional working directory.
    /// The session maintains shell state (working directory, environment) across multiple <see cref="RunInSessionAsync"/> calls.
    /// </summary>
    /// <param name="options">Optional options (e.g. initial working directory).</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <returns>The session ID for use with <see cref="RunInSessionAsync"/> and <see cref="DeleteSessionAsync"/>.</returns>
    /// <exception cref="SandboxException">Thrown when the execd service request fails.</exception>
    Task<string> CreateSessionAsync(
        CreateSessionOptions? options = null,
        CancellationToken cancellationToken = default);

    /// <summary>
    /// Runs a shell command in an existing bash session and returns the execution result (SSE consumed internally).
    /// </summary>
    /// <param name="sessionId">Session ID from <see cref="CreateSessionAsync"/>.</param>
    /// <param name="command">Shell command to execute.</param>
    /// <param name="options">Optional working directory and timeout for this run.</param>
    /// <param name="handlers">Optional event handlers for real-time processing.</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <returns>The execution result with stdout/stderr and completion status.</returns>
    /// <exception cref="InvalidArgumentException">Thrown when <paramref name="sessionId"/> or <paramref name="command"/> is null or empty.</exception>
    /// <exception cref="SandboxException">Thrown when the execd service request fails.</exception>
    Task<Execution> RunInSessionAsync(
        string sessionId,
        string command,
        RunInSessionOptions? options = null,
        ExecutionHandlers? handlers = null,
        CancellationToken cancellationToken = default);

    /// <summary>
    /// Deletes a bash session and releases resources.
    /// </summary>
    /// <param name="sessionId">Session ID to delete (from <see cref="CreateSessionAsync"/>).</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <exception cref="InvalidArgumentException">Thrown when <paramref name="sessionId"/> is null or empty.</exception>
    /// <exception cref="SandboxException">Thrown when the execd service request fails.</exception>
    Task DeleteSessionAsync(
        string sessionId,
        CancellationToken cancellationToken = default);
}
