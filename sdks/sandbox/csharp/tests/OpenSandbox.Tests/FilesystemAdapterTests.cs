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
using FluentAssertions;
using OpenSandbox.Adapters;
using OpenSandbox.Internal;
using Xunit;

namespace OpenSandbox.Tests;

public class FilesystemAdapterTests
{
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
