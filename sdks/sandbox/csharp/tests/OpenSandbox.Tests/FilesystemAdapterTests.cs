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
using FluentAssertions;
using OpenSandbox.Adapters;
using OpenSandbox.Internal;
using Xunit;

namespace OpenSandbox.Tests;

public class FilesystemAdapterTests
{
    [Fact]
    public async Task WithIdentity_ShouldScopeJsonAndBinaryRequestsWithoutMutatingOriginal()
    {
        var handler = new IdentityCaptureHandler();
        using var client = new HttpClient(handler);
        const string baseUrl = "http://localhost:8080/sandboxes/example/port/44772";
        var headers = new Dictionary<string, string> { ["X-EXECD-ACCESS-TOKEN"] = "secret" };
        var wrapper = new HttpClientWrapper(client, baseUrl, headers);
        var original = new FilesystemAdapter(wrapper, client, baseUrl, headers);
        var scoped = original.WithIdentity(1001, 2000);

        await scoped.GetFileInfoAsync(new[] { "/file" });
        await scoped.ReadBytesAsync("/file");
        await scoped.WriteFilesAsync(new[] { new OpenSandbox.Models.WriteEntry { Path = "/file", Data = new byte[] { 0, 255, 1 } } });
        await original.GetFileInfoAsync(new[] { "/original" });

        handler.Paths.Should().Equal(
            "/sandboxes/example/port/44772/v1/filesystem/1001/2000/files/info",
            "/sandboxes/example/port/44772/v1/filesystem/1001/2000/files/download",
            "/sandboxes/example/port/44772/v1/filesystem/1001/2000/files/upload",
            "/sandboxes/example/port/44772/files/info");
        handler.Tokens.Should().OnlyContain(token => token == "secret");
        Action invalidUid = () => original.WithIdentity(uint.MaxValue, 0);
        invalidUid.Should().Throw<ArgumentOutOfRangeException>();
    }

    [Theory]
    [InlineData(HttpStatusCode.NotFound)]
    [InlineData(HttpStatusCode.NotImplemented)]
    [InlineData(HttpStatusCode.ServiceUnavailable)]
    public async Task WithIdentity_ShouldNeverFallBack(HttpStatusCode status)
    {
        var handler = new IdentityCaptureHandler(status);
        using var client = new HttpClient(handler);
        var wrapper = new HttpClientWrapper(client, "http://localhost:8080");
        var original = new FilesystemAdapter(wrapper, client, wrapper.BaseUrl, new Dictionary<string, string>());
        var scoped = original.WithIdentity(1001, 2000);

        Func<Task> operation = () => scoped.DeleteFilesAsync(new[] { "/file" });
        await operation.Should().ThrowAsync<OpenSandbox.Core.SandboxApiException>();
        handler.Paths.Should().Equal("/v1/filesystem/1001/2000/files");
    }

    private sealed class IdentityCaptureHandler(HttpStatusCode status = HttpStatusCode.OK) : HttpMessageHandler
    {
        public List<string> Paths { get; } = new();
        public List<string> Tokens { get; } = new();

        protected override async Task<HttpResponseMessage> SendAsync(HttpRequestMessage request, CancellationToken cancellationToken)
        {
            Paths.Add(request.RequestUri!.AbsolutePath);
            Tokens.Add(request.Headers.TryGetValues("X-EXECD-ACCESS-TOKEN", out var values) ? values.Single() : "");
            if (request.Content != null)
                await request.Content.ReadAsByteArrayAsync(cancellationToken);
            return new HttpResponseMessage(status)
            {
                Content = new StringContent(status == HttpStatusCode.OK ? "{}" : "{\"code\":\"FILESYSTEM_IDENTITY_UNAVAILABLE\",\"message\":\"unsupported\"}", Encoding.UTF8, "application/json")
            };
        }
    }

    [Fact]
    public async Task ListDirectoryAsync_ShouldParseEntryTypeAndSendDepthZero()
    {
        var payload = """
        [
          {
            "path": "/workspace/link",
            "type": "symlink",
            "size": 11,
            "modified_at": "2026-06-08T10:00:00Z",
            "created_at": "2026-06-08T10:00:00Z",
            "owner": "root",
            "group": "root",
            "mode": 777
          }
        ]
        """;
        var handler = new CaptureJsonHandler(payload);
        using var client = new HttpClient(handler);
        var wrapper = new HttpClientWrapper(client, "http://localhost:8080");
        var adapter = new FilesystemAdapter(wrapper, client, "http://localhost:8080", new Dictionary<string, string>());

        var entries = await adapter.ListDirectoryAsync("/workspace", depth: 0);

        handler.LastRequestUri.Should().NotBeNull();
        handler.LastRequestUri!.PathAndQuery.Should().Contain("/directories/list");
        handler.LastRequestUri!.Query.Should().Contain("path=%2Fworkspace");
        handler.LastRequestUri!.Query.Should().Contain("depth=0");
        entries.Should().ContainSingle();
        entries[0].Type.Should().Be("symlink");
    }

    [Fact]
    public async Task ListDirectoryAsync_ShouldOmitDepthWhenNull()
    {
        var handler = new CaptureJsonHandler("[]");
        using var client = new HttpClient(handler);
        var wrapper = new HttpClientWrapper(client, "http://localhost:8080");
        var adapter = new FilesystemAdapter(wrapper, client, "http://localhost:8080", new Dictionary<string, string>());

        var entries = await adapter.ListDirectoryAsync("/workspace");

        handler.LastRequestUri.Should().NotBeNull();
        handler.LastRequestUri!.Query.Should().Contain("path=%2Fworkspace");
        handler.LastRequestUri!.Query.Should().NotContain("depth=");
        entries.Should().BeEmpty();
    }

    private sealed class CaptureJsonHandler(string payload) : HttpMessageHandler
    {
        public Uri? LastRequestUri { get; private set; }

        protected override Task<HttpResponseMessage> SendAsync(HttpRequestMessage request, CancellationToken cancellationToken)
        {
            LastRequestUri = request.RequestUri;
            var response = new HttpResponseMessage(HttpStatusCode.OK)
            {
                Content = new StringContent(payload, Encoding.UTF8, "application/json")
            };
            return Task.FromResult(response);
        }
    }
}
