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

using OpenSandbox.CodeInterpreter.Models;
using OpenSandbox.Core;
using OpenSandbox.Models;

namespace OpenSandbox.CodeInterpreter.Services;

/// <summary>
/// Service interface for code execution operations.
/// </summary>
public interface ICodes
{
    /// <summary>
    /// Creates a new code execution context for the specified language.
    /// </summary>
    /// <param name="language">The programming language (use <see cref="SupportedLanguage"/> constants).</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <returns>The created context.</returns>
    /// <exception cref="InvalidArgumentException">Thrown when <paramref name="language"/> is null or empty.</exception>
    /// <exception cref="SandboxException">Thrown when the sandbox service request fails.</exception>
    Task<CodeContext> CreateContextAsync(string language, CancellationToken cancellationToken = default);

    /// <summary>
    /// Gets an existing context by ID.
    /// </summary>
    /// <param name="contextId">The context ID.</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <returns>The context.</returns>
    /// <exception cref="InvalidArgumentException">Thrown when <paramref name="contextId"/> is null or empty.</exception>
    /// <exception cref="SandboxException">Thrown when the sandbox service request fails.</exception>
    Task<CodeContext> GetContextAsync(string contextId, CancellationToken cancellationToken = default);

    /// <summary>
    /// Lists active contexts for the specified language.
    /// </summary>
    /// <param name="language">Required language filter.</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <returns>List of contexts.</returns>
    /// <exception cref="InvalidArgumentException">Thrown when <paramref name="language"/> is null or empty.</exception>
    /// <exception cref="SandboxException">Thrown when the sandbox service request fails.</exception>
    Task<IReadOnlyList<CodeContext>> ListContextsAsync(string language, CancellationToken cancellationToken = default);

    /// <summary>
    /// Deletes a context by ID.
    /// </summary>
    /// <param name="contextId">The context ID.</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <exception cref="InvalidArgumentException">Thrown when <paramref name="contextId"/> is null or empty.</exception>
    /// <exception cref="SandboxException">Thrown when the sandbox service request fails.</exception>
    Task DeleteContextAsync(string contextId, CancellationToken cancellationToken = default);

    /// <summary>
    /// Deletes all contexts for the specified language.
    /// </summary>
    /// <param name="language">The programming language.</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <exception cref="InvalidArgumentException">Thrown when <paramref name="language"/> is null or empty.</exception>
    /// <exception cref="SandboxException">Thrown when the sandbox service request fails.</exception>
    Task DeleteContextsAsync(string language, CancellationToken cancellationToken = default);

    /// <summary>
    /// Runs code and returns the execution result.
    /// </summary>
    /// <param name="code">The code to execute.</param>
    /// <param name="options">Optional execution options.</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <returns>The execution result.</returns>
    /// <exception cref="InvalidArgumentException">Thrown when required request fields are missing.</exception>
    /// <exception cref="SandboxException">Thrown when the sandbox service request fails.</exception>
    Task<Execution> RunAsync(string code, RunCodeOptions? options = null, CancellationToken cancellationToken = default);

    /// <summary>
    /// Runs code and streams execution events.
    /// </summary>
    /// <param name="request">The run code request.</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <returns>An async enumerable of server stream events.</returns>
    /// <exception cref="InvalidArgumentException">Thrown when the request is invalid.</exception>
    /// <exception cref="SandboxException">Thrown when the sandbox service request fails.</exception>
    IAsyncEnumerable<ServerStreamEvent> RunStreamAsync(RunCodeRequest request, CancellationToken cancellationToken = default);

    /// <summary>
    /// Interrupts a running code execution.
    /// </summary>
    /// <param name="executionId">
    /// The execution ID to interrupt, typically obtained from the run result or the <c>init</c> event.
    /// </param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <exception cref="InvalidArgumentException">Thrown when <paramref name="executionId"/> is null or empty.</exception>
    /// <exception cref="SandboxException">Thrown when the sandbox service request fails.</exception>
    Task InterruptAsync(string executionId, CancellationToken cancellationToken = default);
}
