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
using OpenSandbox.Adapters;
using OpenSandbox.CodeInterpreter.Models;
using OpenSandbox.CodeInterpreter.Services;
using OpenSandbox.Core;
using OpenSandbox.Internal;
using OpenSandbox.Models;
using Microsoft.Extensions.Logging;

namespace OpenSandbox.CodeInterpreter.Adapters;

/// <summary>
/// Adapter implementation for the codes service.
/// </summary>
internal sealed class CodesAdapter : ICodes
{
    private readonly HttpClientWrapper _client;
    private readonly HttpClient _sseHttpClient;
    private readonly string _baseUrl;
    private readonly IReadOnlyDictionary<string, string> _headers;
    private readonly ILogger _logger;

    private static readonly JsonSerializerOptions JsonOptions = new()
    {
        PropertyNamingPolicy = JsonNamingPolicy.CamelCase,
        PropertyNameCaseInsensitive = true,
        DefaultIgnoreCondition = System.Text.Json.Serialization.JsonIgnoreCondition.WhenWritingNull
    };

    public CodesAdapter(
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

    public async Task<CodeContext> CreateContextAsync(string language, CancellationToken cancellationToken = default)
    {
        if (string.IsNullOrWhiteSpace(language))
        {
            throw new InvalidArgumentException("Language cannot be empty");
        }

        var request = new CreateContextRequest { Language = language };
        _logger.LogDebug("Creating code context (language={Language})", language);
        var response = await _client.PostAsync<CodeContext>("/code/context", request, cancellationToken).ConfigureAwait(false);

        if (response == null || string.IsNullOrEmpty(response.Language))
        {
            throw new SandboxApiException(
                message: "Create code context failed: unexpected response shape",
                error: new SandboxError(SandboxErrorCodes.UnexpectedResponse, "Create code context failed: unexpected response shape"));
        }

        return response;
    }

    public async Task<CodeContext> GetContextAsync(string contextId, CancellationToken cancellationToken = default)
    {
        if (string.IsNullOrWhiteSpace(contextId))
        {
            throw new InvalidArgumentException("contextId cannot be empty");
        }

        _logger.LogDebug("Fetching code context: {ContextId}", contextId);
        var response = await _client.GetAsync<CodeContext>($"/code/contexts/{Uri.EscapeDataString(contextId)}", cancellationToken: cancellationToken).ConfigureAwait(false);

        if (response == null || string.IsNullOrEmpty(response.Language))
        {
            throw new SandboxApiException(
                message: "Get code context failed: unexpected response shape",
                error: new SandboxError(SandboxErrorCodes.UnexpectedResponse, "Get code context failed: unexpected response shape"));
        }

        return response;
    }

    public async Task<IReadOnlyList<CodeContext>> ListContextsAsync(string language, CancellationToken cancellationToken = default)
    {
        if (string.IsNullOrWhiteSpace(language))
        {
            throw new InvalidArgumentException("Language cannot be empty");
        }

        _logger.LogDebug("Listing code contexts (language={Language})", language);
        var queryParams = new Dictionary<string, string?> { ["language"] = language };

        var response = await _client.GetAsync<List<CodeContext>>("/code/contexts", queryParams, cancellationToken).ConfigureAwait(false);

        if (response == null)
        {
            throw new SandboxApiException(
                message: "List code contexts failed: unexpected response shape",
                error: new SandboxError(SandboxErrorCodes.UnexpectedResponse, "List code contexts failed: unexpected response shape"));
        }

        return response;
    }

    public async Task DeleteContextAsync(string contextId, CancellationToken cancellationToken = default)
    {
        if (string.IsNullOrWhiteSpace(contextId))
        {
            throw new InvalidArgumentException("contextId cannot be empty");
        }

        _logger.LogInformation("Deleting code context: {ContextId}", contextId);
        await _client.DeleteAsync($"/code/contexts/{Uri.EscapeDataString(contextId)}", cancellationToken: cancellationToken).ConfigureAwait(false);
    }

    public async Task DeleteContextsAsync(string language, CancellationToken cancellationToken = default)
    {
        if (string.IsNullOrWhiteSpace(language))
        {
            throw new InvalidArgumentException("Language cannot be empty");
        }

        _logger.LogInformation("Deleting code contexts (language={Language})", language);
        var queryParams = new Dictionary<string, string?> { ["language"] = language };
        await _client.DeleteAsync("/code/contexts", queryParams, cancellationToken).ConfigureAwait(false);
    }

    public async Task InterruptAsync(string executionId, CancellationToken cancellationToken = default)
    {
        if (string.IsNullOrWhiteSpace(executionId))
        {
            throw new InvalidArgumentException("executionId cannot be empty");
        }

        _logger.LogInformation("Interrupting code execution: {ExecutionId}", executionId);
        var queryParams = new Dictionary<string, string?> { ["id"] = executionId };
        await _client.DeleteAsync("/code", queryParams, cancellationToken).ConfigureAwait(false);
    }

    public async IAsyncEnumerable<ServerStreamEvent> RunStreamAsync(
        RunCodeRequest request,
        [EnumeratorCancellation] CancellationToken cancellationToken = default)
    {
        if (request == null)
        {
            throw new InvalidArgumentException("request cannot be null");
        }

        if (string.IsNullOrWhiteSpace(request.Code))
        {
            throw new InvalidArgumentException("Code cannot be empty");
        }

        var url = $"{_baseUrl}/code";
        _logger.LogDebug("Running code stream (codeLength={CodeLength})", request.Code.Length);
        var json = JsonSerializer.Serialize(request, JsonOptions);

        using var httpRequest = new HttpRequestMessage(HttpMethod.Post, url)
        {
            Content = new StringContent(json, Encoding.UTF8, "application/json")
        };

        httpRequest.Headers.Accept.Add(new System.Net.Http.Headers.MediaTypeWithQualityHeaderValue("text/event-stream"));

        foreach (var header in _headers)
        {
            httpRequest.Headers.TryAddWithoutValidation(header.Key, header.Value);
        }

        using var response = await _sseHttpClient.SendAsync(httpRequest, HttpCompletionOption.ResponseHeadersRead, cancellationToken).ConfigureAwait(false);

        await foreach (var ev in SseParser.ParseJsonEventStreamAsync<ServerStreamEvent>(response, "Run code failed", cancellationToken).ConfigureAwait(false))
        {
            yield return ev;
        }
    }

    public async Task<Execution> RunAsync(string code, RunCodeOptions? options = null, CancellationToken cancellationToken = default)
    {
        if (string.IsNullOrWhiteSpace(code))
        {
            throw new InvalidArgumentException("Code cannot be empty");
        }

        if (options?.Context != null && options.Language != null)
        {
            throw new InvalidArgumentException("Provide either options.Context or options.Language, not both");
        }

        var context = options?.Context
            ?? (options?.Language != null
                ? new CodeContext { Language = options.Language }
                : new CodeContext { Language = SupportedLanguage.Python });

        var request = new RunCodeRequest
        {
            Code = code,
            Context = context
        };

        var execution = new Execution();
        _logger.LogDebug("Running code (codeLength={CodeLength})", code.Length);
        var dispatcher = new ExecutionEventDispatcher(execution, options?.Handlers);

        await foreach (var ev in RunStreamAsync(request, cancellationToken).ConfigureAwait(false))
        {
            await dispatcher.DispatchAsync(ev).ConfigureAwait(false);
        }

        return execution;
    }
}
