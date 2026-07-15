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

using System.Text;
using System.Text.Json;
using OpenSandbox.Core;
using OpenSandbox.Internal;
using OpenSandbox.Models;
using OpenSandbox.Services;
using Microsoft.Extensions.Logging;

namespace OpenSandbox.Adapters;

internal sealed class IsolationSessionHandle : IIsolationSession
{
    private readonly IsolatedSessionsAdapter _adapter;
    private ISandboxFiles? _files;

    public IsolationSessionHandle(IsolatedSessionInfo info, IsolatedSessionsAdapter adapter)
    {
        Info = info;
        _adapter = adapter;
    }

    public string SessionId => Info.SessionId;
    public IsolatedSessionInfo Info { get; }

    public ISandboxFiles Files
    {
        get
        {
            _files ??= _adapter.CreateFilesForSession(Info.SessionId);
            return _files;
        }
    }

    public Task<Execution> RunAsync(
        string code, IsolatedRunOpts? opts = null, ExecutionHandlers? handlers = null,
        CancellationToken cancellationToken = default)
        => _adapter.RunInternalAsync(SessionId, code, opts, handlers, cancellationToken);

    public Task<IsolatedSessionState> GetAsync(CancellationToken cancellationToken = default)
        => _adapter.GetInternalAsync(SessionId, cancellationToken);

    public Task DeleteAsync(CancellationToken cancellationToken = default)
        => _adapter.DeleteInternalAsync(SessionId, cancellationToken);
}

internal sealed class IsolatedSessionsAdapter : IIsolatedSessions
{
    private readonly HttpClient _httpClient;
    private readonly HttpClient _sseHttpClient;
    internal readonly string _baseUrl;
    internal readonly IReadOnlyDictionary<string, string> _headers;
    private readonly ILogger _logger;

    private static readonly JsonSerializerOptions JsonOptions = new()
    {
        PropertyNamingPolicy = JsonNamingPolicy.SnakeCaseLower,
        DefaultIgnoreCondition = System.Text.Json.Serialization.JsonIgnoreCondition.WhenWritingNull
    };

    public IsolatedSessionsAdapter(
        HttpClient httpClient,
        HttpClient sseHttpClient,
        string baseUrl,
        IReadOnlyDictionary<string, string> headers,
        ILogger logger)
    {
        _httpClient = httpClient ?? throw new ArgumentNullException(nameof(httpClient));
        _sseHttpClient = sseHttpClient ?? throw new ArgumentNullException(nameof(sseHttpClient));
        _baseUrl = baseUrl?.TrimEnd('/') ?? throw new ArgumentNullException(nameof(baseUrl));
        _headers = headers ?? new Dictionary<string, string>();
        _logger = logger ?? throw new ArgumentNullException(nameof(logger));
    }

    public async Task<IIsolationSession> CreateAsync(
        CreateIsolatedSessionRequest request,
        CancellationToken cancellationToken = default)
    {
        var url = $"{_baseUrl}/v1/isolated/session";
        var json = JsonSerializer.Serialize(request, JsonOptions);
        using var httpRequest = new HttpRequestMessage(HttpMethod.Post, url);
        httpRequest.Content = new StringContent(json, Encoding.UTF8, "application/json");
        ApplyHeaders(httpRequest);

        using var response = await _httpClient.SendAsync(httpRequest, cancellationToken);
        await EnsureSuccessAsync(response, "create isolated session");
        var body = await response.Content.ReadAsStringAsync().ConfigureAwait(false);
        var info = JsonSerializer.Deserialize<IsolatedSessionInfo>(body, JsonOptions)!;
        return new IsolationSessionHandle(info, this);
    }

    public async Task<IIsolationSession> AttachAsync(
        string sessionId,
        CancellationToken cancellationToken = default)
    {
        if (string.IsNullOrWhiteSpace(sessionId))
        {
            throw new ArgumentException("sessionId cannot be empty", nameof(sessionId));
        }

        var state = await GetStateAsync(sessionId, "attach isolated session", cancellationToken)
            .ConfigureAwait(false);
        // Build an IsolatedSessionInfo from the SessionState response. The
        // creation-parameter echo fields are additive/optional; older execd
        // builds omit them. Missing fields remain null on the info so the
        // handle is still usable via sessionId for RunAsync/GetAsync/DeleteAsync.
        var info = new IsolatedSessionInfo(
            SessionId: sessionId,
            CreatedAt: state.CreatedAt,
            Profile: state.Profile,
            Workspace: state.Workspace,
            ExtraWritable: state.ExtraWritable,
            Binds: state.Binds,
            ShareNet: state.ShareNet,
            EnvPassthrough: state.EnvPassthrough,
            Uid: state.Uid,
            Gid: state.Gid,
            UidMode: state.UidMode,
            IdleTimeoutSeconds: state.IdleTimeoutSeconds);
        return new IsolationSessionHandle(info, this);
    }

    internal Task<IsolatedSessionState> GetInternalAsync(
        string sessionId, CancellationToken cancellationToken = default)
        => GetStateAsync(sessionId, "get isolated session", cancellationToken);

    private async Task<IsolatedSessionState> GetStateAsync(
        string sessionId, string operation, CancellationToken cancellationToken)
    {
        var url = $"{_baseUrl}/v1/isolated/session/{Uri.EscapeDataString(sessionId)}";
        using var httpRequest = new HttpRequestMessage(HttpMethod.Get, url);
        ApplyHeaders(httpRequest);

        using var response = await _httpClient.SendAsync(httpRequest, cancellationToken);
        await EnsureSuccessAsync(response, operation);
        var body = await response.Content.ReadAsStringAsync().ConfigureAwait(false);
        return JsonSerializer.Deserialize<IsolatedSessionState>(body, JsonOptions)!;
    }

    internal async Task<Execution> RunInternalAsync(
        string sessionId, string code, IsolatedRunOpts? opts = null,
        ExecutionHandlers? handlers = null, CancellationToken cancellationToken = default)
    {
        var url = $"{_baseUrl}/v1/isolated/session/{Uri.EscapeDataString(sessionId)}/run";
        var bodyObj = new Dictionary<string, object> { ["code"] = code };
        if (opts?.Envs != null) bodyObj["envs"] = opts.Envs;
        if (opts?.TimeoutSeconds != null) bodyObj["timeout_seconds"] = opts.TimeoutSeconds;

        var json = JsonSerializer.Serialize(bodyObj, JsonOptions);
        using var httpRequest = new HttpRequestMessage(HttpMethod.Post, url);
        httpRequest.Content = new StringContent(json, Encoding.UTF8, "application/json");
        httpRequest.Headers.Accept.ParseAdd("text/event-stream");
        ApplyHeaders(httpRequest);

        using var response = await _sseHttpClient.SendAsync(
            httpRequest, HttpCompletionOption.ResponseHeadersRead, cancellationToken).ConfigureAwait(false);

        var execution = new Execution();
        var dispatcher = new ExecutionEventDispatcher(execution, handlers);

        await foreach (var ev in SseParser.ParseJsonEventStreamAsync<ServerStreamEvent>(
            response, "run in isolated session failed", cancellationToken).ConfigureAwait(false))
        {
            await dispatcher.DispatchAsync(ev).ConfigureAwait(false);
        }

        execution.ExitCode = InferExitCode(execution);
        return execution;
    }

    internal async Task DeleteInternalAsync(
        string sessionId, CancellationToken cancellationToken = default)
    {
        var url = $"{_baseUrl}/v1/isolated/session/{Uri.EscapeDataString(sessionId)}";
        using var httpRequest = new HttpRequestMessage(HttpMethod.Delete, url);
        ApplyHeaders(httpRequest);

        using var response = await _httpClient.SendAsync(httpRequest, cancellationToken);
        await EnsureSuccessAsync(response, "delete isolated session");
    }

    public async Task<IsolatedCapabilities> CapabilitiesAsync(
        CancellationToken cancellationToken = default)
    {
        var url = $"{_baseUrl}/v1/isolated/capabilities";
        using var httpRequest = new HttpRequestMessage(HttpMethod.Get, url);
        ApplyHeaders(httpRequest);

        using var response = await _httpClient.SendAsync(httpRequest, cancellationToken);
        await EnsureSuccessAsync(response, "get isolated capabilities");
        var body = await response.Content.ReadAsStringAsync().ConfigureAwait(false);
        return JsonSerializer.Deserialize<IsolatedCapabilities>(body, JsonOptions)!;
    }

    public async Task<IReadOnlyList<IsolatedSessionSummary>> ListAsync(
        CancellationToken cancellationToken = default)
    {
        var url = $"{_baseUrl}/v1/isolated/sessions";
        using var httpRequest = new HttpRequestMessage(HttpMethod.Get, url);
        ApplyHeaders(httpRequest);

        using var response = await _httpClient.SendAsync(httpRequest, cancellationToken);
        await EnsureSuccessAsync(response, "list isolated sessions");
        var body = await response.Content.ReadAsStringAsync().ConfigureAwait(false);
        var result = JsonSerializer.Deserialize<ListIsolatedSessionsResponse>(body, JsonOptions)!;
        return result.Sessions ?? new List<IsolatedSessionSummary>();
    }

    internal ISandboxFiles CreateFilesForSession(string sessionId)
    {
        var sessionBaseUrl = $"{_baseUrl}/v1/isolated/session/{Uri.EscapeDataString(sessionId)}";
        var clientWrapper = new HttpClientWrapper(
            _httpClient, sessionBaseUrl, _headers,
            _logger);
        return new FilesystemAdapter(clientWrapper, _httpClient, sessionBaseUrl, _headers);
    }

    private void ApplyHeaders(HttpRequestMessage request)
    {
        foreach (var header in _headers)
        {
            request.Headers.TryAddWithoutValidation(header.Key, header.Value);
        }
    }

    private static async Task EnsureSuccessAsync(HttpResponseMessage response, string operation)
    {
        if (response.IsSuccessStatusCode) return;
        var body = await response.Content.ReadAsStringAsync().ConfigureAwait(false);
        throw new SandboxApiException(
            $"{operation} failed. Status: {(int)response.StatusCode}, Body: {body}",
            (int)response.StatusCode);
    }

    private static int? InferExitCode(Execution execution)
    {
        if (execution.Error != null)
        {
            if (int.TryParse(execution.Error.Value?.Trim(), out var code))
                return code;
            return null;
        }
        if (execution.Complete != null) return 0;
        return null;
    }
}
