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

using System.Text.Json;
using System.Linq;
using OpenSandbox.Core;
using OpenSandbox.Internal;
using OpenSandbox.Models;
using OpenSandbox.Services;

namespace OpenSandbox.Adapters;

/// <summary>
/// Adapter for the sandbox lifecycle service.
/// </summary>
internal sealed class SandboxesAdapter : ISandboxes
{
    private readonly HttpClientWrapper _client;

    private static readonly JsonSerializerOptions JsonOptions = new()
    {
        PropertyNamingPolicy = JsonNamingPolicy.CamelCase,
        PropertyNameCaseInsensitive = true,
        DefaultIgnoreCondition = System.Text.Json.Serialization.JsonIgnoreCondition.WhenWritingNull
    };

    public SandboxesAdapter(HttpClientWrapper client)
    {
        _client = client ?? throw new ArgumentNullException(nameof(client));
    }

    public async Task<CreateSandboxResponse> CreateSandboxAsync(
        CreateSandboxRequest request,
        CancellationToken cancellationToken = default)
    {
        var response = await _client.PostAsync<JsonElement>("/sandboxes", request, cancellationToken).ConfigureAwait(false);
        return ParseCreateSandboxResponse(response);
    }

    public async Task<SandboxInfo> GetSandboxAsync(
        string sandboxId,
        CancellationToken cancellationToken = default)
    {
        var response = await _client.GetAsync<JsonElement>($"/sandboxes/{Uri.EscapeDataString(sandboxId)}", cancellationToken: cancellationToken).ConfigureAwait(false);
        return ParseSandboxInfo(response);
    }

    public async Task<ListSandboxesResponse> ListSandboxesAsync(
        ListSandboxesParams? @params = null,
        CancellationToken cancellationToken = default)
    {
        var queryParts = new List<string>();

        if (@params?.States != null && @params.States.Count > 0)
        {
            // The API expects repeated query params: ?state=Running&state=Paused
            queryParts.AddRange(@params.States.Select(state => $"state={Uri.EscapeDataString(state)}"));
        }

        if (@params?.Metadata != null && @params.Metadata.Count > 0)
        {
            // Encode metadata as k=v&k2=v2
            var metadataStr = string.Join("&", @params.Metadata.Select(kv => $"{kv.Key}={kv.Value}"));
            queryParts.Add($"metadata={Uri.EscapeDataString(metadataStr)}");
        }

        if (@params?.Page.HasValue == true)
        {
            queryParts.Add($"page={@params.Page.Value}");
        }

        if (@params?.PageSize.HasValue == true)
        {
            queryParts.Add($"pageSize={@params.PageSize.Value}");
        }

        var path = queryParts.Count > 0
            ? $"/sandboxes?{string.Join("&", queryParts)}"
            : "/sandboxes";

        var response = await _client.GetAsync<JsonElement>(path, cancellationToken: cancellationToken).ConfigureAwait(false);
        return ParseListSandboxesResponse(response);
    }

    public async Task<SandboxInfo> PatchSandboxMetadataAsync(
        string sandboxId,
        IReadOnlyDictionary<string, string?> patch,
        CancellationToken cancellationToken = default)
    {
        var response = await _client.PatchAsync<JsonElement>(
            $"/sandboxes/{Uri.EscapeDataString(sandboxId)}/metadata",
            patch,
            cancellationToken).ConfigureAwait(false);
        return ParseSandboxInfo(response);
    }

    public async Task DeleteSandboxAsync(
        string sandboxId,
        CancellationToken cancellationToken = default)
    {
        await _client.DeleteAsync($"/sandboxes/{Uri.EscapeDataString(sandboxId)}", cancellationToken: cancellationToken).ConfigureAwait(false);
    }

    public async Task PauseSandboxAsync(
        string sandboxId,
        CancellationToken cancellationToken = default)
    {
        await _client.PostAsync($"/sandboxes/{Uri.EscapeDataString(sandboxId)}/pause", cancellationToken: cancellationToken).ConfigureAwait(false);
    }

    public async Task ResumeSandboxAsync(
        string sandboxId,
        CancellationToken cancellationToken = default)
    {
        await _client.PostAsync($"/sandboxes/{Uri.EscapeDataString(sandboxId)}/resume", cancellationToken: cancellationToken).ConfigureAwait(false);
    }

    public async Task<RenewSandboxExpirationResponse> RenewSandboxExpirationAsync(
        string sandboxId,
        RenewSandboxExpirationRequest request,
        CancellationToken cancellationToken = default)
    {
        var response = await _client.PostAsync<JsonElement>(
            $"/sandboxes/{Uri.EscapeDataString(sandboxId)}/renew-expiration",
            request,
            cancellationToken).ConfigureAwait(false);

        return ParseRenewSandboxExpirationResponse(response);
    }

    public async Task<SnapshotInfo> CreateSnapshotAsync(
        string sandboxId,
        CreateSnapshotRequest? request = null,
        CancellationToken cancellationToken = default)
    {
        var response = await _client.PostAsync<JsonElement>(
            $"/sandboxes/{Uri.EscapeDataString(sandboxId)}/snapshots",
            request ?? new CreateSnapshotRequest(),
            cancellationToken).ConfigureAwait(false);

        return ParseSnapshotInfo(response);
    }

    public async Task<SnapshotInfo> GetSnapshotAsync(
        string snapshotId,
        CancellationToken cancellationToken = default)
    {
        var response = await _client.GetAsync<JsonElement>(
            $"/snapshots/{Uri.EscapeDataString(snapshotId)}",
            cancellationToken: cancellationToken).ConfigureAwait(false);

        return ParseSnapshotInfo(response);
    }

    public async Task<ListSnapshotsResponse> ListSnapshotsAsync(
        ListSnapshotsParams? @params = null,
        CancellationToken cancellationToken = default)
    {
        var queryParts = new List<string>();

        var sandboxId = @params?.SandboxId;
        if (!string.IsNullOrWhiteSpace(sandboxId))
        {
            queryParts.Add($"sandboxId={Uri.EscapeDataString(sandboxId)}");
        }

        if (@params?.States != null && @params.States.Count > 0)
        {
            queryParts.AddRange(@params.States.Select(state => $"state={Uri.EscapeDataString(state)}"));
        }

        if (@params?.Page.HasValue == true)
        {
            queryParts.Add($"page={@params.Page.Value}");
        }

        if (@params?.PageSize.HasValue == true)
        {
            queryParts.Add($"pageSize={@params.PageSize.Value}");
        }

        var path = queryParts.Count > 0
            ? $"/snapshots?{string.Join("&", queryParts)}"
            : "/snapshots";

        var response = await _client.GetAsync<JsonElement>(path, cancellationToken: cancellationToken).ConfigureAwait(false);
        return ParseListSnapshotsResponse(response);
    }

    public async Task DeleteSnapshotAsync(
        string snapshotId,
        CancellationToken cancellationToken = default)
    {
        await _client.DeleteAsync($"/snapshots/{Uri.EscapeDataString(snapshotId)}", cancellationToken: cancellationToken).ConfigureAwait(false);
    }

    public async Task<Endpoint> GetSandboxEndpointAsync(
        string sandboxId,
        int port,
        bool useServerProxy = false,
        CancellationToken cancellationToken = default)
    {
        var queryParams = new Dictionary<string, string?>
        {
            ["use_server_proxy"] = useServerProxy ? "true" : "false"
        };

        var response = await _client.GetAsync<JsonElement>(
            $"/sandboxes/{Uri.EscapeDataString(sandboxId)}/endpoints/{port}",
            queryParams,
            cancellationToken: cancellationToken).ConfigureAwait(false);

        return ParseEndpointResponse(response);
    }

    public async Task<Endpoint> GetSignedSandboxEndpointAsync(
        string sandboxId,
        int port,
        long expires,
        bool useServerProxy = false,
        CancellationToken cancellationToken = default)
    {
        var queryParams = new Dictionary<string, string?>
        {
            ["use_server_proxy"] = useServerProxy ? "true" : "false",
            ["expires"] = expires.ToString()
        };

        var response = await _client.GetAsync<JsonElement>(
            $"/sandboxes/{Uri.EscapeDataString(sandboxId)}/endpoints/{port}",
            queryParams,
            cancellationToken: cancellationToken).ConfigureAwait(false);

        return ParseEndpointResponse(response);
    }

    private static Endpoint ParseEndpointResponse(JsonElement response)
    {
        return new Endpoint
        {
            EndpointAddress = response.GetProperty("endpoint").GetString() ?? throw new SandboxApiException("Missing endpoint in response"),
            Headers = response.TryGetProperty("headers", out var headersElement) && headersElement.ValueKind == JsonValueKind.Object
                ? headersElement.EnumerateObject().ToDictionary(p => p.Name, p => p.Value.GetString() ?? string.Empty)
                : new Dictionary<string, string>()
        };
    }

    private static DateTime ParseIsoDate(string fieldName, JsonElement element)
    {
        var value = element.GetString();
        if (string.IsNullOrEmpty(value))
        {
            throw new SandboxApiException($"Invalid {fieldName}: expected ISO string, got null or empty");
        }

        if (!DateTime.TryParse(value, out var date))
        {
            throw new SandboxApiException($"Invalid {fieldName}: {value}");
        }

        return date.ToUniversalTime();
    }

    private static DateTime? ParseOptionalIsoDate(string fieldName, JsonElement element)
    {
        return element.ValueKind == JsonValueKind.Null ? null : ParseIsoDate(fieldName, element);
    }

    private static SandboxInfo ParseSandboxInfo(JsonElement element)
    {
        var status = element.GetProperty("status");

        return new SandboxInfo
        {
            Id = element.GetProperty("id").GetString() ?? throw new SandboxApiException("Missing id in response"),
            Image = element.TryGetProperty("image", out var image) && image.ValueKind == JsonValueKind.Object
                ? new ImageSpec
                {
                    Uri = image.GetProperty("uri").GetString() ?? throw new SandboxApiException("Missing image.uri in response"),
                    Auth = image.TryGetProperty("auth", out var auth) && auth.ValueKind != JsonValueKind.Null
                        ? JsonSerializer.Deserialize<ImageAuth>(auth.GetRawText(), JsonOptions)
                        : null
                }
                : null,
            SnapshotId = element.TryGetProperty("snapshotId", out var snapshotId) && snapshotId.ValueKind != JsonValueKind.Null
                ? snapshotId.GetString()
                : null,
            Platform = element.TryGetProperty("platform", out var platform) && platform.ValueKind == JsonValueKind.Object
                ? JsonSerializer.Deserialize<PlatformSpec>(platform.GetRawText(), JsonOptions)
                : null,
            Entrypoint = element.GetProperty("entrypoint").EnumerateArray().Select(e => e.GetString() ?? string.Empty).ToList(),
            Metadata = element.TryGetProperty("metadata", out var metadata) && metadata.ValueKind == JsonValueKind.Object
                ? metadata.EnumerateObject().ToDictionary(p => p.Name, p => p.Value.GetString() ?? string.Empty)
                : null,
            Status = new SandboxStatus
            {
                State = status.GetProperty("state").GetString() ?? throw new SandboxApiException("Missing status.state in response"),
                Reason = status.TryGetProperty("reason", out var reason) ? reason.GetString() : null,
                Message = status.TryGetProperty("message", out var message) ? message.GetString() : null
            },
            CreatedAt = ParseIsoDate("createdAt", element.GetProperty("createdAt")),
            ExpiresAt = element.TryGetProperty("expiresAt", out var expiresAtElement)
                ? ParseOptionalIsoDate("expiresAt", expiresAtElement)
                : null
        };
    }

    private static CreateSandboxResponse ParseCreateSandboxResponse(JsonElement element)
    {
        var status = element.GetProperty("status");

        return new CreateSandboxResponse
        {
            Id = element.GetProperty("id").GetString() ?? throw new SandboxApiException("Missing id in response"),
            Status = new SandboxStatus
            {
                State = status.GetProperty("state").GetString() ?? throw new SandboxApiException("Missing status.state in response"),
                Reason = status.TryGetProperty("reason", out var reason) ? reason.GetString() : null,
                Message = status.TryGetProperty("message", out var message) ? message.GetString() : null
            },
            Platform = element.TryGetProperty("platform", out var platform) && platform.ValueKind == JsonValueKind.Object
                ? JsonSerializer.Deserialize<PlatformSpec>(platform.GetRawText(), JsonOptions)
                : null,
            Metadata = element.TryGetProperty("metadata", out var metadata) && metadata.ValueKind == JsonValueKind.Object
                ? metadata.EnumerateObject().ToDictionary(p => p.Name, p => p.Value.GetString() ?? string.Empty)
                : null,
            CreatedAt = ParseIsoDate("createdAt", element.GetProperty("createdAt")),
            ExpiresAt = element.TryGetProperty("expiresAt", out var expiresAtElement)
                ? ParseOptionalIsoDate("expiresAt", expiresAtElement)
                : null,
            Entrypoint = element.GetProperty("entrypoint").EnumerateArray().Select(e => e.GetString() ?? string.Empty).ToList()
        };
    }

    private static ListSandboxesResponse ParseListSandboxesResponse(JsonElement element)
    {
        var items = element.GetProperty("items").EnumerateArray().Select(ParseSandboxInfo).ToList();

        PaginationInfo? pagination = null;
        if (element.TryGetProperty("pagination", out var paginationElement) && paginationElement.ValueKind == JsonValueKind.Object)
        {
            pagination = new PaginationInfo
            {
                Page = paginationElement.TryGetProperty("page", out var page) ? page.GetInt32() : 0,
                PageSize = paginationElement.TryGetProperty("pageSize", out var pageSize) ? pageSize.GetInt32() : 0,
                TotalItems = paginationElement.TryGetProperty("totalItems", out var totalItems) ? totalItems.GetInt32() : 0,
                TotalPages = paginationElement.TryGetProperty("totalPages", out var totalPages) ? totalPages.GetInt32() : 0,
                HasNextPage = paginationElement.TryGetProperty("hasNextPage", out var hasNextPage) && hasNextPage.GetBoolean()
            };
        }

        return new ListSandboxesResponse
        {
            Items = items,
            Pagination = pagination
        };
    }

    private static SnapshotInfo ParseSnapshotInfo(JsonElement element)
    {
        var status = element.GetProperty("status");
        return new SnapshotInfo
        {
            Id = element.GetProperty("id").GetString() ?? throw new SandboxApiException("Missing id in response"),
            SandboxId = element.GetProperty("sandboxId").GetString() ?? throw new SandboxApiException("Missing sandboxId in response"),
            Name = element.TryGetProperty("name", out var name) && name.ValueKind != JsonValueKind.Null
                ? name.GetString()
                : null,
            Status = new SnapshotStatus
            {
                State = status.GetProperty("state").GetString() ?? throw new SandboxApiException("Missing status.state in response"),
                Reason = status.TryGetProperty("reason", out var reason) ? reason.GetString() : null,
                Message = status.TryGetProperty("message", out var message) ? message.GetString() : null,
                LastTransitionAt = status.TryGetProperty("lastTransitionAt", out var lastTransitionAt) && lastTransitionAt.ValueKind != JsonValueKind.Null
                    ? ParseIsoDate("lastTransitionAt", lastTransitionAt)
                    : null
            },
            CreatedAt = ParseIsoDate("createdAt", element.GetProperty("createdAt"))
        };
    }

    private static ListSnapshotsResponse ParseListSnapshotsResponse(JsonElement element)
    {
        var items = element.GetProperty("items").EnumerateArray().Select(ParseSnapshotInfo).ToList();

        PaginationInfo? pagination = null;
        if (element.TryGetProperty("pagination", out var paginationElement) && paginationElement.ValueKind == JsonValueKind.Object)
        {
            pagination = new PaginationInfo
            {
                Page = paginationElement.TryGetProperty("page", out var page) ? page.GetInt32() : 0,
                PageSize = paginationElement.TryGetProperty("pageSize", out var pageSize) ? pageSize.GetInt32() : 0,
                TotalItems = paginationElement.TryGetProperty("totalItems", out var totalItems) ? totalItems.GetInt32() : 0,
                TotalPages = paginationElement.TryGetProperty("totalPages", out var totalPages) ? totalPages.GetInt32() : 0,
                HasNextPage = paginationElement.TryGetProperty("hasNextPage", out var hasNextPage) && hasNextPage.GetBoolean()
            };
        }

        return new ListSnapshotsResponse
        {
            Items = items,
            Pagination = pagination
        };
    }

    private static RenewSandboxExpirationResponse ParseRenewSandboxExpirationResponse(JsonElement element)
    {
        DateTime? expiresAt = null;
        if (element.TryGetProperty("expiresAt", out var expiresAtElement) && expiresAtElement.ValueKind == JsonValueKind.String)
        {
            expiresAt = ParseIsoDate("expiresAt", expiresAtElement);
        }

        return new RenewSandboxExpirationResponse
        {
            ExpiresAt = expiresAt
        };
    }
}
