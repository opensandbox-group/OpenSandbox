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

using System.Net.Http.Headers;
using System.Runtime.CompilerServices;
using System.Text;
using System.Text.Json;
using OpenSandbox.Core;
using OpenSandbox.Internal;
using OpenSandbox.Models;
using OpenSandbox.Services;

namespace OpenSandbox.Adapters;

/// <summary>
/// Adapter for the execd filesystem service.
/// </summary>
internal sealed class FilesystemAdapter : ISandboxFiles
{
    private readonly HttpClientWrapper _client;
    private readonly HttpClient _httpClient;
    private readonly string _baseUrl;
    private readonly IReadOnlyDictionary<string, string> _headers;

    private static readonly JsonSerializerOptions JsonOptions = new()
    {
        PropertyNamingPolicy = JsonNamingPolicy.CamelCase,
        PropertyNameCaseInsensitive = true,
        DefaultIgnoreCondition = System.Text.Json.Serialization.JsonIgnoreCondition.WhenWritingNull
    };

    public FilesystemAdapter(
        HttpClientWrapper client,
        HttpClient httpClient,
        string baseUrl,
        IReadOnlyDictionary<string, string> headers)
    {
        _client = client ?? throw new ArgumentNullException(nameof(client));
        _httpClient = httpClient ?? throw new ArgumentNullException(nameof(httpClient));
        _baseUrl = baseUrl?.TrimEnd('/') ?? throw new ArgumentNullException(nameof(baseUrl));
        _headers = headers ?? new Dictionary<string, string>();
    }

    public async Task<IReadOnlyDictionary<string, SandboxFileInfo>> GetFileInfoAsync(
        IEnumerable<string> paths,
        CancellationToken cancellationToken = default)
    {
        var pathWithQuery = BuildRepeatedPathQuery("/files/info", "path", paths);
        var response = await _client.GetAsync<JsonElement>(pathWithQuery, cancellationToken: cancellationToken).ConfigureAwait(false);
        return ParseFilesInfoResponse(response);
    }

    public async Task<IReadOnlyList<SandboxFileInfo>> SearchAsync(
        SearchEntry entry,
        CancellationToken cancellationToken = default)
    {
        var queryParams = new Dictionary<string, string?>
        {
            ["path"] = entry.Path,
            ["pattern"] = entry.Pattern
        };

        var response = await _client.GetAsync<JsonElement>("/files/search", queryParams, cancellationToken).ConfigureAwait(false);
        return ParseSearchFilesResponse(response);
    }

    public async Task CreateDirectoriesAsync(
        IEnumerable<CreateDirectoryEntry> entries,
        CancellationToken cancellationToken = default)
    {
        var body = entries.ToDictionary(
            e => e.Path,
            e => new Permission
            {
                Mode = e.Mode ?? 755,
                Owner = e.Owner,
                Group = e.Group
            });

        await _client.PostAsync("/directories", body, cancellationToken).ConfigureAwait(false);
    }

    public async Task DeleteDirectoriesAsync(
        IEnumerable<string> paths,
        CancellationToken cancellationToken = default)
    {
        var pathWithQuery = BuildRepeatedPathQuery("/directories", "path", paths);
        await _client.DeleteAsync(pathWithQuery, cancellationToken: cancellationToken).ConfigureAwait(false);
    }

    public async Task<IReadOnlyList<SandboxFileInfo>> ListDirectoryAsync(
        string path,
        int? depth = null,
        CancellationToken cancellationToken = default)
    {
        var queryParams = new Dictionary<string, string?>
        {
            ["path"] = path
        };
        if (depth.HasValue)
        {
            queryParams["depth"] = depth.Value.ToString();
        }

        var response = await _client.GetAsync<JsonElement>("/directories/list", queryParams, cancellationToken).ConfigureAwait(false);
        return ParseSearchFilesResponse(response);
    }

    public async Task WriteFilesAsync(
        IEnumerable<WriteEntry> entries,
        CancellationToken cancellationToken = default)
    {
        var entryList = entries.ToList();
        if (entryList.Count == 0)
        {
            return;
        }
        var url = $"{_baseUrl}/files/upload";

        using var form = new MultipartFormDataContent();
        foreach (var entry in entryList)
        {
            var fileName = GetFileName(entry.Path);
            var metadata = new FileMetadata
            {
                Path = entry.Path,
                Mode = entry.Mode,
                Owner = entry.Owner,
                Group = entry.Group
            };

            var metadataJson = JsonSerializer.Serialize(metadata, JsonOptions);
            var metadataContent = new StringContent(metadataJson, Encoding.UTF8, "application/json");
            form.Add(metadataContent, "metadata", "metadata");

            var fileContent = CreateFileContent(entry.Data);
            form.Add(fileContent, "file", fileName);
        }

        using var request = new HttpRequestMessage(HttpMethod.Post, url)
        {
            Content = form
        };

        foreach (var header in _headers)
        {
            request.Headers.TryAddWithoutValidation(header.Key, header.Value);
        }

        using var response = await _httpClient.SendAsync(request, cancellationToken).ConfigureAwait(false);

        if (!response.IsSuccessStatusCode)
        {
            var content = await response.Content.ReadAsStringAsync().ConfigureAwait(false);
            var requestId = response.Headers.TryGetValues(Constants.RequestIdHeader, out var values)
                ? values.FirstOrDefault()
                : null;

            throw new SandboxApiException(
                message: $"Upload failed (status={(int)response.StatusCode})",
                statusCode: (int)response.StatusCode,
                requestId: requestId,
                rawBody: content);
        }
    }

    public async Task<string> ReadFileAsync(
        string path,
        ReadFileOptions? options = null,
        CancellationToken cancellationToken = default)
    {
        var bytes = await ReadBytesAsync(path, new ReadBytesOptions { Range = options?.Range, Offset = options?.Offset, Limit = options?.Limit }, cancellationToken).ConfigureAwait(false);
        var encoding = GetEncoding(options?.Encoding ?? "utf-8");
        return encoding.GetString(bytes);
    }

    public async Task<byte[]> ReadBytesAsync(
        string path,
        ReadBytesOptions? options = null,
        CancellationToken cancellationToken = default)
    {
        var headers = new Dictionary<string, string>();
        var range = options?.Range;
        if (range != null && range.Length > 0)
        {
            headers["Range"] = range;
        }

        var queryParams = new Dictionary<string, string?>
        {
            ["path"] = path
        };

        if (options?.Offset != null)
        {
            queryParams["offset"] = options.Offset.Value.ToString();
        }
        if (options?.Limit != null)
        {
            queryParams["limit"] = options.Limit.Value.ToString();
        }

        return await _client.GetBytesAsync("/files/download", queryParams, headers, cancellationToken).ConfigureAwait(false);
    }

    public async IAsyncEnumerable<byte[]> ReadBytesStreamAsync(
        string path,
        ReadBytesOptions? options = null,
        [EnumeratorCancellation] CancellationToken cancellationToken = default)
    {
        var url = $"{_baseUrl}/files/download?path={Uri.EscapeDataString(path)}";
        if (options?.Offset != null)
        {
            url += $"&offset={options.Offset.Value}";
        }
        if (options?.Limit != null)
        {
            url += $"&limit={options.Limit.Value}";
        }

        using var request = new HttpRequestMessage(HttpMethod.Get, url);
        foreach (var header in _headers)
        {
            request.Headers.TryAddWithoutValidation(header.Key, header.Value);
        }

        var range = options?.Range;
        if (range != null && range.Length > 0)
        {
            request.Headers.TryAddWithoutValidation("Range", range);
        }

        using var response = await _httpClient.SendAsync(request, HttpCompletionOption.ResponseHeadersRead, cancellationToken).ConfigureAwait(false);

        if (!response.IsSuccessStatusCode)
        {
            var content = await response.Content.ReadAsStringAsync().ConfigureAwait(false);
            var requestId = response.Headers.TryGetValues(Constants.RequestIdHeader, out var values)
                ? values.FirstOrDefault()
                : null;

            throw new SandboxApiException(
                message: "Download stream failed",
                statusCode: (int)response.StatusCode,
                requestId: requestId,
                rawBody: content);
        }

        var stream = await response.Content.ReadAsStreamAsync().ConfigureAwait(false);
        var buffer = new byte[8192];
        int bytesRead;

        while ((bytesRead = await stream.ReadAsync(buffer, 0, buffer.Length, cancellationToken).ConfigureAwait(false)) > 0)
        {
            var chunk = new byte[bytesRead];
            Array.Copy(buffer, chunk, bytesRead);
            yield return chunk;
        }
    }

    public async Task DeleteFilesAsync(
        IEnumerable<string> paths,
        CancellationToken cancellationToken = default)
    {
        var pathWithQuery = BuildRepeatedPathQuery("/files", "path", paths);
        await _client.DeleteAsync(pathWithQuery, cancellationToken: cancellationToken).ConfigureAwait(false);
    }

    public async Task MoveFilesAsync(
        IEnumerable<MoveEntry> entries,
        CancellationToken cancellationToken = default)
    {
        var body = entries.Select(e => new RenameFileItem
        {
            Src = e.Src,
            Dest = e.Dest
        }).ToList();

        await _client.PostAsync("/files/mv", body, cancellationToken).ConfigureAwait(false);
    }

    public async Task ReplaceContentsAsync(
        IEnumerable<ContentReplaceEntry> entries,
        CancellationToken cancellationToken = default)
    {
        var body = entries.ToDictionary(
            e => e.Path,
            e => new ReplaceFileContentItem
            {
                Old = e.OldContent,
                New = e.NewContent
            });

        await _client.PostAsync("/files/replace", body, cancellationToken).ConfigureAwait(false);
    }

    public async Task<IReadOnlyList<ContentReplaceResult>> ReplaceContentsDetailedAsync(
        IEnumerable<ContentReplaceEntry> entries,
        CancellationToken cancellationToken = default)
    {
        var body = entries.ToDictionary(
            e => e.Path,
            e => new ReplaceFileContentItem
            {
                Old = e.OldContent,
                New = e.NewContent
            });

        var response = await _client.PostAsync<Dictionary<string, ReplaceFileContentResult>>(
            "/files/replace?verbose=true", body, cancellationToken).ConfigureAwait(false);

        return response.Select(kv => new ContentReplaceResult
        {
            Path = kv.Key,
            ReplacedCount = kv.Value.ReplacedCount,
        }).ToList();
    }

    public async Task SetPermissionsAsync(
        IEnumerable<SetPermissionEntry> entries,
        CancellationToken cancellationToken = default)
    {
        var body = entries.ToDictionary(
            e => e.Path,
            e => new Permission
            {
                Mode = e.Mode,
                Owner = e.Owner,
                Group = e.Group
            });

        await _client.PostAsync("/files/permissions", body, cancellationToken).ConfigureAwait(false);
    }

    private static HttpContent CreateFileContent(object? data)
    {
        return data switch
        {
            null => new ByteArrayContent(Array.Empty<byte>()),
            string str => new StringContent(str, Encoding.UTF8),
            byte[] bytes => new ByteArrayContent(bytes),
            Stream stream => new StreamContent(stream),
            _ => throw new InvalidArgumentException($"Unsupported file data type: {data.GetType().FullName}")
        };
    }

    private static string GetFileName(string path)
    {
        var parts = path.Split('/', '\\');
        return parts.Length > 0 ? parts[^1] : "file";
    }

    private static string BuildRepeatedPathQuery(string route, string key, IEnumerable<string> values)
    {
        var encodedValues = values
            .Where(v => !string.IsNullOrEmpty(v))
            .Select(v => $"{Uri.EscapeDataString(key)}={Uri.EscapeDataString(v)}")
            .ToList();

        if (encodedValues.Count == 0)
        {
            return route;
        }

        return $"{route}?{string.Join("&", encodedValues)}";
    }

    private static Encoding GetEncoding(string encodingName)
    {
        return encodingName.ToLowerInvariant() switch
        {
            "utf-8" or "utf8" => Encoding.UTF8,
            "ascii" => Encoding.ASCII,
            "utf-16" or "utf16" or "unicode" => Encoding.Unicode,
            "utf-32" or "utf32" => Encoding.UTF32,
            _ => Encoding.GetEncoding(encodingName)
        };
    }

    private static IReadOnlyDictionary<string, SandboxFileInfo> ParseFilesInfoResponse(JsonElement element)
    {
        var result = new Dictionary<string, SandboxFileInfo>();

        if (element.ValueKind != JsonValueKind.Object)
            return result;

        foreach (var property in element.EnumerateObject())
        {
            result[property.Name] = ParseFileInfo(property.Value);
        }

        return result;
    }

    private static IReadOnlyList<SandboxFileInfo> ParseSearchFilesResponse(JsonElement element)
    {
        if (element.ValueKind != JsonValueKind.Array)
            return Array.Empty<SandboxFileInfo>();

        return element.EnumerateArray().Select(ParseFileInfo).ToList();
    }

    private static SandboxFileInfo ParseFileInfo(JsonElement element)
    {
        return new SandboxFileInfo
        {
            Path = element.GetProperty("path").GetString() ?? string.Empty,
            Type = element.TryGetProperty("type", out var type) ? type.GetString() : null,
            Size = element.TryGetProperty("size", out var size) && size.ValueKind == JsonValueKind.Number
                ? size.GetInt64()
                : null,
            ModifiedAt = element.TryGetProperty("modified_at", out var modifiedAt) && modifiedAt.ValueKind == JsonValueKind.String
                ? DateTime.TryParse(modifiedAt.GetString(), out var modDate) ? modDate : null
                : null,
            CreatedAt = element.TryGetProperty("created_at", out var createdAt) && createdAt.ValueKind == JsonValueKind.String
                ? DateTime.TryParse(createdAt.GetString(), out var createDate) ? createDate : null
                : null,
            Mode = element.TryGetProperty("mode", out var mode) && mode.ValueKind == JsonValueKind.Number
                ? mode.GetInt32()
                : null,
            Owner = element.TryGetProperty("owner", out var owner) ? owner.GetString() : null,
            Group = element.TryGetProperty("group", out var group) ? group.GetString() : null
        };
    }
}
