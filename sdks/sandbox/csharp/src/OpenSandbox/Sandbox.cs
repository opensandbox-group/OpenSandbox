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
using OpenSandbox.Core;
using OpenSandbox.Factory;
using OpenSandbox.Models;
using OpenSandbox.Services;
using Microsoft.Extensions.Logging;
using Microsoft.Extensions.Logging.Abstractions;
using System.Text.RegularExpressions;

namespace OpenSandbox;

/// <summary>
/// Main entry point for interacting with a sandbox.
/// </summary>
/// <remarks>
/// <see cref="DisposeAsync"/> releases local SDK resources (HTTP clients and adapters) only.
/// To terminate the remote sandbox instance, call <see cref="KillAsync"/>.
/// </remarks>
public sealed class Sandbox : IAsyncDisposable
{
    private static readonly Regex HostPathPattern = new("^(/|[A-Za-z]:[\\\\/])", RegexOptions.Compiled);

    /// <summary>
    /// Gets the sandbox ID.
    /// </summary>
    public string Id { get; }

    /// <summary>
    /// Gets the connection configuration.
    /// </summary>
    public ConnectionConfig ConnectionConfig { get; }

    /// <summary>
    /// Gets the command execution service.
    /// </summary>
    public IExecdCommands Commands { get; }

    /// <summary>
    /// Gets the filesystem service.
    /// </summary>
    public ISandboxFiles Files { get; }

    /// <summary>
    /// Gets the health check service.
    /// </summary>
    public IExecdHealth Health { get; }

    /// <summary>
    /// Gets the metrics service.
    /// </summary>
    public IExecdMetrics Metrics { get; }

    private readonly IEgress _egress;

    private readonly ISandboxes _sandboxes;
    private readonly IAdapterFactory _adapterFactory;
    private readonly string _lifecycleBaseUrl;
    private readonly string _execdBaseUrl;
    private readonly HttpClientProvider _httpClientProvider;
    private readonly ILoggerFactory _loggerFactory;
    private readonly ILogger _logger;
    private bool _disposed;

    internal HttpClientProvider SharedHttpClientProvider => _httpClientProvider;
    internal ILoggerFactory SharedLoggerFactory => _loggerFactory;

    private Sandbox(
        string id,
        ConnectionConfig connectionConfig,
        IAdapterFactory adapterFactory,
        string lifecycleBaseUrl,
        string execdBaseUrl,
        ILoggerFactory loggerFactory,
        HttpClientProvider httpClientProvider,
        ISandboxes sandboxes,
        IExecdCommands commands,
        ISandboxFiles files,
        IExecdHealth health,
        IExecdMetrics metrics,
        IEgress egress)
    {
        Id = id;
        ConnectionConfig = connectionConfig;
        _adapterFactory = adapterFactory;
        _lifecycleBaseUrl = lifecycleBaseUrl;
        _execdBaseUrl = execdBaseUrl;
        _loggerFactory = loggerFactory ?? NullLoggerFactory.Instance;
        _httpClientProvider = httpClientProvider;
        _logger = _loggerFactory.CreateLogger("OpenSandbox.Sandbox");
        _sandboxes = sandboxes;
        Commands = commands;
        Files = files;
        Health = health;
        Metrics = metrics;
        _egress = egress;
    }

    /// <summary>
    /// Creates a new sandbox.
    /// </summary>
    /// <param name="options">The creation options.</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <returns>The created sandbox.</returns>
    /// <exception cref="InvalidArgumentException">Thrown when request options are invalid.</exception>
    /// <exception cref="SandboxApiException">Thrown when the sandbox API returns an error.</exception>
    /// <exception cref="SandboxReadyTimeoutException">Thrown when readiness checks exceed timeout.</exception>
    /// <exception cref="SandboxException">Thrown when sandbox creation fails.</exception>
    public static async Task<Sandbox> CreateAsync(
        SandboxCreateOptions options,
        CancellationToken cancellationToken = default)
    {
        var connectionConfig = options.ConnectionConfig ?? new ConnectionConfig();
        var loggerFactory = options.Diagnostics?.LoggerFactory ?? NullLoggerFactory.Instance;
        var logger = loggerFactory.CreateLogger("OpenSandbox.Sandbox");
        var lifecycleBaseUrl = connectionConfig.GetBaseUrl();
        var adapterFactory = options.AdapterFactory ?? DefaultAdapterFactory.Create();
        if (string.IsNullOrWhiteSpace(options.Image) == string.IsNullOrWhiteSpace(options.SnapshotId))
        {
            throw new InvalidArgumentException("Exactly one of Image or SnapshotId must be specified.");
        }
        if (!string.IsNullOrWhiteSpace(options.SnapshotId) && options.Entrypoint is not null)
        {
            throw new InvalidArgumentException("Entrypoint must be omitted when SnapshotId is provided.");
        }
        ValidateHostPaths(options.Volumes);
        var httpClientProvider = new HttpClientProvider(connectionConfig, loggerFactory);

        ISandboxes sandboxes;
        logger.LogInformation(
            "Creating sandbox (startupSource={StartupSource}, useServerProxy={UseServerProxy})",
            options.Image ?? options.SnapshotId,
            connectionConfig.UseServerProxy);
        try
        {
            var lifecycleStack = adapterFactory.CreateLifecycleStack(new CreateLifecycleStackOptions
            {
                ConnectionConfig = connectionConfig,
                LifecycleBaseUrl = lifecycleBaseUrl,
                HttpClientProvider = httpClientProvider,
                LoggerFactory = loggerFactory
            });
            sandboxes = lifecycleStack.Sandboxes;
        }
        catch
        {
            logger.LogError("Failed to initialize lifecycle adapters while creating sandbox");
            httpClientProvider.Dispose();
            throw;
        }

        var request = new CreateSandboxRequest
        {
            Image = string.IsNullOrWhiteSpace(options.Image)
                ? null
                : new ImageSpec
                {
                    Uri = options.Image!,
                    Auth = options.ImageAuth
                },
            SnapshotId = options.SnapshotId,
            Entrypoint = string.IsNullOrWhiteSpace(options.SnapshotId)
                ? options.Entrypoint ?? Constants.DefaultEntrypoint
                : null,
            Timeout = options.ManualCleanup ? null : options.TimeoutSeconds ?? Constants.DefaultTimeoutSeconds,
            ResourceLimits = options.Resource ?? Constants.DefaultResourceLimits,
            Env = options.Env,
            SecureAccess = options.SecureAccess,
            Metadata = options.Metadata,
            Platform = options.Platform,
            NetworkPolicy = options.NetworkPolicy != null
                ? new NetworkPolicy
                {
                    DefaultAction = options.NetworkPolicy.DefaultAction ?? NetworkRuleAction.Deny,
                    Egress = options.NetworkPolicy.Egress
                }
                : null,
            Volumes = options.Volumes,
            Extensions = options.Extensions?.ToDictionary(kv => kv.Key, kv => (object)kv.Value)
        };

        string? sandboxId = null;
        try
        {
            var created = await sandboxes.CreateSandboxAsync(request, cancellationToken).ConfigureAwait(false);
            sandboxId = created.Id;
            logger.LogInformation("Sandbox created: {SandboxId}", sandboxId);

            var endpoint = await sandboxes.GetSandboxEndpointAsync(
                sandboxId,
                Constants.DefaultExecdPort,
                connectionConfig.UseServerProxy,
                cancellationToken).ConfigureAwait(false);
            var protocol = connectionConfig.Protocol == ConnectionProtocol.Https ? "https" : "http";
            var execdBaseUrl = $"{protocol}://{endpoint.EndpointAddress}";
            var execdHeaders = MergeHeaders(connectionConfig.Headers, endpoint.Headers);
            var egressEndpoint = await sandboxes.GetSandboxEndpointAsync(
                sandboxId,
                Constants.DefaultEgressPort,
                connectionConfig.UseServerProxy,
                cancellationToken).ConfigureAwait(false);
            var egressBaseUrl = $"{protocol}://{egressEndpoint.EndpointAddress}";
            var egressHeaders = MergeHeaders(connectionConfig.Headers, egressEndpoint.Headers);

            var execdStack = adapterFactory.CreateExecdStack(new CreateExecdStackOptions
            {
                ConnectionConfig = connectionConfig,
                ExecdBaseUrl = execdBaseUrl,
                ExecdHeaders = execdHeaders,
                HttpClientProvider = httpClientProvider,
                LoggerFactory = loggerFactory
            });
            var egressStack = adapterFactory.CreateEgressStack(new CreateEgressStackOptions
            {
                ConnectionConfig = connectionConfig,
                EgressBaseUrl = egressBaseUrl,
                EgressHeaders = egressHeaders,
                HttpClientProvider = httpClientProvider,
                LoggerFactory = loggerFactory
            });

            var sandbox = new Sandbox(
                sandboxId,
                connectionConfig,
                adapterFactory,
                lifecycleBaseUrl,
                execdBaseUrl,
                loggerFactory,
                httpClientProvider,
                sandboxes,
                execdStack.Commands,
                execdStack.Files,
                execdStack.Health,
                execdStack.Metrics,
                egressStack.Egress);

            if (!options.SkipHealthCheck)
            {
                logger.LogDebug("Waiting for sandbox readiness: {SandboxId}", sandboxId);
                await sandbox.WaitUntilReadyAsync(new WaitUntilReadyOptions
                {
                    ReadyTimeoutSeconds = options.ReadyTimeoutSeconds ?? Constants.DefaultReadyTimeoutSeconds,
                    PollingIntervalMillis = options.HealthCheckPollingInterval ?? Constants.DefaultHealthCheckPollingIntervalMillis,
                    HealthCheck = options.HealthCheck
                }, cancellationToken).ConfigureAwait(false);
            }

            return sandbox;
        }
        catch (Exception ex)
        {
            if (sandboxId != null)
            {
                try
                {
                    await sandboxes.DeleteSandboxAsync(sandboxId, CancellationToken.None).ConfigureAwait(false);
                }
                catch
                {
                    // Ignore cleanup failure; surface original error
                }
            }

            httpClientProvider.Dispose();
            logger.LogError(ex, "Sandbox create flow failed");
            throw;
        }
    }

    /// <summary>
    /// Connects to an existing sandbox.
    /// </summary>
    /// <param name="options">The connection options.</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <returns>The connected sandbox.</returns>
    /// <exception cref="InvalidArgumentException">Thrown when request options are invalid.</exception>
    /// <exception cref="SandboxApiException">Thrown when the sandbox API returns an error.</exception>
    /// <exception cref="SandboxReadyTimeoutException">Thrown when readiness checks exceed timeout.</exception>
    /// <exception cref="SandboxException">Thrown when sandbox connection fails.</exception>
    public static async Task<Sandbox> ConnectAsync(
        SandboxConnectOptions options,
        CancellationToken cancellationToken = default)
    {
        var connectionConfig = options.ConnectionConfig ?? new ConnectionConfig();
        var loggerFactory = options.Diagnostics?.LoggerFactory ?? NullLoggerFactory.Instance;
        var logger = loggerFactory.CreateLogger("OpenSandbox.Sandbox");
        var lifecycleBaseUrl = connectionConfig.GetBaseUrl();
        var adapterFactory = options.AdapterFactory ?? DefaultAdapterFactory.Create();
        var httpClientProvider = new HttpClientProvider(connectionConfig, loggerFactory);
        logger.LogInformation("Connecting to sandbox: {SandboxId}", options.SandboxId);

        ISandboxes sandboxes;
        try
        {
            var lifecycleStack = adapterFactory.CreateLifecycleStack(new CreateLifecycleStackOptions
            {
                ConnectionConfig = connectionConfig,
                LifecycleBaseUrl = lifecycleBaseUrl,
                HttpClientProvider = httpClientProvider,
                LoggerFactory = loggerFactory
            });
            sandboxes = lifecycleStack.Sandboxes;
        }
        catch (Exception ex)
        {
            logger.LogError(ex, "Failed to initialize lifecycle adapters while connecting sandbox");
            httpClientProvider.Dispose();
            throw;
        }

        try
        {
            var endpoint = await sandboxes.GetSandboxEndpointAsync(
                options.SandboxId,
                Constants.DefaultExecdPort,
                connectionConfig.UseServerProxy,
                cancellationToken).ConfigureAwait(false);
            var protocol = connectionConfig.Protocol == ConnectionProtocol.Https ? "https" : "http";
            var execdBaseUrl = $"{protocol}://{endpoint.EndpointAddress}";
            var execdHeaders = MergeHeaders(connectionConfig.Headers, endpoint.Headers);
            var egressEndpoint = await sandboxes.GetSandboxEndpointAsync(
                options.SandboxId,
                Constants.DefaultEgressPort,
                connectionConfig.UseServerProxy,
                cancellationToken).ConfigureAwait(false);
            var egressBaseUrl = $"{protocol}://{egressEndpoint.EndpointAddress}";
            var egressHeaders = MergeHeaders(connectionConfig.Headers, egressEndpoint.Headers);

            var execdStack = adapterFactory.CreateExecdStack(new CreateExecdStackOptions
            {
                ConnectionConfig = connectionConfig,
                ExecdBaseUrl = execdBaseUrl,
                ExecdHeaders = execdHeaders,
                HttpClientProvider = httpClientProvider,
                LoggerFactory = loggerFactory
            });
            var egressStack = adapterFactory.CreateEgressStack(new CreateEgressStackOptions
            {
                ConnectionConfig = connectionConfig,
                EgressBaseUrl = egressBaseUrl,
                EgressHeaders = egressHeaders,
                HttpClientProvider = httpClientProvider,
                LoggerFactory = loggerFactory
            });

            var sandbox = new Sandbox(
                options.SandboxId,
                connectionConfig,
                adapterFactory,
                lifecycleBaseUrl,
                execdBaseUrl,
                loggerFactory,
                httpClientProvider,
                sandboxes,
                execdStack.Commands,
                execdStack.Files,
                execdStack.Health,
                execdStack.Metrics,
                egressStack.Egress);

            if (!options.SkipHealthCheck)
            {
                await sandbox.WaitUntilReadyAsync(new WaitUntilReadyOptions
                {
                    ReadyTimeoutSeconds = options.ReadyTimeoutSeconds ?? Constants.DefaultReadyTimeoutSeconds,
                    PollingIntervalMillis = options.HealthCheckPollingInterval ?? Constants.DefaultHealthCheckPollingIntervalMillis,
                    HealthCheck = options.HealthCheck
                }, cancellationToken).ConfigureAwait(false);
            }

            return sandbox;
        }
        catch (Exception ex)
        {
            logger.LogError(ex, "Sandbox connect flow failed: {SandboxId}", options.SandboxId);
            httpClientProvider.Dispose();
            throw;
        }
    }

    /// <summary>
    /// Resumes a paused sandbox by ID.
    /// </summary>
    /// <param name="options">The connection options.</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <returns>The resumed sandbox.</returns>
    /// <exception cref="InvalidArgumentException">Thrown when request options are invalid.</exception>
    /// <exception cref="SandboxApiException">Thrown when the sandbox API returns an error.</exception>
    /// <exception cref="SandboxReadyTimeoutException">Thrown when readiness checks exceed timeout.</exception>
    /// <exception cref="SandboxException">Thrown when sandbox resume fails.</exception>
    public static async Task<Sandbox> ResumeAsync(
        SandboxConnectOptions options,
        CancellationToken cancellationToken = default)
    {
        var connectionConfig = options.ConnectionConfig ?? new ConnectionConfig();
        var loggerFactory = options.Diagnostics?.LoggerFactory ?? NullLoggerFactory.Instance;
        var logger = loggerFactory.CreateLogger("OpenSandbox.Sandbox");
        var lifecycleBaseUrl = connectionConfig.GetBaseUrl();
        var adapterFactory = options.AdapterFactory ?? DefaultAdapterFactory.Create();
        var httpClientProvider = new HttpClientProvider(connectionConfig, loggerFactory);
        logger.LogInformation("Resuming sandbox: {SandboxId}", options.SandboxId);

        try
        {
            var lifecycleStack = adapterFactory.CreateLifecycleStack(new CreateLifecycleStackOptions
            {
                ConnectionConfig = connectionConfig,
                LifecycleBaseUrl = lifecycleBaseUrl,
                HttpClientProvider = httpClientProvider,
                LoggerFactory = loggerFactory
            });

            await lifecycleStack.Sandboxes.ResumeSandboxAsync(options.SandboxId, cancellationToken).ConfigureAwait(false);
            return await ConnectAsync(options, cancellationToken).ConfigureAwait(false);
        }
        finally
        {
            httpClientProvider.Dispose();
        }
    }

    /// <summary>
    /// Gets information about this sandbox.
    /// </summary>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <returns>The sandbox information.</returns>
    /// <exception cref="SandboxApiException">Thrown when the sandbox API returns an error.</exception>
    public Task<SandboxInfo> GetInfoAsync(CancellationToken cancellationToken = default)
    {
        return _sandboxes.GetSandboxAsync(Id, cancellationToken);
    }

    /// <summary>
    /// Patches metadata for this sandbox.
    /// </summary>
    /// <param name="patch">Metadata merge patch. Non-null values add or replace keys; null values delete keys.</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <returns>The current sandbox information after applying the patch.</returns>
    /// <exception cref="SandboxApiException">Thrown when the sandbox API returns an error.</exception>
    public Task<SandboxInfo> PatchMetadataAsync(
        IReadOnlyDictionary<string, string?> patch,
        CancellationToken cancellationToken = default)
    {
        return _sandboxes.PatchSandboxMetadataAsync(Id, patch, cancellationToken);
    }

    /// <summary>
    /// Checks if the sandbox is healthy.
    /// </summary>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <returns>True if healthy, false otherwise.</returns>
    public async Task<bool> IsHealthyAsync(CancellationToken cancellationToken = default)
    {
        try
        {
            return await Health.PingAsync(cancellationToken).ConfigureAwait(false);
        }
        catch (Exception ex)
        {
            _logger.LogDebug(ex, "Health check failed for sandbox {SandboxId}", Id);
            return false;
        }
    }

    /// <summary>
    /// Gets the current resource metrics.
    /// </summary>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <returns>The sandbox metrics.</returns>
    /// <exception cref="SandboxApiException">Thrown when the sandbox API returns an error.</exception>
    public Task<SandboxMetrics> GetMetricsAsync(CancellationToken cancellationToken = default)
    {
        return Metrics.GetMetricsAsync(cancellationToken);
    }

    /// <summary>
    /// Pauses the sandbox.
    /// </summary>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <exception cref="SandboxApiException">Thrown when the sandbox API returns an error.</exception>
    public Task PauseAsync(CancellationToken cancellationToken = default)
    {
        return _sandboxes.PauseSandboxAsync(Id, cancellationToken);
    }

    /// <summary>
    /// Resumes this paused sandbox and returns a fresh, connected instance.
    /// </summary>
    /// <param name="options">Optional resume options.</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <returns>A new sandbox instance with refreshed connections.</returns>
    /// <exception cref="SandboxApiException">Thrown when the sandbox API returns an error.</exception>
    /// <exception cref="SandboxReadyTimeoutException">Thrown when readiness checks exceed timeout.</exception>
    /// <exception cref="SandboxException">Thrown when sandbox resume fails.</exception>
    public async Task<Sandbox> ResumeAsync(
        SandboxResumeOptions? options = null,
        CancellationToken cancellationToken = default)
    {
        await _sandboxes.ResumeSandboxAsync(Id, cancellationToken).ConfigureAwait(false);

        return await ConnectAsync(new SandboxConnectOptions
        {
            SandboxId = Id,
            ConnectionConfig = ConnectionConfig,
            Diagnostics = new SdkDiagnosticsOptions
            {
                LoggerFactory = _loggerFactory
            },
            AdapterFactory = _adapterFactory,
            SkipHealthCheck = options?.SkipHealthCheck ?? false,
            ReadyTimeoutSeconds = options?.ReadyTimeoutSeconds,
            HealthCheckPollingInterval = options?.HealthCheckPollingInterval
        }, cancellationToken).ConfigureAwait(false);
    }

    /// <summary>
    /// Terminates the sandbox.
    /// </summary>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <exception cref="SandboxApiException">Thrown when the sandbox API returns an error.</exception>
    public Task KillAsync(CancellationToken cancellationToken = default)
    {
        return _sandboxes.DeleteSandboxAsync(Id, cancellationToken);
    }

    /// <summary>
    /// Renews the sandbox expiration time.
    /// </summary>
    /// <param name="timeoutSeconds">The new timeout in seconds from now.</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <returns>The renewal response.</returns>
    /// <exception cref="SandboxApiException">Thrown when the sandbox API returns an error.</exception>
    public Task<RenewSandboxExpirationResponse> RenewAsync(
        int timeoutSeconds,
        CancellationToken cancellationToken = default)
    {
        var expiresAt = DateTime.UtcNow.AddSeconds(timeoutSeconds).ToString("O");
        return _sandboxes.RenewSandboxExpirationAsync(Id, new RenewSandboxExpirationRequest
        {
            ExpiresAt = expiresAt
        }, cancellationToken);
    }

    /// <summary>
    /// Creates a persistent snapshot from this sandbox.
    /// </summary>
    public Task<SnapshotInfo> CreateSnapshotAsync(
        string? name = null,
        CancellationToken cancellationToken = default)
    {
        return _sandboxes.CreateSnapshotAsync(
            Id,
            new CreateSnapshotRequest { Name = name },
            cancellationToken);
    }

    /// <summary>
    /// Gets current egress policy for this sandbox.
    /// </summary>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <returns>The current egress policy.</returns>
    public async Task<NetworkPolicy> GetEgressPolicyAsync(CancellationToken cancellationToken = default)
    {
        return await _egress.GetPolicyAsync(cancellationToken).ConfigureAwait(false);
    }

    /// <summary>
    /// Patches egress rules for this sandbox using sidecar merge semantics.
    ///
    /// Incoming rules take priority over existing rules with the same target.
    /// Existing rules for other targets remain unchanged. Within one patch payload,
    /// the first rule for a target wins. The current defaultAction is preserved.
    /// </summary>
    /// <param name="rules">Patch egress rules payload.</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    public async Task PatchEgressRulesAsync(
        IReadOnlyList<NetworkRule> rules,
        CancellationToken cancellationToken = default)
    {
        await _egress.PatchRulesAsync(rules, cancellationToken).ConfigureAwait(false);
    }

    /// <summary>
    /// Gets the endpoint for a port.
    /// </summary>
    /// <param name="port">The port number.</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <returns>The endpoint information.</returns>
    /// <exception cref="SandboxApiException">Thrown when the sandbox API returns an error.</exception>
    public Task<Endpoint> GetEndpointAsync(int port, CancellationToken cancellationToken = default)
    {
        return _sandboxes.GetSandboxEndpointAsync(Id, port, ConnectionConfig.UseServerProxy, cancellationToken);
    }

    /// <summary>
    /// Gets a signed endpoint for a port with an OSEP-0011 route token.
    /// </summary>
    /// <param name="port">The port number.</param>
    /// <param name="expires">Unix epoch seconds for the signed route token expiry.</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <returns>The endpoint information.</returns>
    /// <exception cref="SandboxApiException">Thrown when the sandbox API returns an error.</exception>
    public Task<Endpoint> GetSignedEndpointAsync(int port, long expires, CancellationToken cancellationToken = default)
    {
        return _sandboxes.GetSignedSandboxEndpointAsync(Id, port, expires, ConnectionConfig.UseServerProxy, cancellationToken);
    }

    /// <summary>
    /// Gets the endpoint URL for a port.
    /// </summary>
    /// <param name="port">The port number.</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <returns>The endpoint URL.</returns>
    /// <exception cref="SandboxApiException">Thrown when the sandbox API returns an error.</exception>
    public async Task<string> GetEndpointUrlAsync(int port, CancellationToken cancellationToken = default)
    {
        var endpoint = await GetEndpointAsync(port, cancellationToken).ConfigureAwait(false);
        var protocol = ConnectionConfig.Protocol == ConnectionProtocol.Https ? "https" : "http";
        return $"{protocol}://{endpoint.EndpointAddress}";
    }

    /// <summary>
    /// Waits until the sandbox is ready.
    /// </summary>
    /// <param name="options">The wait options.</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <exception cref="SandboxReadyTimeoutException">Thrown when readiness checks exceed timeout.</exception>
    /// <exception cref="OperationCanceledException">Thrown when <paramref name="cancellationToken"/> is canceled.</exception>
    public async Task WaitUntilReadyAsync(
        WaitUntilReadyOptions options,
        CancellationToken cancellationToken = default)
    {
        _logger.LogDebug("Start readiness check for sandbox {SandboxId} (timeoutSeconds={TimeoutSeconds})", Id, options.ReadyTimeoutSeconds);
        var deadline = DateTime.UtcNow.AddSeconds(options.ReadyTimeoutSeconds);
        var attempt = 0;
        var errorDetail = "Health check returned false continuously.";

        while (true)
        {
            cancellationToken.ThrowIfCancellationRequested();

            if (DateTime.UtcNow > deadline)
            {
                var context = $"domain={ConnectionConfig.Domain}, useServerProxy={ConnectionConfig.UseServerProxy}";
                var suggestion = "If this sandbox runs in Docker bridge or remote-network mode, consider enabling useServerProxy=true.";
                if (!ConnectionConfig.UseServerProxy)
                {
                    suggestion += " You can also configure server-side [docker].host_ip for direct endpoint access.";
                }
                throw new SandboxReadyTimeoutException(
                    $"Sandbox health check timed out after {options.ReadyTimeoutSeconds}s ({attempt} attempts). {errorDetail} Connection context: {context}. {suggestion}");
            }
            attempt++;

            try
            {
                bool isReady;
                if (options.HealthCheck != null)
                {
                    isReady = await options.HealthCheck(this).ConfigureAwait(false);
                }
                else
                {
                    isReady = await Health.PingAsync(cancellationToken).ConfigureAwait(false);
                }

                if (isReady)
                {
                    _logger.LogInformation("Sandbox is ready: {SandboxId}", Id);
                    return;
                }

                errorDetail = "Health check returned false continuously.";
            }
            catch (Exception ex)
            {
                _logger.LogDebug(ex, "Readiness probe failed for sandbox {SandboxId}", Id);
                errorDetail = $"Last health check error: {ex.Message}";
            }

            await Task.Delay(options.PollingIntervalMillis, cancellationToken).ConfigureAwait(false);
        }
    }

    /// <summary>
    /// Releases resources used by this sandbox instance.
    /// </summary>
    public ValueTask DisposeAsync()
    {
        if (_disposed)
        {
            return default;
        }

        _disposed = true;
        _logger.LogDebug("Disposing sandbox resources: {SandboxId}", Id);
        _httpClientProvider.Dispose();
        return default;
    }

    private static void ValidateHostPaths(IEnumerable<Volume>? volumes)
    {
        if (volumes == null)
        {
            return;
        }

        foreach (var volume in volumes)
        {
            var hostPath = volume.Host?.Path;
            if (hostPath != null && !HostPathPattern.IsMatch(hostPath))
            {
                throw new InvalidArgumentException(
                    "Host path must be an absolute path starting with '/' or a Windows drive letter (e.g. 'C:\\' or 'D:/')");
            }
        }
    }

    internal static IReadOnlyDictionary<string, string> MergeHeaders(
        IReadOnlyDictionary<string, string> baseHeaders,
        IReadOnlyDictionary<string, string>? overrideHeaders)
    {
        var merged = baseHeaders.ToDictionary(header => header.Key, header => header.Value);
        if (overrideHeaders != null)
        {
            foreach (var header in overrideHeaders)
            {
                merged[header.Key] = header.Value;
            }
        }

        return merged;
    }
}
