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

    public async IAsyncEnumerable<ServerStreamEvent> RunStreamAsync(
        string command,
        RunCommandOptions? options = null,
        [EnumeratorCancellation] CancellationToken cancellationToken = default)
    {
        ValidateRunOptions(options);
        _logger.LogDebug("Running command stream (commandLength={CommandLength})", command.Length);

        var spec = new StreamingRequestSpec(
            Url: $"{_baseUrl}/command",
            Body: BuildRunCommandRequest(command, options),
            ErrorMessage: "Run command failed");

        await foreach (var ev in StreamExecutionAsync(spec, cancellationToken).ConfigureAwait(false))
        {
            yield return ev;
        }
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
            inferExitCode: !(options?.Background ?? false),
            cancellationToken).ConfigureAwait(false);
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
            inferExitCode: true,
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

    private static RunCommandRequest BuildRunCommandRequest(string command, RunCommandOptions? options)
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
        bool inferExitCode,
        CancellationToken cancellationToken)
    {
        var execution = new Execution();
        var dispatcher = new ExecutionEventDispatcher(execution, handlers);

        await foreach (var ev in stream.WithCancellation(cancellationToken).ConfigureAwait(false))
        {
            PreserveLegacyInitId(ev, execution);
            await dispatcher.DispatchAsync(ev).ConfigureAwait(false);
        }

        if (inferExitCode)
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

        var message = errorMessage ?? $"Request failed with status code {(int)response.StatusCode}";
        return new SandboxApiException(
            message: message,
            statusCode: (int)response.StatusCode,
            requestId: requestId,
            rawBody: rawBody,
            error: new SandboxError(errorCode ?? SandboxErrorCodes.UnexpectedResponse, errorMessage ?? message));
    }
}
