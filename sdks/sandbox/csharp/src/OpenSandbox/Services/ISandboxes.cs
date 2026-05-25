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
/// Service interface for sandbox lifecycle management.
/// </summary>
public interface ISandboxes
{
    /// <summary>
    /// Creates a new sandbox.
    /// </summary>
    /// <param name="request">The create sandbox request.</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <returns>The created sandbox response.</returns>
    /// <exception cref="InvalidArgumentException">Thrown when request values are invalid.</exception>
    /// <exception cref="SandboxException">Thrown when the sandbox service request fails.</exception>
    Task<CreateSandboxResponse> CreateSandboxAsync(
        CreateSandboxRequest request,
        CancellationToken cancellationToken = default);

    /// <summary>
    /// Gets information about a sandbox.
    /// </summary>
    /// <param name="sandboxId">The sandbox ID.</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <returns>The sandbox information.</returns>
    /// <exception cref="InvalidArgumentException">Thrown when <paramref name="sandboxId"/> is null or empty.</exception>
    /// <exception cref="SandboxException">Thrown when the sandbox service request fails.</exception>
    Task<SandboxInfo> GetSandboxAsync(
        string sandboxId,
        CancellationToken cancellationToken = default);

    /// <summary>
    /// Lists sandboxes with optional filtering.
    /// </summary>
    /// <param name="params">Optional filter parameters.</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <returns>The list of sandboxes.</returns>
    /// <exception cref="SandboxException">Thrown when the sandbox service request fails.</exception>
    Task<ListSandboxesResponse> ListSandboxesAsync(
        ListSandboxesParams? @params = null,
        CancellationToken cancellationToken = default);

    /// <summary>
    /// Patches sandbox metadata.
    /// </summary>
    /// <param name="sandboxId">The sandbox ID.</param>
    /// <param name="patch">Metadata merge patch. Non-null values add or replace keys; null values delete keys.</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <returns>The current sandbox information after applying the patch.</returns>
    /// <exception cref="InvalidArgumentException">Thrown when <paramref name="sandboxId"/> is null or empty.</exception>
    /// <exception cref="SandboxException">Thrown when the sandbox service request fails.</exception>
    Task<SandboxInfo> PatchSandboxMetadataAsync(
        string sandboxId,
        IReadOnlyDictionary<string, string?> patch,
        CancellationToken cancellationToken = default);

    /// <summary>
    /// Deletes a sandbox.
    /// </summary>
    /// <param name="sandboxId">The sandbox ID.</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <exception cref="InvalidArgumentException">Thrown when <paramref name="sandboxId"/> is null or empty.</exception>
    /// <exception cref="SandboxException">Thrown when the sandbox service request fails.</exception>
    Task DeleteSandboxAsync(
        string sandboxId,
        CancellationToken cancellationToken = default);

    /// <summary>
    /// Pauses a sandbox.
    /// </summary>
    /// <param name="sandboxId">The sandbox ID.</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <exception cref="InvalidArgumentException">Thrown when <paramref name="sandboxId"/> is null or empty.</exception>
    /// <exception cref="SandboxException">Thrown when the sandbox service request fails.</exception>
    Task PauseSandboxAsync(
        string sandboxId,
        CancellationToken cancellationToken = default);

    /// <summary>
    /// Resumes a paused sandbox.
    /// </summary>
    /// <param name="sandboxId">The sandbox ID.</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <exception cref="InvalidArgumentException">Thrown when <paramref name="sandboxId"/> is null or empty.</exception>
    /// <exception cref="SandboxException">Thrown when the sandbox service request fails.</exception>
    Task ResumeSandboxAsync(
        string sandboxId,
        CancellationToken cancellationToken = default);

    /// <summary>
    /// Renews the expiration time of a sandbox.
    /// </summary>
    /// <param name="sandboxId">The sandbox ID.</param>
    /// <param name="request">The renewal request.</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <returns>The renewal response.</returns>
    /// <exception cref="InvalidArgumentException">Thrown when arguments are invalid.</exception>
    /// <exception cref="SandboxException">Thrown when the sandbox service request fails.</exception>
    Task<RenewSandboxExpirationResponse> RenewSandboxExpirationAsync(
        string sandboxId,
        RenewSandboxExpirationRequest request,
        CancellationToken cancellationToken = default);

    Task<SnapshotInfo> CreateSnapshotAsync(
        string sandboxId,
        CreateSnapshotRequest? request = null,
        CancellationToken cancellationToken = default);

    Task<SnapshotInfo> GetSnapshotAsync(
        string snapshotId,
        CancellationToken cancellationToken = default);

    Task<ListSnapshotsResponse> ListSnapshotsAsync(
        ListSnapshotsParams? @params = null,
        CancellationToken cancellationToken = default);

    Task DeleteSnapshotAsync(
        string snapshotId,
        CancellationToken cancellationToken = default);

    /// <summary>
    /// Gets the endpoint for a sandbox port.
    /// </summary>
    /// <param name="sandboxId">The sandbox ID.</param>
    /// <param name="port">The port number.</param>
    /// <param name="useServerProxy">Whether to return a server-proxied URL.</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <returns>The endpoint information.</returns>
    /// <exception cref="InvalidArgumentException">Thrown when arguments are invalid.</exception>
    /// <exception cref="SandboxException">Thrown when the sandbox service request fails.</exception>
    Task<Endpoint> GetSandboxEndpointAsync(
        string sandboxId,
        int port,
        bool useServerProxy = false,
        CancellationToken cancellationToken = default);

    /// <summary>
    /// Gets a signed endpoint for a sandbox port with an OSEP-0011 route token.
    /// </summary>
    /// <param name="sandboxId">The sandbox ID.</param>
    /// <param name="port">The port number.</param>
    /// <param name="expires">Unix epoch seconds for the signed route token expiry.</param>
    /// <param name="useServerProxy">Whether to return a server-proxied URL.</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <returns>The endpoint information.</returns>
    /// <exception cref="InvalidArgumentException">Thrown when arguments are invalid.</exception>
    /// <exception cref="SandboxException">Thrown when the sandbox service request fails.</exception>
    Task<Endpoint> GetSignedSandboxEndpointAsync(
        string sandboxId,
        int port,
        long expires,
        bool useServerProxy = false,
        CancellationToken cancellationToken = default);
}
