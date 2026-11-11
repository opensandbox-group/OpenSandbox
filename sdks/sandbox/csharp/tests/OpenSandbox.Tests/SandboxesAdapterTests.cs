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

using System.Net;
using System.Text;
using System.Text.Json;
using FluentAssertions;
using OpenSandbox.Adapters;
using OpenSandbox.Core;
using OpenSandbox.Internal;
using OpenSandbox.Models;
using Xunit;

namespace OpenSandbox.Tests;

public class SandboxesAdapterTests
{
    [Fact]
    public async Task GetSandboxEndpointAsync_ShouldIncludeUseServerProxyQueryParam()
    {
        // Arrange
        var handler = new CaptureHandler();
        var client = new HttpClient(handler);
        var wrapper = new HttpClientWrapper(client, "http://localhost:8080/v1");
        var adapter = new SandboxesAdapter(wrapper);

        // Act
        _ = await adapter.GetSandboxEndpointAsync("sbx-1", 44772, useServerProxy: true);

        // Assert
        handler.LastRequestUri.Should().NotBeNull();
        handler.LastRequestUri!.PathAndQuery.Should().Contain("/sandboxes/sbx-1/endpoints/44772");
        handler.LastRequestUri!.Query.Should().Contain("use_server_proxy=true");
    }

    [Fact]
    public async Task GetSandboxEndpointAsync_ShouldDefaultUseServerProxyToFalse()
    {
        // Arrange
        var handler = new CaptureHandler();
        var client = new HttpClient(handler);
        var wrapper = new HttpClientWrapper(client, "http://localhost:8080/v1");
        var adapter = new SandboxesAdapter(wrapper);

        // Act
        _ = await adapter.GetSandboxEndpointAsync("sbx-2", 44772);

        // Assert
        handler.LastRequestUri.Should().NotBeNull();
        handler.LastRequestUri!.Query.Should().Contain("use_server_proxy=false");
    }

    [Fact]
    public async Task GetSandboxAsync_ShouldTreatMissingExpiresAtAsNull()
    {
        var payload = """
        {
          "id": "sbx-1",
          "image": { "uri": "python:3.11" },
          "platform": { "os": "linux", "arch": "amd64" },
          "entrypoint": ["python"],
          "extensions": { "opensandbox.extensions.custom-label": "中文数据" },
          "status": { "state": "Running" },
          "createdAt": "2026-03-14T12:00:00Z"
        }
        """;
        var adapter = CreateAdapterWithJsonResponse(payload);

        SandboxInfo sandbox = await adapter.GetSandboxAsync("sbx-1");

        sandbox.ExpiresAt.Should().BeNull();
        sandbox.Allocation.Should().BeNull();
        sandbox.Platform.Should().NotBeNull();
        sandbox.Platform!.Arch.Should().Be("amd64");
        sandbox.Extensions.Should().ContainKey("opensandbox.extensions.custom-label")
            .WhoseValue.Should().Be("中文数据");
    }

    [Fact]
    public async Task GetSandboxAsync_ShouldParseAllocation()
    {
        const string payload = """
        {
          "id": "sbx-pool",
          "status": { "state": "Running" },
          "entrypoint": ["/bin/sh"],
          "createdAt": "2026-03-14T12:00:00Z",
          "allocation": {
            "mode": "pool",
            "poolRef": "default/python",
            "state": "allocated"
          }
        }
        """;
        var adapter = CreateAdapterWithJsonResponse(payload);

        SandboxInfo sandbox = await adapter.GetSandboxAsync("sbx-pool");

        sandbox.Allocation.Should().NotBeNull();
        sandbox.Allocation!.Mode.Should().Be("pool");
        sandbox.Allocation.PoolRef.Should().Be("default/python");
        sandbox.Allocation.State.Should().Be("allocated");
    }

    [Fact]
    public async Task ListSandboxesAsync_ShouldParseAllocation()
    {
        const string payload = """
        {
          "items": [
            {
              "id": "sbx-pool",
              "status": { "state": "Running" },
              "entrypoint": ["/bin/sh"],
              "createdAt": "2026-03-14T12:00:00Z",
              "allocation": {
                "mode": "pool",
                "poolRef": "default/python",
                "state": "allocated"
              }
            },
            {
              "id": "sbx-legacy",
              "status": { "state": "Running" },
              "entrypoint": ["/bin/sh"],
              "createdAt": "2026-03-14T12:00:00Z"
            }
          ]
        }
        """;
        var adapter = CreateAdapterWithJsonResponse(payload);

        ListSandboxesResponse response = await adapter.ListSandboxesAsync();

        response.Items[0].Allocation.Should().NotBeNull();
        response.Items[0].Allocation!.PoolRef.Should().Be("default/python");
        response.Items[1].Allocation.Should().BeNull();
    }

    [Fact]
    public async Task ListSandboxesAsync_ShouldEncodeMetadataFilterRoundTrip()
    {
        var handler = new CapturingHandler("""{"items": []}""");
        var client = new HttpClient(handler);
        var wrapper = new HttpClientWrapper(client, "http://localhost:8080/v1");
        var adapter = new SandboxesAdapter(wrapper);

        await adapter.ListSandboxesAsync(new ListSandboxesParams
        {
            Metadata = new Dictionary<string, string> { ["team"] = "platform&a=1" }
        });

        ParseQsl(ExtractQueryValue(handler.PathAndQuery!, "metadata"))
            .Should().ContainSingle().Which.Should().Be(new KeyValuePair<string, string>("team", "platform&a=1"));
    }

    [Fact]
    public async Task CreateSandboxAsync_ShouldTreatMissingExpiresAtAsNull()
    {
        var payload = """
        {
          "id": "sbx-2",
          "status": { "state": "Pending" },
          "platform": { "os": "linux", "arch": "arm64" },
          "extensions": { "opensandbox.extensions.custom-label": "中文数据" },
          "createdAt": "2026-03-14T12:00:00Z",
          "entrypoint": ["python"]
        }
        """;
        var adapter = CreateAdapterWithJsonResponse(payload);

        CreateSandboxResponse response = await adapter.CreateSandboxAsync(new CreateSandboxRequest
        {
            Image = new ImageSpec { Uri = "python:3.11" },
            ResourceLimits = new Dictionary<string, string>(),
            Entrypoint = new List<string> { "python" }
        });

        response.ExpiresAt.Should().BeNull();
        response.Platform.Should().NotBeNull();
        response.Platform!.Arch.Should().Be("arm64");
        response.Extensions.Should().ContainKey("opensandbox.extensions.custom-label")
            .WhoseValue.Should().Be("中文数据");
    }

    [Fact]
    public async Task CreateSandboxAsync_ShouldSerializeSecureAccess()
    {
        var handler = new CaptureCreateRequestHandler();
        var client = new HttpClient(handler);
        var wrapper = new HttpClientWrapper(client, "http://localhost:8080/v1");
        var adapter = new SandboxesAdapter(wrapper);

        _ = await adapter.CreateSandboxAsync(new CreateSandboxRequest
        {
            Image = new ImageSpec { Uri = "python:3.11" },
            ResourceLimits = new Dictionary<string, string>(),
            Entrypoint = new List<string> { "python" },
            SecureAccess = true
        });

        handler.RequestBody.Should().NotBeNullOrEmpty();
        using var json = JsonDocument.Parse(handler.RequestBody!);
        json.RootElement.GetProperty("secureAccess").GetBoolean().Should().BeTrue();
    }

    [Fact]
    public async Task CreateSandboxAsync_ShouldSerializeLifecycleHooks()
    {
        var handler = new CaptureCreateRequestHandler();
        var client = new HttpClient(handler);
        var wrapper = new HttpClientWrapper(client, "http://localhost:8080/v1");
        var adapter = new SandboxesAdapter(wrapper);

        _ = await adapter.CreateSandboxAsync(new CreateSandboxRequest
        {
            ResourceLimits = new Dictionary<string, string>(),
            Lifecycle = new SandboxLifecycle
            {
                PreStart = new LifecycleHook
                {
                    Command = ["/opt/hooks/restore.sh"],
                    TimeoutSeconds = 300
                },
                Periodic =
                [
                    new PeriodicLifecycleHook
                    {
                        Name = "checkpoint",
                        Schedule = "@hourly",
                        Command = ["/opt/hooks/checkpoint.sh"]
                    }
                ]
            }
        });

        handler.RequestBody.Should().NotBeNullOrEmpty();
        using var json = JsonDocument.Parse(handler.RequestBody!);
        var lifecycle = json.RootElement.GetProperty("lifecycle");
        var preStart = lifecycle.GetProperty("preStart");
        preStart.GetProperty("command")[0].GetString().Should().Be("/opt/hooks/restore.sh");
        preStart.GetProperty("timeoutSeconds").GetInt32().Should().Be(300);
        var periodic = lifecycle.GetProperty("periodic")[0];
        periodic.GetProperty("name").GetString().Should().Be("checkpoint");
        periodic.GetProperty("schedule").GetString().Should().Be("@hourly");
        periodic.GetProperty("command")[0].GetString().Should().Be("/opt/hooks/checkpoint.sh");
    }

    [Fact]
    public async Task PatchSandboxMetadataAsync_ShouldSendMetadataBodyAndPreserveNull()
    {
        var handler = new CapturePatchMetadataRequestHandler();
        var client = new HttpClient(handler);
        var wrapper = new HttpClientWrapper(client, "http://localhost:8080/v1");
        var adapter = new SandboxesAdapter(wrapper);

        var result = await adapter.PatchSandboxMetadataAsync(
            "sbx-4",
            new Dictionary<string, string?> { ["team"] = "platform", ["old"] = null });

        handler.Method.Should().Be(HttpMethod.Patch);
        handler.PathAndQuery.Should().Be("/v1/sandboxes/sbx-4/metadata");
        handler.RequestBody.Should().NotBeNullOrEmpty();
        using var json = JsonDocument.Parse(handler.RequestBody!);
        json.RootElement.GetProperty("team").GetString().Should().Be("platform");
        json.RootElement.GetProperty("old").ValueKind.Should().Be(JsonValueKind.Null);
        result.Metadata.Should().ContainKey("team").WhoseValue.Should().Be("platform");
    }

    [Fact]
    public async Task ListSnapshotsAsync_ShouldIncludeExactNameFilter()
    {
        var handler = new CaptureListSnapshotsHandler();
        var client = new HttpClient(handler);
        var wrapper = new HttpClientWrapper(client, "http://localhost:8080/v1");
        var adapter = new SandboxesAdapter(wrapper);

        _ = await adapter.ListSnapshotsAsync(new ListSnapshotsParams
        {
            Name = "toolchain:csharp@rev-1"
        });

        handler.PathAndQuery.Should().Be(
            "/v1/snapshots?name=toolchain%3Acsharp%40rev-1");
    }

    [Fact]
    public async Task GetSandboxEndpointAsync_ShouldCaptureOriginResponseHeader()
    {
        var handler = new CaptureEndpointHandler(origin: SandboxOrigin.Template);
        var client = new HttpClient(handler);
        var wrapper = new HttpClientWrapper(client, "http://localhost:8080/v1");
        var adapter = new SandboxesAdapter(wrapper);

        var endpoint = await adapter.GetSandboxEndpointAsync("fsb-1", 44772);

        endpoint.Origin.Should().Be(SandboxOrigin.Template);
    }

    [Fact]
    public async Task GetSandboxEndpointAsync_ShouldTreatMissingOriginHeaderAsNull()
    {
        var handler = new CaptureEndpointHandler(origin: null);
        var client = new HttpClient(handler);
        var wrapper = new HttpClientWrapper(client, "http://localhost:8080/v1");
        var adapter = new SandboxesAdapter(wrapper);

        var endpoint = await adapter.GetSandboxEndpointAsync("sbx-1", 44772);

        endpoint.Origin.Should().BeNull();
    }

    [Fact]
    public async Task CreateSandboxAsync_ShouldSerializeTemplateId()
    {
        var handler = new CaptureCreateRequestHandler();
        var client = new HttpClient(handler);
        var wrapper = new HttpClientWrapper(client, "http://localhost:8080/v1");
        var adapter = new SandboxesAdapter(wrapper);

        _ = await adapter.CreateSandboxAsync(new CreateSandboxRequest
        {
            TemplateId = "tpl-123",
            Timeout = 600,
            Metadata = new Dictionary<string, string> { ["team"] = "platform" }
        });

        handler.RequestBody.Should().NotBeNullOrEmpty();
        using var json = JsonDocument.Parse(handler.RequestBody!);
        json.RootElement.GetProperty("templateId").GetString().Should().Be("tpl-123");
        json.RootElement.GetProperty("timeout").GetInt32().Should().Be(600);
        json.RootElement.TryGetProperty("image", out _).Should().BeFalse();
        json.RootElement.TryGetProperty("resourceLimits", out _).Should().BeFalse();
        json.RootElement.TryGetProperty("entrypoint", out _).Should().BeFalse();
    }

    [Fact]
    public async Task CreateTemplateAsync_ShouldPostAndParseTemplate()
    {
        const string payload = """
        {
          "templateId": "tpl-1",
          "image": "python:3.11",
          "publish": "s3://bucket/publish",
          "format": "overlaybd",
          "status": { "phase": "Pending" },
          "createdAt": "2026-03-14T12:00:00Z",
          "updatedAt": "2026-03-14T12:00:00Z",
          "resourceLimits": { "cpu": "1", "memory": "512Mi" },
          "entrypoint": ["tail", "-f", "/dev/null"],
          "env": { "LOG_LEVEL": "info" },
          "metadata": { "team": "platform" },
          "readiness": { "probe": "tcp://127.0.0.1:44772", "warmupSeconds": 30 }
        }
        """;
        var handler = new CapturingHandler(payload);
        var client = new HttpClient(handler);
        var wrapper = new HttpClientWrapper(client, "http://localhost:8080/v1");
        var adapter = new SandboxesAdapter(wrapper);

        var template = await adapter.CreateTemplateAsync(new CreateTemplateRequest
        {
            Image = "python:3.11",
            Publish = "s3://bucket/publish",
            Env = new Dictionary<string, string> { ["LOG_LEVEL"] = "info" },
            Metadata = new Dictionary<string, string> { ["team"] = "platform" }
        });

        handler.Method.Should().Be(HttpMethod.Post);
        handler.PathAndQuery.Should().Be("/v1/templates");
        handler.RequestBody.Should().NotBeNullOrEmpty();
        using var json = JsonDocument.Parse(handler.RequestBody!);
        json.RootElement.GetProperty("image").GetString().Should().Be("python:3.11");
        json.RootElement.GetProperty("publish").GetString().Should().Be("s3://bucket/publish");
        json.RootElement.GetProperty("env").GetProperty("LOG_LEVEL").GetString().Should().Be("info");
        json.RootElement.GetProperty("metadata").GetProperty("team").GetString().Should().Be("platform");

        template.TemplateId.Should().Be("tpl-1");
        template.Image.Should().Be("python:3.11");
        template.Publish.Should().Be("s3://bucket/publish");
        template.Format.Should().Be("overlaybd");
        template.Status.Phase.Should().Be(TemplatePhases.Pending);
        template.Status.ManifestRef.Should().BeNull();
        template.ResourceLimits.Should().ContainKey("cpu").WhoseValue.Should().Be("1");
        template.Entrypoint.Should().Equal("tail", "-f", "/dev/null");
        template.Env.Should().ContainKey("LOG_LEVEL").WhoseValue.Should().Be("info");
        template.Metadata.Should().ContainKey("team").WhoseValue.Should().Be("platform");
        template.Readiness.Should().NotBeNull();
        template.Readiness!.Probe.Should().Be("tcp://127.0.0.1:44772");
        template.Readiness.WarmupSeconds.Should().Be(30);
    }

    [Fact]
    public async Task GetTemplateAsync_ShouldParseSucceededStatus()
    {
        const string payload = """
        {
          "templateId": "tpl-1",
          "image": "python:3.11",
          "publish": "s3://bucket/publish",
          "format": "overlaybd",
          "status": { "phase": "Succeeded", "manifestRef": "s3://bucket/publish/manifest.json" },
          "createdAt": "2026-03-14T12:00:00Z",
          "updatedAt": "2026-03-14T12:01:00Z"
        }
        """;
        var handler = new CapturingHandler(payload);
        var client = new HttpClient(handler);
        var wrapper = new HttpClientWrapper(client, "http://localhost:8080/v1");
        var adapter = new SandboxesAdapter(wrapper);

        var template = await adapter.GetTemplateAsync("tpl-1");

        handler.Method.Should().Be(HttpMethod.Get);
        handler.PathAndQuery.Should().Be("/v1/templates/tpl-1");
        template.Status.Phase.Should().Be(TemplatePhases.Succeeded);
        template.Status.ManifestRef.Should().Be("s3://bucket/publish/manifest.json");
        template.Readiness.Should().BeNull();
        template.Entrypoint.Should().BeNull();
    }

    [Fact]
    public async Task ListTemplatesAsync_ShouldEncodeMetadataFilterAndParsePage()
    {
        const string payload = """
        {
          "items": [
            {
              "templateId": "tpl-1",
              "image": "python:3.11",
              "publish": "s3://bucket/publish",
              "format": "overlaybd",
              "status": { "phase": "Building" },
              "createdAt": "2026-03-14T12:00:00Z",
              "updatedAt": "2026-03-14T12:00:00Z"
            }
          ],
          "pagination": {
            "page": 2,
            "pageSize": 20,
            "totalItems": 21,
            "totalPages": 2,
            "hasNextPage": false
          }
        }
        """;
        var handler = new CapturingHandler(payload);
        var client = new HttpClient(handler);
        var wrapper = new HttpClientWrapper(client, "http://localhost:8080/v1");
        var adapter = new SandboxesAdapter(wrapper);

        var response = await adapter.ListTemplatesAsync(new ListTemplatesParams
        {
            Metadata = new Dictionary<string, string> { ["team"] = "platform&a=1" },
            Page = 2,
            PageSize = 20
        });

        // Keys and values must survive the server's parse_qsl round trip,
        // so each key/value is percent-encoded before joining.
        handler.PathAndQuery.Should().Be(
            "/v1/templates?metadata=team%3Dplatform%2526a%253D1&page=2&pageSize=20");

        // Simulate the server: decode the query value, split with parse_qsl,
        // then decode each key/value.
        ParseQsl(ExtractQueryValue(handler.PathAndQuery!, "metadata"))
            .Should().ContainSingle().Which.Should().Be(new KeyValuePair<string, string>("team", "platform&a=1"));

        response.Items.Should().ContainSingle();
        response.Items[0].Status.Phase.Should().Be(TemplatePhases.Building);
        response.Pagination!.TotalItems.Should().Be(21);
        response.Pagination.HasNextPage.Should().BeFalse();
    }

    [Fact]
    public async Task DeleteTemplateAsync_ShouldSendDeleteRequest()
    {
        var handler = new CapturingHandler("");
        var client = new HttpClient(handler);
        var wrapper = new HttpClientWrapper(client, "http://localhost:8080/v1");
        var adapter = new SandboxesAdapter(wrapper);

        await adapter.DeleteTemplateAsync("tpl-1");

        handler.Method.Should().Be(HttpMethod.Delete);
        handler.PathAndQuery.Should().Be("/v1/templates/tpl-1");
    }

    private static SandboxesAdapter CreateAdapterWithJsonResponse(string payload)
    {
        var handler = new StaticJsonHandler(payload);
        var client = new HttpClient(handler);
        var wrapper = new HttpClientWrapper(client, "http://localhost:8080/v1");
        return new SandboxesAdapter(wrapper);
    }

    private static string ExtractQueryValue(string pathAndQuery, string key)
    {
        var query = pathAndQuery.Substring(pathAndQuery.IndexOf('?') + 1);
        var part = query.Split('&').Single(p => p.StartsWith($"{key}=", StringComparison.Ordinal));
        return Uri.UnescapeDataString(part.Substring(key.Length + 1));
    }

    /// <summary>
    /// Mimics the server: split the (already query-decoded) value with
    /// parse_qsl semantics, then percent-decode each key and value.
    /// </summary>
    private static List<KeyValuePair<string, string>> ParseQsl(string value)
    {
        return value.Split('&').Select(pair =>
        {
            var separator = pair.IndexOf('=');
            return new KeyValuePair<string, string>(
                Uri.UnescapeDataString(pair[..separator]),
                Uri.UnescapeDataString(pair[(separator + 1)..]));
        }).ToList();
    }

    private sealed class CaptureHandler : HttpMessageHandler
    {
        public Uri? LastRequestUri { get; private set; }

        protected override Task<HttpResponseMessage> SendAsync(HttpRequestMessage request, CancellationToken cancellationToken)
        {
            LastRequestUri = request.RequestUri;
            var payload = "{\"endpoint\":\"example.internal:44772\",\"headers\":{}}";
            var response = new HttpResponseMessage(HttpStatusCode.OK)
            {
                Content = new StringContent(payload, Encoding.UTF8, "application/json")
            };
            return Task.FromResult(response);
        }
    }

    private sealed class StaticJsonHandler(string payload) : HttpMessageHandler
    {
        protected override Task<HttpResponseMessage> SendAsync(HttpRequestMessage request, CancellationToken cancellationToken)
        {
            var response = new HttpResponseMessage(HttpStatusCode.OK)
            {
                Content = new StringContent(payload, Encoding.UTF8, "application/json")
            };
            return Task.FromResult(response);
        }
    }

    private sealed class CaptureCreateRequestHandler : HttpMessageHandler
    {
        public string? RequestBody { get; private set; }

        protected override async Task<HttpResponseMessage> SendAsync(HttpRequestMessage request, CancellationToken cancellationToken)
        {
            RequestBody = request.Content is null
                ? null
                : await request.Content.ReadAsStringAsync();
            var payload = """
            {
              "id": "sbx-3",
              "status": { "state": "Pending" },
              "createdAt": "2026-03-14T12:00:00Z",
              "entrypoint": ["python"]
            }
            """;
            var response = new HttpResponseMessage(HttpStatusCode.OK)
            {
                Content = new StringContent(payload, Encoding.UTF8, "application/json")
            };
            return response;
        }
    }

    private sealed class CapturePatchMetadataRequestHandler : HttpMessageHandler
    {
        public HttpMethod? Method { get; private set; }
        public string? PathAndQuery { get; private set; }
        public string? RequestBody { get; private set; }

        protected override async Task<HttpResponseMessage> SendAsync(HttpRequestMessage request, CancellationToken cancellationToken)
        {
            Method = request.Method;
            PathAndQuery = request.RequestUri?.PathAndQuery;
            RequestBody = request.Content is null
                ? null
                : await request.Content.ReadAsStringAsync();
            var payload = """
            {
              "id": "sbx-4",
              "status": { "state": "Running" },
              "metadata": { "team": "platform" },
              "createdAt": "2026-03-14T12:00:00Z",
              "entrypoint": ["python"]
            }
            """;
            var response = new HttpResponseMessage(HttpStatusCode.OK)
            {
                Content = new StringContent(payload, Encoding.UTF8, "application/json")
            };
            return response;
        }
    }

    private sealed class CaptureListSnapshotsHandler : HttpMessageHandler
    {
        public string? PathAndQuery { get; private set; }

        protected override Task<HttpResponseMessage> SendAsync(
            HttpRequestMessage request,
            CancellationToken cancellationToken)
        {
            PathAndQuery = request.RequestUri?.PathAndQuery;
            const string payload = """
            {
              "items": [],
              "pagination": {
                "page": 1,
                "pageSize": 20,
                "totalItems": 0,
                "totalPages": 0,
                "hasNextPage": false
              }
            }
            """;
            var response = new HttpResponseMessage(HttpStatusCode.OK)
            {
                Content = new StringContent(payload, Encoding.UTF8, "application/json")
            };
            return Task.FromResult(response);
        }
    }

    private sealed class CaptureEndpointHandler(string? origin) : HttpMessageHandler
    {
        protected override Task<HttpResponseMessage> SendAsync(
            HttpRequestMessage request,
            CancellationToken cancellationToken)
        {
            const string payload = "{\"endpoint\":\"example.internal:44772\",\"headers\":{}}";
            var response = new HttpResponseMessage(HttpStatusCode.OK)
            {
                Content = new StringContent(payload, Encoding.UTF8, "application/json")
            };
            if (origin != null)
            {
                response.Headers.Add(Constants.SandboxOriginHeader, origin);
            }

            return Task.FromResult(response);
        }
    }

    private sealed class CapturingHandler(string payload) : HttpMessageHandler
    {
        public HttpMethod? Method { get; private set; }

        public string? PathAndQuery { get; private set; }

        public string? RequestBody { get; private set; }

        protected override async Task<HttpResponseMessage> SendAsync(
            HttpRequestMessage request,
            CancellationToken cancellationToken)
        {
            Method = request.Method;
            PathAndQuery = request.RequestUri?.PathAndQuery;
            RequestBody = request.Content is null
                ? null
                : await request.Content.ReadAsStringAsync();
            var response = new HttpResponseMessage(HttpStatusCode.OK)
            {
                Content = new StringContent(payload, Encoding.UTF8, "application/json")
            };
            return response;
        }
    }
}
