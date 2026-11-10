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
using OpenSandbox.Core;
using OpenSandbox.Internal;
using OpenSandbox.Models;
using OpenSandbox.Services;
using Xunit;

namespace OpenSandbox.Tests;

public class FilesystemAdapterTests
{
    [Theory]
    [InlineData(2, null)]
    [InlineData(null, 2)]
    public async Task LineReads_ShouldReportUnknownTotalSize(int? offset, int? limit)
    {
        var handler = new DownloadHandler();
        handler.Enqueue(DownloadResponse(HttpStatusCode.OK, "hello"));
        handler.Enqueue(DownloadResponse(HttpStatusCode.OK, "hello"));
        using var client = new HttpClient(handler);
        var adapter = CreateAdapter(client);
        var options = new ReadBytesOptions { Offset = offset, Limit = limit };

        var bytes = await adapter.ReadBytesDetailedAsync("/data.txt", options);
        bytes.IsPartial.Should().BeFalse();
        bytes.ContentLength.Should().Be(5);
        bytes.TotalSize.Should().Be(-1);

        var stream = await adapter.ReadBytesStreamDetailedAsync("/data.txt", options);
        await using var body = stream.Body;
        stream.IsPartial.Should().BeFalse();
        stream.ContentLength.Should().Be(5);
        stream.TotalSize.Should().Be(-1);
    }

    [Fact]
    public async Task WithIdentity_ShouldScopeJsonAndBinaryRequestsWithoutMutatingOriginal()
    {
        var handler = new IdentityCaptureHandler();
        using var client = new HttpClient(handler);
        const string baseUrl = "http://localhost:8080/sandboxes/example/port/44772";
        var headers = new Dictionary<string, string> { ["X-EXECD-ACCESS-TOKEN"] = "secret" };
        var wrapper = new HttpClientWrapper(client, baseUrl, headers);
        var original = new FilesystemAdapter(wrapper, baseUrl, headers);
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
    [InlineData("http://localhost:8080/v1/filesystem/")]
    [InlineData("http://localhost:8080/proxy/v1/filesystem/tenant/port/44772")]
    [InlineData("http://localhost:8080/proxy/V1/FILESYSTEM/tenant/port/44772/")]
    public async Task WithIdentity_ShouldPreserveProxyRootWhenRescoped(string baseUrl)
    {
        var handler = new IdentityCaptureHandler();
        using var client = new HttpClient(handler);
        var wrapper = new HttpClientWrapper(client, baseUrl);
        var original = new FilesystemAdapter(wrapper, baseUrl, new Dictionary<string, string>());
        OpenSandbox.Services.IIdentitySandboxFiles capability = original;
        var first = capability.WithIdentity(1, 2);
        var second = ((OpenSandbox.Services.IIdentitySandboxFiles)first).WithIdentity(3, 4);

        await first.GetFileInfoAsync(new[] { "/first" });
        await second.GetFileInfoAsync(new[] { "/second" });
        await original.GetFileInfoAsync(new[] { "/original" });

        var root = new Uri(baseUrl.TrimEnd('/')).AbsolutePath.TrimEnd('/');
        handler.Paths.Should().Equal(
            root + "/v1/filesystem/1/2/files/info",
            root + "/v1/filesystem/3/4/files/info",
            root + "/files/info");
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
        var original = new FilesystemAdapter(wrapper, wrapper.BaseUrl, new Dictionary<string, string>());
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
    public async Task ReadFileAsync_ShouldCancelAfterHeaders()
    {
        using var content = new StallingContent();
        var handler = new DownloadHandler();
        handler.Enqueue(new HttpResponseMessage(HttpStatusCode.OK) { Content = content });
        using var client = new HttpClient(handler);
        using var cancellation = new CancellationTokenSource();
        var adapter = CreateAdapter(client);

        var read = adapter.ReadFileAsync("/data.txt", cancellationToken: cancellation.Token);
        await content.Started.Task.WaitAsync(TimeSpan.FromSeconds(5));
        cancellation.Cancel();

        var action = async () => await read.WaitAsync(TimeSpan.FromSeconds(5));
        await action.Should().ThrowAsync<OperationCanceledException>();
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
        var adapter = new FilesystemAdapter(wrapper, "http://localhost:8080", new Dictionary<string, string>());

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
        var adapter = new FilesystemAdapter(wrapper, "http://localhost:8080", new Dictionary<string, string>());

        var entries = await adapter.ListDirectoryAsync("/workspace");

        handler.LastRequestUri.Should().NotBeNull();
        handler.LastRequestUri!.Query.Should().Contain("path=%2Fworkspace");
        handler.LastRequestUri!.Query.Should().NotContain("depth=");
        entries.Should().BeEmpty();
    }

    [Fact]
    public async Task ReadBytesDetailedAsync_ShouldExposePartialMetadata()
    {
        var handler = new DownloadHandler();
        handler.Enqueue(DownloadResponse(
            HttpStatusCode.PartialContent,
            "hello",
            contentRange: "bytes 0-4/10",
            contentType: "application/octet-stream",
            contentDisposition: "attachment; filename=\"data.bin\""));
        using var client = new HttpClient(handler);
        var adapter = CreateAdapter(client);

        var response = await adapter.ReadBytesDetailedAsync(
            "/data.bin",
            new ReadBytesOptions { Range = "bytes=0-4" });

        response.Body.Should().Equal(Encoding.UTF8.GetBytes("hello"));
        response.StatusCode.Should().Be(206);
        response.IsPartial.Should().BeTrue();
        response.ContentType.Should().Be("application/octet-stream");
        response.ContentDisposition.Should().Be("attachment; filename=\"data.bin\"");
        response.ContentLength.Should().Be(5);
        response.TotalSize.Should().Be(10);
        response.ContentRange.Should().Be(new ByteRange(0, 4, 10, "bytes 0-4/10"));
        handler.LastRange.Should().Be("bytes=0-4");
    }

    [Fact]
    public async Task ReadBytesDetailedAsync_ShouldIdentifyIgnoredRange()
    {
        var handler = new DownloadHandler();
        handler.Enqueue(DownloadResponse(HttpStatusCode.OK, "whole file"));
        using var client = new HttpClient(handler);
        var adapter = CreateAdapter(client);

        var response = await adapter.ReadBytesDetailedAsync(
            "/data.bin",
            new ReadBytesOptions { Range = "bytes=5-" });

        response.IsPartial.Should().BeFalse();
        response.ContentRange.Should().BeNull();
        response.ContentLength.Should().Be(10);
        response.TotalSize.Should().Be(10);
    }

    [Theory]
    [InlineData(null, -1, -1)]
    [InlineData("garbage", -1, -1)]
    [InlineData("bytes 5-9/*", 5, 9)]
    [InlineData("bytes 0-4/4", -1, -1)]
    public async Task ReadBytesDetailedAsync_ShouldReportUnknownTotalSize(string? raw, long start, long end)
    {
        var handler = new DownloadHandler();
        handler.Enqueue(DownloadResponse(HttpStatusCode.PartialContent, "hello", raw));
        using var client = new HttpClient(handler);
        var adapter = CreateAdapter(client);

        var response = await adapter.ReadBytesDetailedAsync("/data.bin");

        response.IsPartial.Should().BeTrue();
        response.ContentRange.Should().Be(raw == null ? null : new ByteRange(start, end, -1, raw));
        response.TotalSize.Should().Be(-1);
    }

    [Fact]
    public async Task DetailedAndLegacyReads_ShouldReturnResponseBodies()
    {
        var handler = new DownloadHandler();
        handler.Enqueue(DownloadResponse(HttpStatusCode.PartialContent, "hello", "bytes 0-4/10"));
        handler.Enqueue(DownloadResponse(HttpStatusCode.OK, "hello"));
        handler.Enqueue(DownloadResponse(HttpStatusCode.OK, "hello"));
        using var client = new HttpClient(handler);
        var adapter = CreateAdapter(client);

        var detailed = await adapter.ReadBytesStreamDetailedAsync("/data.bin");
        detailed.IsPartial.Should().BeTrue();
        (await CollectAsync(detailed.Body)).Should().Equal(Encoding.UTF8.GetBytes("hello"));
        (await adapter.ReadBytesAsync("/data.bin")).Should().Equal(Encoding.UTF8.GetBytes("hello"));
        (await CollectAsync(adapter.ReadBytesStreamAsync("/data.bin"))).Should().Equal(Encoding.UTF8.GetBytes("hello"));
    }

    [Fact]
    public async Task DetailedStreams_ShouldDisposeUnconsumedAndPartialBodies()
    {
        var unconsumedContent = new TrackingContent("hello");
        var partialContent = new TrackingContent("hello");
        var handler = new DownloadHandler();
        handler.Enqueue(new HttpResponseMessage(HttpStatusCode.PartialContent) { Content = unconsumedContent });
        handler.Enqueue(new HttpResponseMessage(HttpStatusCode.PartialContent) { Content = partialContent });
        using var client = new HttpClient(handler);
        var adapter = CreateAdapter(client);

        var unconsumed = await adapter.ReadBytesStreamDetailedAsync("/data.bin");
        await unconsumed.Body.DisposeAsync();
        unconsumedContent.Disposed.Should().BeTrue();

        var partial = await adapter.ReadBytesStreamDetailedAsync("/data.bin");
        await foreach (var _ in partial.Body)
        {
            break;
        }
        partialContent.Disposed.Should().BeTrue();
    }

    [Fact]
    public async Task ReadBytesStreamDetailedAsync_ShouldUseStandardApiErrorMapping()
    {
        var errorContent = new TrackingContent(
            """{"error":{"code":"FILE_NOT_FOUND","message":"missing file"}}""");
        var errorResponse = new HttpResponseMessage(HttpStatusCode.NotFound)
        {
            Content = errorContent,
        };
        errorResponse.Headers.TryAddWithoutValidation(Constants.RequestIdHeader, "request-123");
        var handler = new DownloadHandler();
        handler.Enqueue(errorResponse);
        using var client = new HttpClient(handler);
        var adapter = CreateAdapter(client);

        var action = () => adapter.ReadBytesStreamDetailedAsync("/missing.bin");

        var exception = await action.Should().ThrowAsync<SandboxApiException>();
        exception.Which.StatusCode.Should().Be(404);
        exception.Which.RequestId.Should().Be("request-123");
        exception.Which.Error.Code.Should().Be("FILE_NOT_FOUND");
        exception.Which.Error.Message.Should().Be("missing file");
        errorContent.Disposed.Should().BeTrue();
    }

    private static ISandboxFiles CreateAdapter(HttpClient client)
    {
        var wrapper = new HttpClientWrapper(client, "http://localhost:8080");
        return new FilesystemAdapter(wrapper, "http://localhost:8080", new Dictionary<string, string>());
    }

    private static HttpResponseMessage DownloadResponse(
        HttpStatusCode statusCode,
        string body,
        string? contentRange = null,
        string? contentType = null,
        string? contentDisposition = null)
    {
        var content = new ByteArrayContent(Encoding.UTF8.GetBytes(body));
        if (contentRange != null)
        {
            content.Headers.TryAddWithoutValidation("Content-Range", contentRange);
        }
        if (contentType != null)
        {
            content.Headers.TryAddWithoutValidation("Content-Type", contentType);
        }
        if (contentDisposition != null)
        {
            content.Headers.TryAddWithoutValidation("Content-Disposition", contentDisposition);
        }
        return new HttpResponseMessage(statusCode) { Content = content };
    }

    private static async Task<byte[]> CollectAsync(IAsyncEnumerable<byte[]> chunks)
    {
        using var stream = new MemoryStream();
        await foreach (var chunk in chunks)
        {
            await stream.WriteAsync(chunk);
        }
        return stream.ToArray();
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

    private sealed class DownloadHandler : HttpMessageHandler
    {
        private readonly Queue<HttpResponseMessage> _responses = new();

        public string? LastRange { get; private set; }

        public void Enqueue(HttpResponseMessage response) => _responses.Enqueue(response);

        protected override Task<HttpResponseMessage> SendAsync(
            HttpRequestMessage request,
            CancellationToken cancellationToken)
        {
            LastRange = request.Headers.TryGetValues("Range", out var values)
                ? values.SingleOrDefault()
                : null;
            return Task.FromResult(_responses.Dequeue());
        }
    }

    private sealed class TrackingContent(string body) : ByteArrayContent(Encoding.UTF8.GetBytes(body))
    {
        public bool Disposed { get; private set; }

        protected override void Dispose(bool disposing)
        {
            Disposed = true;
            base.Dispose(disposing);
        }
    }

    private sealed class StallingContent : HttpContent
    {
        public TaskCompletionSource<bool> Started { get; } = new(TaskCreationOptions.RunContinuationsAsynchronously);
        private readonly TaskCompletionSource<bool> _release = new(TaskCreationOptions.RunContinuationsAsynchronously);

        protected override Task SerializeToStreamAsync(Stream stream, TransportContext? context) =>
            SerializeToStreamAsync(stream, context, CancellationToken.None);

        protected override async Task SerializeToStreamAsync(Stream stream, TransportContext? context, CancellationToken cancellationToken)
        {
            Started.TrySetResult(true);
            await _release.Task.WaitAsync(cancellationToken);
        }

        protected override bool TryComputeLength(out long length)
        {
            length = 0;
            return false;
        }

        protected override void Dispose(bool disposing)
        {
            _release.TrySetResult(true);
            base.Dispose(disposing);
        }
    }
}
