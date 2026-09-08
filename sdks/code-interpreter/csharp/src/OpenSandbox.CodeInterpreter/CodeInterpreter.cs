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

using System.Diagnostics;
using OpenSandbox.CodeInterpreter.Factory;
using OpenSandbox.CodeInterpreter.Services;
using OpenSandbox.Config;
using OpenSandbox.Core;
using OpenSandbox.Internal;
using OpenSandbox.Services;
using Microsoft.Extensions.Logging;
using Microsoft.Extensions.Logging.Abstractions;

namespace OpenSandbox.CodeInterpreter;

/// <summary>
/// Strict health check helpers for code interpreters.
/// </summary>
public static class CodeInterpreterHealthCheck
{
    /// <summary>
    /// The shell command run inside the sandbox to verify the interpreter runtime
    /// (Jupyter kernel gateway) is actually serving. execd starts serving
    /// <c>/ping</c> before the entrypoint launches Jupyter, and the setup stage may
    /// run short-lived "jupyter kernelspec" helpers, so a daemon ping or a
    /// process-name grep cannot prove the runtime is ready. Probing the Jupyter
    /// listen port (127.0.0.1:${JUPYTER_PORT:-44771}, same default as the entrypoint)
    /// only passes once the server accepts connections.
    /// </summary>
    public const string RuntimeCheckCommand =
        "bash -c 'exec 3<>/dev/tcp/127.0.0.1/${JUPYTER_PORT:-44771}' && exit 0 || exit 1";
}

/// <summary>
/// Options for creating a code interpreter.
/// </summary>
public class CodeInterpreterCreateOptions
{
    /// <summary>
    /// Gets or sets the adapter factory. If not provided, a default factory is used.
    /// </summary>
    public ICodeInterpreterAdapterFactory? AdapterFactory { get; set; }

    /// <summary>
    /// Gets or sets diagnostics options such as logging.
    /// </summary>
    public SdkDiagnosticsOptions? Diagnostics { get; set; }

    /// <summary>
    /// Gets or sets whether to skip the strict code-executor readiness check.
    /// The returned interpreter may fail on first use if the execd daemon is
    /// not serving yet.
    /// </summary>
    public bool SkipHealthCheck { get; set; }

    /// <summary>
    /// Gets or sets the timeout for the code execution service health check, in seconds.
    /// </summary>
    public int? ReadyTimeoutSeconds { get; set; }

    /// <summary>
    /// Gets or sets the health check polling interval, in milliseconds.
    /// </summary>
    public int? HealthCheckPollingInterval { get; set; }
}

/// <summary>
/// Options for waiting until a code interpreter is ready.
/// </summary>
public class CodeInterpreterWaitUntilReadyOptions
{
    /// <summary>
    /// Gets or sets the timeout in seconds.
    /// </summary>
    public int ReadyTimeoutSeconds { get; set; }

    /// <summary>
    /// Gets or sets the polling interval in milliseconds.
    /// </summary>
    public int PollingIntervalMillis { get; set; }
}

/// <summary>
/// Code interpreter facade for executing code in multiple languages.
/// </summary>
/// <remarks>
/// This class wraps an existing <see cref="Sandbox"/> and provides a high-level API for code execution.
/// Use <see cref="Codes"/> to create contexts and run code.
/// <see cref="Files"/>, <see cref="Commands"/>, and <see cref="Metrics"/> are exposed for convenience
/// and are the same instances as on the underlying <see cref="Sandbox"/>.
/// This type does not own the remote sandbox lifecycle. Call <see cref="Sandbox.KillAsync"/> when you want to terminate
/// the remote instance. Dispose the wrapped <see cref="Sandbox"/> to release local SDK resources.
/// </remarks>
public sealed class CodeInterpreter
{
    /// <summary>
    /// Gets the underlying sandbox instance.
    /// </summary>
    public Sandbox Sandbox { get; }

    /// <summary>
    /// Gets the codes service for code execution operations.
    /// </summary>
    public ICodes Codes { get; }

    /// <summary>
    /// Gets the sandbox ID.
    /// </summary>
    public string Id => Sandbox.Id;

    /// <summary>
    /// Gets the filesystem service.
    /// </summary>
    public ISandboxFiles Files => Sandbox.Files;

    /// <summary>
    /// Gets the command execution service.
    /// </summary>
    public IExecdCommands Commands => Sandbox.Commands;

    /// <summary>
    /// Gets the metrics service.
    /// </summary>
    public IExecdMetrics Metrics => Sandbox.Metrics;

    private readonly ILogger _logger;
    private readonly HttpClientWrapper? _fallbackExecdClient;

    private CodeInterpreter(Sandbox sandbox, ICodes codes, ILogger logger, HttpClientWrapper? fallbackExecdClient = null)
    {
        Sandbox = sandbox ?? throw new ArgumentNullException(nameof(sandbox));
        Codes = codes ?? throw new ArgumentNullException(nameof(codes));
        _logger = logger ?? throw new ArgumentNullException(nameof(logger));
        _fallbackExecdClient = fallbackExecdClient;
        _logger.LogDebug("Code interpreter initialized for sandbox: {SandboxId}", sandbox.Id);
    }

    /// <summary>
    /// Creates a new code interpreter from an existing sandbox.
    /// </summary>
    /// <remarks>
    /// By default a strict health check runs before the interpreter is returned: the code
    /// execution service (execd) must answer <c>GET /ping</c> AND the code interpreter
    /// runtime (Jupyter kernel gateway) must be serving, both within
    /// <see cref="CodeInterpreterCreateOptions.ReadyTimeoutSeconds"/>. execd starts
    /// serving before the runtime launches, so the daemon ping alone is not enough.
    /// Set <see cref="CodeInterpreterCreateOptions.SkipHealthCheck"/> to opt out.
    /// </remarks>
    /// <param name="sandbox">The sandbox to wrap.</param>
    /// <param name="options">Optional creation options.</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <returns>A new code interpreter instance.</returns>
    /// <exception cref="InvalidArgumentException">Thrown when <paramref name="sandbox"/> is null.</exception>
    /// <exception cref="SandboxException">Thrown when endpoint discovery or adapter initialization fails.</exception>
    /// <exception cref="SandboxReadyTimeoutException">Thrown when the code execution service health check times out.</exception>
    public static async Task<CodeInterpreter> CreateAsync(
        Sandbox sandbox,
        CodeInterpreterCreateOptions? options = null,
        CancellationToken cancellationToken = default)
    {
        if (sandbox == null)
        {
            throw new InvalidArgumentException("sandbox cannot be null");
        }

        var loggerFactory = options?.Diagnostics?.LoggerFactory ?? sandbox.SharedLoggerFactory ?? NullLoggerFactory.Instance;
        var logger = loggerFactory.CreateLogger("OpenSandbox.CodeInterpreter.CodeInterpreter");
        var endpoint = await sandbox.GetEndpointAsync(Constants.DefaultExecdPort, cancellationToken).ConfigureAwait(false);
        logger.LogInformation("Creating code interpreter for sandbox: {SandboxId}", sandbox.Id);
        var protocol = sandbox.ConnectionConfig.Protocol == ConnectionProtocol.Https ? "https" : "http";
        var execdBaseUrl = $"{protocol}://{endpoint.EndpointAddress}";
        var execdHeaders = MergeHeaders(sandbox.ConnectionConfig.Headers, endpoint.Headers);
        var adapterFactory = options?.AdapterFactory ?? DefaultCodeInterpreterAdapterFactory.Create();

        var codes = adapterFactory.CreateCodes(new CreateCodesStackOptions
        {
            ConnectionConfig = sandbox.ConnectionConfig,
            ExecdBaseUrl = execdBaseUrl,
            ExecdHeaders = execdHeaders,
            HttpClientProvider = sandbox.SharedHttpClientProvider,
            LoggerFactory = loggerFactory
        });

        // Fallback execd probe for custom codes adapters that do not implement
        // IExecdHealth; shares the sandbox's HTTP client.
        var fallbackExecdClient = new HttpClientWrapper(
            sandbox.SharedHttpClientProvider.HttpClient,
            execdBaseUrl,
            execdHeaders,
            loggerFactory.CreateLogger("OpenSandbox.CodeInterpreter.HttpClientWrapper"));

        var interpreter = new CodeInterpreter(sandbox, codes, logger, fallbackExecdClient);

        if (!(options?.SkipHealthCheck ?? false))
        {
            await interpreter.WaitUntilReadyAsync(new CodeInterpreterWaitUntilReadyOptions
            {
                ReadyTimeoutSeconds = options?.ReadyTimeoutSeconds ?? Constants.DefaultReadyTimeoutSeconds,
                PollingIntervalMillis = options?.HealthCheckPollingInterval ?? Constants.DefaultHealthCheckPollingIntervalMillis,
            }, cancellationToken).ConfigureAwait(false);
        }

        return interpreter;
    }

    /// <summary>
    /// Checks whether the code interpreter is healthy (strict check).
    /// </summary>
    /// <remarks>
    /// Healthy means both:
    /// <list type="bullet">
    /// <item>the code execution service (execd) answers <c>GET /ping</c>; and</item>
    /// <item>the code interpreter runtime (Jupyter kernel gateway) is serving
    /// inside the sandbox, verified by probing its listen port through the
    /// execd command API.</item>
    /// </list>
    /// Exceptions from either leg are treated as unhealthy.
    /// </remarks>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <returns>True if the code interpreter is healthy, false otherwise.</returns>
    public async Task<bool> IsHealthyAsync(CancellationToken cancellationToken = default)
    {
        try
        {
            if (!await PingExecdAsync(cancellationToken).ConfigureAwait(false))
            {
                return false;
            }

            return await IsRuntimeServingAsync(cancellationToken).ConfigureAwait(false);
        }
        catch (Exception ex)
        {
            _logger.LogDebug(ex, "Health check failed for code interpreter {SandboxId}", Id);
            return false;
        }
    }

    /// <summary>
    /// Pings the execd daemon on the interpreter's own endpoint. Prefers the codes
    /// service's optional <see cref="IExecdHealth"/> capability; custom adapters that
    /// do not implement it fall back to a direct probe sharing the sandbox HTTP client.
    /// </summary>
    private async Task<bool> PingExecdAsync(CancellationToken cancellationToken)
    {
        if (Codes is IExecdHealth health)
        {
            return await health.PingAsync(cancellationToken).ConfigureAwait(false);
        }

        if (_fallbackExecdClient == null)
        {
            return false;
        }

        try
        {
            await _fallbackExecdClient.GetAsync("/ping", cancellationToken: cancellationToken).ConfigureAwait(false);
            return true;
        }
        catch (Exception ex)
        {
            _logger.LogDebug(ex, "Fallback execd ping failed for code interpreter {SandboxId}", Id);
            return false;
        }
    }

    private async Task<bool> IsRuntimeServingAsync(CancellationToken cancellationToken)
    {
        try
        {
            var execution = await Sandbox.Commands.RunAsync(
                CodeInterpreterHealthCheck.RuntimeCheckCommand,
                cancellationToken: cancellationToken).ConfigureAwait(false);
            return execution?.Error == null;
        }
        catch (Exception ex)
        {
            _logger.LogDebug(ex, "Runtime check failed for code interpreter {SandboxId}", Id);
            return false;
        }
    }

    /// <summary>
    /// Waits until the strict health check passes (execd answers <c>GET /ping</c>
    /// and the interpreter runtime process is alive).
    /// </summary>
    /// <param name="options">The wait options.</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <exception cref="SandboxReadyTimeoutException">Thrown when the health check times out.</exception>
    /// <exception cref="OperationCanceledException">Thrown when <paramref name="cancellationToken"/> is canceled.</exception>
    public async Task WaitUntilReadyAsync(
        CodeInterpreterWaitUntilReadyOptions options,
        CancellationToken cancellationToken = default)
    {
        _logger.LogDebug(
            "Start readiness check for code interpreter {SandboxId} (timeoutSeconds={TimeoutSeconds})",
            Id, options.ReadyTimeoutSeconds);
        var timeout = TimeSpan.FromSeconds(options.ReadyTimeoutSeconds);
        var stopwatch = Stopwatch.StartNew();
        var attempt = 0;
        var errorDetail = "Health check returned false continuously.";

        while (true)
        {
            cancellationToken.ThrowIfCancellationRequested();

            if (stopwatch.Elapsed > timeout)
            {
                throw new SandboxReadyTimeoutException(
                    $"Code interpreter {Id} health check timed out after {options.ReadyTimeoutSeconds}s ({attempt} attempts). " +
                    $"{errorDetail} The code execution service (execd) or the interpreter runtime (Jupyter) " +
                    "did not become ready. Set SkipHealthCheck to skip this check.");
            }
            attempt++;

            try
            {
                if (await IsHealthyAsync(cancellationToken).ConfigureAwait(false))
                {
                    _logger.LogInformation("Code interpreter is ready: {SandboxId}", Id);
                    return;
                }

                errorDetail = "Health check returned false continuously.";
            }
            catch (Exception ex)
            {
                _logger.LogDebug(ex, "Readiness probe failed for code interpreter {SandboxId}", Id);
                errorDetail = $"Last health check error: {ex.Message}";
            }

            var remaining = timeout - stopwatch.Elapsed;
            if (remaining <= TimeSpan.Zero)
            {
                continue;
            }

            var pollingInterval = TimeSpan.FromMilliseconds(options.PollingIntervalMillis);
            var delay = pollingInterval < remaining ? pollingInterval : remaining;
            await Task.Delay(delay, cancellationToken).ConfigureAwait(false);
        }
    }

    private static IReadOnlyDictionary<string, string> MergeHeaders(
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
