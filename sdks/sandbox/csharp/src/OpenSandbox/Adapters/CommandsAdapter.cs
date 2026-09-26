// Copyright 2026 The OpenSandbox Authors
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

using System.Runtime.CompilerServices;
using System.Text;
using System.Text.Json;
using OpenSandbox.Core;
using OpenSandbox.Internal;
using OpenSandbox.Models;
using OpenSandbox.Services;
using Microsoft.Extensions.Logging;

namespace OpenSandbox.Adapters;

/// <summary>
/// Adapter for the execd commands service.
/// </summary>
internal sealed class CommandsAdapter : IExecdCommands
{
    private readonly HttpClientWrapper _client;
    private readonly HttpClient _sseHttpClient;
    private readonly string _baseUrl;
    private readonly IReadOnlyDictionary<string, string> _headers;
    private readonly ILogger _logger;

    private static readonly JsonSerializerOptions JsonOptions = new()
    {
        PropertyNamingPolicy = JsonNamingPolicy.CamelCase,
        DefaultIgnoreCondition = System.Text.Json.Serialization.JsonIgnoreCondition.WhenWritingNull
    };

    public CommandsAdapter(
        HttpClientWrapper client,
        HttpClient sseHttpClient,
        string baseUrl,
        IReadOnlyDictionary<string, string> headers,
        ILogger logger)
    {
        _client = client ?? throw new ArgumentNullException(nameof(client));
        _sseHttpClient = sseHttpClient ?? throw new ArgumentNullException(nameof(sseHttpClient));
        _baseUrl = baseUrl?.TrimEnd('/') ?? throw new ArgumentNullException(nameof(baseUrl));
        _headers = headers ?? new Dictionary<string, string>();
        _logger = logger ?? throw new ArgumentNullException(nameof(logger));
    }

    public IAsyncEnumerable<ServerStreamEvent> RunStreamAsync(
        string command,
        RunCommandOptions? options = null,
        CancellationToken cancellationToken = default)
    {
        ValidateRunOptions(options);
        return RunRequestStreamAsync(BuildRunCommandRequest(command, options), cancellationToken);
    }

    public IAsyncEnumerable<ServerStreamEvent> RunStreamAsync(
        IReadOnlyList<string> argv,
        RunCommandOptions? options = null,
        CancellationToken cancellationToken = default)
    {
        ValidateRunOptions(options);
        if (argv is null || argv.Count == 0 || string.IsNullOrEmpty(argv[0]) || argv.Any(arg => arg is null || arg.Contains('\0')))
            throw new InvalidArgumentException("Argv requires a non-empty executable and strings without NUL");
        var request = BuildRunCommandRequest(null, options);
        request.Argv = argv.ToArray();
        return RunRequestStreamAsync(request, cancellationToken);
    }

    private async IAsyncEnumerable<ServerStreamEvent> RunRequestStreamAsync(
        RunCommandRequest request,
        [EnumeratorCancellation] CancellationToken cancellationToken)
    {
        var spec = new StreamingRequestSpec(
            Url: $"{_baseUrl}/command",
            Body: request,
            ErrorMessage: "Run command failed");
        await foreach (var ev in StreamExecutionAsync(spec, cancellationToken).ConfigureAwait(false))
        {
            yield return ev;
        }
    }

    public Task<Execution> RunAsync(
        IReadOnlyList<string> argv,
        RunCommandOptions? options = null,
        ExecutionHandlers? handlers = null,
        CancellationToken cancellationToken = default)
    {
        return ConsumeExecutionAsync(
            RunStreamAsync(argv, options, cancellationToken), handlers,
            isBackground: options?.Background ?? false, cancellationToken);
    }

    public async Task<Execution> RunAsync(
        string command,
        RunCommandOptions? options = null,
        ExecutionHandlers? handlers = null,
        CancellationToken cancellationToken = default)
    {
        _logger.LogDebug("Running command (commandLength={CommandLength})", command.Length);
        return await ConsumeExecutionAsync(
            RunStreamAsync(command, options, cancellationToken),
            handlers,
            isBackground: options?.Background ?? false,
            cancellationToken).ConfigureAwait(false);
    }

    public async Task SetEnvAsync(string key, string value, CancellationToken cancellationToken = default)
    {
        var command = BuildSetEnvCommand(key, value);
        var execution = await RunAsync(command, cancellationToken: cancellationToken).ConfigureAwait(false);
        var failed = execution.Error != null || (execution.ExitCode.HasValue && execution.ExitCode.Value != 0);
        if (!failed)
        {
            return;
        }

        var stderr = string.Concat(execution.Logs.Stderr.Select(m => m.Text)).Trim();
        var detail = stderr.Length > 0 ? stderr : execution.Error?.Value?.Trim() ?? string.Empty;
        var message = $"commands.SetEnvAsync failed for '{key}'" + (detail.Length > 0 ? $": {detail}" : string.Empty);
        throw new SandboxException(
            message,
            error: new SandboxError(SandboxErrorCodes.InternalUnknownError, message));
    }

    public async Task InterruptAsync(string sessionId, CancellationToken cancellationToken = default)
    {
        _logger.LogInformation("Interrupting execution: {ExecutionId}", sessionId);
        var queryParams = new Dictionary<string, string?> { ["id"] = sessionId };
        await _client.DeleteAsync("/command", queryParams, cancellationToken).ConfigureAwait(false);
    }

    public async Task<string> CreateSessionAsync(
        CreateSessionOptions? options = null,
        CancellationToken cancellationToken = default)
    {
        _logger.LogDebug("Creating bash session (workingDirectory={WorkingDirectory})", options?.WorkingDirectory);
        var response = await _client.PostAsync<CreateSessionResponse>("/session", BuildCreateSessionBody(options), cancellationToken).ConfigureAwait(false);
        if (string.IsNullOrEmpty(response?.SessionId))
        {
            throw new SandboxApiException(
                message: "Create session returned empty session_id",
                statusCode: 200,
                error: new SandboxError(SandboxErrorCodes.UnexpectedResponse, "Create session returned empty session_id"));
        }

        return response!.SessionId;
    }

    public async IAsyncEnumerable<ServerStreamEvent> RunInSessionStreamAsync(
        string sessionId,
        string command,
        RunInSessionOptions? options = null,
        [EnumeratorCancellation] CancellationToken cancellationToken = default)
    {
        if (string.IsNullOrWhiteSpace(sessionId))
        {
            throw new InvalidArgumentException("sessionId cannot be empty");
        }
        if (string.IsNullOrWhiteSpace(command))
        {
            throw new InvalidArgumentException("command cannot be empty");
        }

        var spec = new StreamingRequestSpec(
            Url: $"{_baseUrl}/session/{Uri.EscapeDataString(sessionId)}/run",
            Body: BuildRunInSessionRequest(command, options),
            ErrorMessage: "Run in session failed");

        await foreach (var ev in StreamExecutionAsync(spec, cancellationToken).ConfigureAwait(false))
        {
            yield return ev;
        }
    }

    public async Task<Execution> RunInSessionAsync(
        string sessionId,
        string command,
        RunInSessionOptions? options = null,
        ExecutionHandlers? handlers = null,
        CancellationToken cancellationToken = default)
    {
        if (string.IsNullOrWhiteSpace(sessionId))
        {
            throw new InvalidArgumentException("sessionId cannot be empty");
        }
        if (string.IsNullOrWhiteSpace(command))
        {
            throw new InvalidArgumentException("command cannot be empty");
        }

        _logger.LogDebug("Running in session: {SessionId} (commandLength={CommandLength})", sessionId, command.Length);
        return await ConsumeExecutionAsync(
            RunInSessionStreamAsync(sessionId, command, options, cancellationToken),
            handlers,
            isBackground: false,
            cancellationToken).ConfigureAwait(false);
    }

    public async Task DeleteSessionAsync(string sessionId, CancellationToken cancellationToken = default)
    {
        if (string.IsNullOrWhiteSpace(sessionId))
        {
            throw new InvalidArgumentException("sessionId cannot be empty");
        }

        _logger.LogDebug("Deleting bash session: {SessionId}", sessionId);
        var path = $"/session/{Uri.EscapeDataString(sessionId)}";
        await _client.DeleteAsync(path, cancellationToken: cancellationToken).ConfigureAwait(false);
    }

    public Task<CommandStatus> GetCommandStatusAsync(string executionId, CancellationToken cancellationToken = default)
    {
        if (string.IsNullOrWhiteSpace(executionId))
        {
            throw new InvalidArgumentException("executionId cannot be empty");
        }

        _logger.LogDebug("Fetching command status: {ExecutionId}", executionId);
        return _client.GetAsync<CommandStatus>($"/command/status/{Uri.EscapeDataString(executionId)}", cancellationToken: cancellationToken);
    }

    public async Task<CommandLogs> GetBackgroundCommandLogsAsync(
        string executionId,
        long? cursor = null,
        CancellationToken cancellationToken = default)
    {
        if (string.IsNullOrWhiteSpace(executionId))
        {
            throw new InvalidArgumentException("executionId cannot be empty");
        }

        _logger.LogDebug("Fetching command logs: {ExecutionId} (cursor={Cursor})", executionId, cursor);
        var path = $"/command/{Uri.EscapeDataString(executionId)}/logs";
        var query = cursor.HasValue ? $"?cursor={cursor.Value}" : string.Empty;
        var url = $"{_baseUrl}{path}{query}";

        using var request = new HttpRequestMessage(HttpMethod.Get, url);
        using var response = await _client.SendAsync(request, cancellationToken).ConfigureAwait(false);

        var content = await response.Content.ReadAsStringAsync().ConfigureAwait(false);
        if (!response.IsSuccessStatusCode)
        {
            throw CreateApiException(response, content);
        }

        var cursorHeader = response.Headers.TryGetValues("EXECD-COMMANDS-TAIL-CURSOR", out var values)
            ? values.FirstOrDefault()
            : null;
        var parsedCursor = long.TryParse(cursorHeader, out var c) ? c : (long?)null;

        return new CommandLogs
        {
            Content = content,
            Cursor = parsedCursor
        };
    }

    private async IAsyncEnumerable<ServerStreamEvent> StreamExecutionAsync(
        StreamingRequestSpec spec,
        [EnumeratorCancellation] CancellationToken cancellationToken = default)
    {
        var json = JsonSerializer.Serialize(spec.Body, JsonOptions);
        using var request = new HttpRequestMessage(HttpMethod.Post, spec.Url)
        {
            Content = new StringContent(json, Encoding.UTF8, "application/json")
        };

        request.Headers.Accept.Add(new System.Net.Http.Headers.MediaTypeWithQualityHeaderValue("text/event-stream"));

        foreach (var header in _headers)
        {
            request.Headers.TryAddWithoutValidation(header.Key, header.Value);
        }

        using var response = await _sseHttpClient.SendAsync(request, HttpCompletionOption.ResponseHeadersRead, cancellationToken).ConfigureAwait(false);

        await foreach (var ev in SseParser.ParseJsonEventStreamAsync<ServerStreamEvent>(response, spec.ErrorMessage, cancellationToken).ConfigureAwait(false))
        {
            yield return ev;
        }
    }

    private static readonly System.Text.RegularExpressions.Regex EnvKeyPattern =
        new("^[A-Za-z_][A-Za-z0-9_]*$", System.Text.RegularExpressions.RegexOptions.Compiled);

    /// <summary>Quotes a string as a single POSIX shell word.</summary>
    private static string ShellQuote(string s) => "'" + s.Replace("'", "'\\''") + "'";

    /// <summary>Escapes a value for the runtime env file's double-quoted form.</summary>
    private static string EscapeDoubleQuoted(string value) => value
        .Replace("\\", "\\\\")
        .Replace("\"", "\\\"")
        .Replace("\n", "\\n")
        .Replace("\r", "\\r")
        .Replace("\t", "\\t");

    /// <summary>
    /// Builds the sandbox-side snippet that appends KEY=VALUE to the env file named
    /// by the sandbox's EXECD_ENVS variable. Values without a single quote use the
    /// runtime env file's lossless single-quoted form; otherwise the double-quoted
    /// form is used (shell-style $NAME sequences in such values may be expanded
    /// when the runtime loads the file).
    /// </summary>
    private static string BuildSetEnvCommand(string key, string value)
    {
        if (!EnvKeyPattern.IsMatch(key))
        {
            throw new InvalidArgumentException($"setEnv key must match [A-Za-z_][A-Za-z0-9_]*, got '{key}'");
        }
        if (value.Contains('\0'))
        {
            throw new InvalidArgumentException("setEnv value cannot contain NUL bytes");
        }

        var entry = value.Contains('\'')
            ? $"{key}=\"{EscapeDoubleQuoted(value)}\""
            : $"{key}='{value}'";
        return string.Join("\n",
            $"if [ -z \"${{EXECD_ENVS:-}}\" ]; then printf '%s\\n' 'EXECD_ENVS is not set; cannot persist environment variable {key}' >&2; exit 1; fi",
            "mkdir -p \"$(dirname \"$EXECD_ENVS\")\"",
            $"printf '%s=%s\\n' {ShellQuote(entry)} >> \"$EXECD_ENVS\"");
    }

    private static void ValidateRunOptions(RunCommandOptions? options)
    {
        if (options?.Gid.HasValue == true && options.Uid.HasValue != true)
        {
            throw new InvalidArgumentException("uid is required when gid is provided");
        }
        if (options?.Uid.HasValue == true && options.Uid.Value < 0)
        {
            throw new InvalidArgumentException("uid must be >= 0");
        }
        if (options?.Gid.HasValue == true && options.Gid.Value < 0)
        {
            throw new InvalidArgumentException("gid must be >= 0");
        }
    }

    private static object? BuildCreateSessionBody(CreateSessionOptions? options)
    {
        var workingDirectory = options?.WorkingDirectory;
        return !string.IsNullOrEmpty(workingDirectory) ? new { cwd = workingDirectory } : null;
    }

    private static RunCommandRequest BuildRunCommandRequest(string? command, RunCommandOptions? options)
    {
        return new RunCommandRequest
        {
            Command = command,
            Cwd = options?.WorkingDirectory,
            Background = options?.Background,
            Timeout = options?.TimeoutSeconds.HasValue == true ? options.TimeoutSeconds.Value * 1000L : null,
            Uid = options?.Uid,
            Gid = options?.Gid,
            Envs = options?.Envs
        };
    }

    private static RunInSessionRequest BuildRunInSessionRequest(string command, RunInSessionOptions? options)
    {
        return new RunInSessionRequest
        {
            Command = command,
            Cwd = options?.WorkingDirectory,
            Timeout = options?.TimeoutSeconds is not null
                ? options.TimeoutSeconds.Value * 1000L
                : null
        };
    }

    private static int? InferForegroundExitCode(Execution execution)
    {
        if (execution.Error != null)
        {
            return int.TryParse(execution.Error.Value, out var exitCode) ? exitCode : null;
        }

        return execution.Complete != null ? 0 : null;
    }

    private static void PreserveLegacyInitId(ServerStreamEvent ev, Execution execution)
    {
        if (ev.Type == ServerStreamEventTypes.Init && string.IsNullOrEmpty(ev.Text) && !string.IsNullOrEmpty(execution.Id))
        {
            ev.Text = execution.Id;
        }
    }

    private async Task<Execution> ConsumeExecutionAsync(
        IAsyncEnumerable<ServerStreamEvent> stream,
        ExecutionHandlers? handlers,
        bool isBackground,
        CancellationToken cancellationToken)
    {
        var execution = new Execution();
        var dispatcher = new ExecutionEventDispatcher(execution, handlers);

        await foreach (var ev in stream.WithCancellation(cancellationToken).ConfigureAwait(false))
        {
            PreserveLegacyInitId(ev, execution);
            await dispatcher.DispatchAsync(ev).ConfigureAwait(false);
            if (isBackground && ev.Type == ServerStreamEventTypes.ExecutionComplete)
                break;
        }

        if (!isBackground)
        {
            execution.ExitCode = InferForegroundExitCode(execution);
        }

        return execution;
    }

    private sealed record StreamingRequestSpec(string Url, object Body, string ErrorMessage);

    private static SandboxApiException CreateApiException(HttpResponseMessage response, string content)
    {
        var requestId = response.Headers.TryGetValues(Constants.RequestIdHeader, out var values)
            ? values.FirstOrDefault()
            : null;

        string? errorMessage = null;
        string? errorCode = null;
        object? rawBody = content;

        if (!string.IsNullOrEmpty(content))
        {
            try
            {
                var parsed = JsonSerializer.Deserialize<Dictionary<string, JsonElement>>(content, JsonOptions);
                if (parsed != null)
                {
                    rawBody = parsed;
                    if (parsed.TryGetValue("message", out var msg))
                    {
                        errorMessage = msg.GetString();
                    }

                    if (parsed.TryGetValue("code", out var code))
                    {
                        errorCode = code.GetString();
                    }
                }
            }
            catch
            {
                // Ignore JSON parse errors and fallback to raw body.
            }
        }

        var message = errorMessage ?? $"Request failed with status code {(int)response.StatusCode}" +
            (string.IsNullOrEmpty(content) ? string.Empty : $": {content}");
        return new SandboxApiException(
            message: message,
            statusCode: (int)response.StatusCode,
            requestId: requestId,
            rawBody: rawBody,
            error: new SandboxError(errorCode ?? SandboxErrorCodes.UnexpectedResponse, errorMessage ?? message));
    }
}
