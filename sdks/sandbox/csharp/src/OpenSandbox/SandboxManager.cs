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

using OpenSandbox.Config;
using OpenSandbox.Factory;
using OpenSandbox.Models;
using OpenSandbox.Services;
using OpenSandbox.Core;
using Microsoft.Extensions.Logging;
using Microsoft.Extensions.Logging.Abstractions;

namespace OpenSandbox;

/// <summary>
/// Administrative interface for managing sandboxes.
/// </summary>
/// <remarks>
/// This type is intended for administrative lifecycle operations (list, inspect, pause, resume, kill, renew).
/// Dispose the manager when finished to release local SDK resources.
/// </remarks>
public sealed class SandboxManager : IAsyncDisposable
{
    private readonly ISandboxes _sandboxes;
    private readonly ConnectionConfig _connectionConfig;
    private readonly HttpClientProvider _httpClientProvider;
    private readonly ILogger _logger;
    private bool _disposed;

    private SandboxManager(
        ISandboxes sandboxes,
        ConnectionConfig connectionConfig,
        HttpClientProvider httpClientProvider,
        ILoggerFactory loggerFactory)
    {
        _sandboxes = sandboxes;
        _connectionConfig = connectionConfig;
        _httpClientProvider = httpClientProvider;
        _logger = (loggerFactory ?? NullLoggerFactory.Instance).CreateLogger("OpenSandbox.SandboxManager");
    }

    /// <summary>
    /// Creates a new sandbox manager.
    /// </summary>
    /// <param name="options">Optional configuration options.</param>
    /// <returns>A new sandbox manager instance.</returns>
    /// <exception cref="SandboxException">Thrown when manager initialization fails.</exception>
    public static SandboxManager Create(SandboxManagerOptions? options = null)
    {
        var connectionConfig = options?.ConnectionConfig ?? new ConnectionConfig();
        var lifecycleBaseUrl = connectionConfig.GetBaseUrl();
        var adapterFactory = options?.AdapterFactory ?? DefaultAdapterFactory.Create();
        var loggerFactory = options?.Diagnostics?.LoggerFactory ?? NullLoggerFactory.Instance;
        var httpClientProvider = new HttpClientProvider(connectionConfig, loggerFactory);
        var logger = loggerFactory.CreateLogger("OpenSandbox.SandboxManager");
        logger.LogInformation("Creating sandbox manager");

        try
        {
            var lifecycleStack = adapterFactory.CreateLifecycleStack(new CreateLifecycleStackOptions
            {
                ConnectionConfig = connectionConfig,
                LifecycleBaseUrl = lifecycleBaseUrl,
                HttpClientProvider = httpClientProvider,
                LoggerFactory = loggerFactory
            });

            return new SandboxManager(lifecycleStack.Sandboxes, connectionConfig, httpClientProvider, loggerFactory);
        }
        catch (Exception ex)
        {
            logger.LogError(ex, "Failed to create sandbox manager");
            httpClientProvider.Dispose();
            throw;
        }
    }

    /// <summary>
    /// Lists sandboxes with optional filtering.
    /// </summary>
    /// <param name="filter">Optional filter criteria.</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <returns>The list of sandboxes.</returns>
    /// <exception cref="SandboxApiException">Thrown when the sandbox API returns an error.</exception>
    public Task<ListSandboxesResponse> ListSandboxInfosAsync(
        SandboxFilter? filter = null,
        CancellationToken cancellationToken = default)
    {
        return _sandboxes.ListSandboxesAsync(new ListSandboxesParams
        {
            States = filter?.States,
            Metadata = filter?.Metadata,
            Page = filter?.Page,
            PageSize = filter?.PageSize
        }, cancellationToken);
    }

    /// <summary>
    /// Gets information about a specific sandbox.
    /// </summary>
    /// <param name="sandboxId">The sandbox ID.</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <returns>The sandbox information.</returns>
    /// <exception cref="InvalidArgumentException">Thrown when <paramref name="sandboxId"/> is null or empty.</exception>
    /// <exception cref="SandboxApiException">Thrown when the sandbox API returns an error.</exception>
    public Task<SandboxInfo> GetSandboxInfoAsync(
        string sandboxId,
        CancellationToken cancellationToken = default)
    {
        _logger.LogDebug("Fetching sandbox info: {SandboxId}", sandboxId);
        return _sandboxes.GetSandboxAsync(sandboxId, cancellationToken);
    }

    /// <summary>
    /// Patches metadata for a sandbox.
    /// </summary>
    /// <param name="sandboxId">The sandbox ID.</param>
    /// <param name="patch">Metadata merge patch. Non-null values add or replace keys; null values delete keys.</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <returns>The current sandbox information after applying the patch.</returns>
    /// <exception cref="InvalidArgumentException">Thrown when <paramref name="sandboxId"/> is null or empty.</exception>
    /// <exception cref="SandboxApiException">Thrown when the sandbox API returns an error.</exception>
    public Task<SandboxInfo> PatchSandboxMetadataAsync(
        string sandboxId,
        IReadOnlyDictionary<string, string?> patch,
        CancellationToken cancellationToken = default)
    {
        _logger.LogInformation("Patching sandbox metadata: {SandboxId}", sandboxId);
        return _sandboxes.PatchSandboxMetadataAsync(sandboxId, patch, cancellationToken);
    }

    /// <summary>
    /// Terminates a sandbox.
    /// </summary>
    /// <param name="sandboxId">The sandbox ID.</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <exception cref="InvalidArgumentException">Thrown when <paramref name="sandboxId"/> is null or empty.</exception>
    /// <exception cref="SandboxApiException">Thrown when the sandbox API returns an error.</exception>
    public Task KillSandboxAsync(
        string sandboxId,
        CancellationToken cancellationToken = default)
    {
        _logger.LogInformation("Killing sandbox: {SandboxId}", sandboxId);
        return _sandboxes.DeleteSandboxAsync(sandboxId, cancellationToken);
    }

    /// <summary>
    /// Pauses a sandbox.
    /// </summary>
    /// <param name="sandboxId">The sandbox ID.</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <exception cref="InvalidArgumentException">Thrown when <paramref name="sandboxId"/> is null or empty.</exception>
    /// <exception cref="SandboxApiException">Thrown when the sandbox API returns an error.</exception>
    public Task PauseSandboxAsync(
        string sandboxId,
        CancellationToken cancellationToken = default)
    {
        _logger.LogInformation("Pausing sandbox: {SandboxId}", sandboxId);
        return _sandboxes.PauseSandboxAsync(sandboxId, cancellationToken);
    }

    /// <summary>
    /// Resumes a paused sandbox.
    /// </summary>
    /// <param name="sandboxId">The sandbox ID.</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <exception cref="InvalidArgumentException">Thrown when <paramref name="sandboxId"/> is null or empty.</exception>
    /// <exception cref="SandboxApiException">Thrown when the sandbox API returns an error.</exception>
    public Task ResumeSandboxAsync(
        string sandboxId,
        CancellationToken cancellationToken = default)
    {
        _logger.LogInformation("Resuming sandbox: {SandboxId}", sandboxId);
        return _sandboxes.ResumeSandboxAsync(sandboxId, cancellationToken);
    }

    /// <summary>
    /// Renews the expiration time of a sandbox.
    /// </summary>
    /// <param name="sandboxId">The sandbox ID.</param>
    /// <param name="timeoutSeconds">The new timeout in seconds from now.</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <exception cref="InvalidArgumentException">Thrown when arguments are invalid.</exception>
    /// <exception cref="SandboxApiException">Thrown when the sandbox API returns an error.</exception>
    public async Task RenewSandboxAsync(
        string sandboxId,
        int timeoutSeconds,
        CancellationToken cancellationToken = default)
    {
        _logger.LogInformation("Renewing sandbox expiration: {SandboxId} (timeoutSeconds={TimeoutSeconds})", sandboxId, timeoutSeconds);
        var expiresAt = DateTime.UtcNow.AddSeconds(timeoutSeconds).ToString("O");
        await _sandboxes.RenewSandboxExpirationAsync(sandboxId, new RenewSandboxExpirationRequest
        {
            ExpiresAt = expiresAt
        }, cancellationToken).ConfigureAwait(false);
    }

    public Task<SnapshotInfo> CreateSnapshotAsync(
        string sandboxId,
        string? name = null,
        CancellationToken cancellationToken = default)
    {
        return _sandboxes.CreateSnapshotAsync(
            sandboxId,
            new CreateSnapshotRequest { Name = name },
            cancellationToken);
    }

    public Task<SnapshotInfo> GetSnapshotAsync(
        string snapshotId,
        CancellationToken cancellationToken = default)
    {
        return _sandboxes.GetSnapshotAsync(snapshotId, cancellationToken);
    }

    public Task<ListSnapshotsResponse> ListSnapshotsAsync(
        ListSnapshotsParams? filter = null,
        CancellationToken cancellationToken = default)
    {
        return _sandboxes.ListSnapshotsAsync(filter, cancellationToken);
    }

    public Task DeleteSnapshotAsync(
        string snapshotId,
        CancellationToken cancellationToken = default)
    {
        return _sandboxes.DeleteSnapshotAsync(snapshotId, cancellationToken);
    }

    /// <summary>
    /// Releases resources used by this manager.
    /// </summary>
    public ValueTask DisposeAsync()
    {
        if (_disposed)
        {
            return default;
        }

        _disposed = true;
        _logger.LogDebug("Disposing sandbox manager resources");
        _httpClientProvider.Dispose();
        return default;
    }
}
