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

using System.Net;
using System.Text;
using System.Text.Json;
using FluentAssertions;
using OpenSandbox.Adapters;
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
          "status": { "state": "Running" },
          "createdAt": "2026-03-14T12:00:00Z"
        }
        """;
        var adapter = CreateAdapterWithJsonResponse(payload);

        SandboxInfo sandbox = await adapter.GetSandboxAsync("sbx-1");

        sandbox.ExpiresAt.Should().BeNull();
        sandbox.Platform.Should().NotBeNull();
        sandbox.Platform!.Arch.Should().Be("amd64");
    }

    [Fact]
    public async Task CreateSandboxAsync_ShouldTreatMissingExpiresAtAsNull()
    {
        var payload = """
        {
          "id": "sbx-2",
          "status": { "state": "Pending" },
          "platform": { "os": "linux", "arch": "arm64" },
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

    private static SandboxesAdapter CreateAdapterWithJsonResponse(string payload)
    {
        var handler = new StaticJsonHandler(payload);
        var client = new HttpClient(handler);
        var wrapper = new HttpClientWrapper(client, "http://localhost:8080/v1");
        return new SandboxesAdapter(wrapper);
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
}
