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
using OpenSandbox.Config;
using OpenSandbox.Internal;
using OpenSandbox.Models;
using Xunit;

namespace OpenSandbox.Tests;

public class StreamReadTimeoutHandlerTests
{
    private static readonly TimeSpan ReadTimeout = TimeSpan.FromMilliseconds(300);

    [Fact]
    public async Task StalledStream_FailsWithTimeout()
    {
        // One event, then the connection goes silent without closing.
        var stream = new ScriptedStream(
            (TimeSpan.Zero, "data: {\"type\":\"stdout\",\"text\":\"line1\"}\n\n"));
        using var client = CreateClient(_ => Task.FromResult(StreamResponse(stream)));

        var events = new List<ServerStreamEvent>();
        var act = async () =>
        {
            using var response = await client.SendAsync(
                new HttpRequestMessage(HttpMethod.Post, "http://execd.local/command"),
                HttpCompletionOption.ResponseHeadersRead);
            await foreach (var ev in SseParser.ParseJsonEventStreamAsync<ServerStreamEvent>(response))
            {
                events.Add(ev);
            }
        };

        var ex = await act.Should().ThrowAsync<TaskCanceledException>().WaitAsync(TimeSpan.FromSeconds(10));
        ex.Which.InnerException.Should().BeOfType<TimeoutException>();
        events.Should().ContainSingle().Which.Text.Should().Be("line1");
    }

    [Fact]
    public async Task KeepalivePings_KeepTheStreamOpenPastTheTimeout()
    {
        // Every gap stays under the read timeout while the stream as a whole runs past it.
        var stream = new ScriptedStream(
            (TimeSpan.Zero, ": ping\n\n"),
            (TimeSpan.FromMilliseconds(400), ": ping\n\n"),
            (TimeSpan.FromMilliseconds(400), ": ping\n\n"),
            (TimeSpan.FromMilliseconds(400), "data: {\"type\":\"execution_complete\",\"execution_time\":1}\n\n"))
        {
            CloseAfterScript = true
        };
        using var client = CreateClient(_ => Task.FromResult(StreamResponse(stream)), TimeSpan.FromSeconds(1));

        using var response = await client.SendAsync(
            new HttpRequestMessage(HttpMethod.Post, "http://execd.local/command"),
            HttpCompletionOption.ResponseHeadersRead);
        var events = new List<ServerStreamEvent>();
        await foreach (var ev in SseParser.ParseJsonEventStreamAsync<ServerStreamEvent>(response))
        {
            events.Add(ev);
        }

        events.Should().ContainSingle().Which.Type.Should().Be("execution_complete");
    }

    [Fact]
    public async Task StalledRead_ThatFailsOnCancellation_ReportsTimeout()
    {
        // A socket stream fails its pending read as soon as the token is
        // canceled, which can happen before the handler's own deadline
        // callback has run.
        using var client = CreateClient(_ => Task.FromResult(StreamResponse(new FailsOnCancelStream())));
        using var response = await client.SendAsync(
            new HttpRequestMessage(HttpMethod.Post, "http://execd.local/command"),
            HttpCompletionOption.ResponseHeadersRead);
        var body = await response.Content.ReadAsStreamAsync();

        var act = () => body.ReadAsync(new byte[16], 0, 16);

        var ex = await act.Should().ThrowAsync<TaskCanceledException>().WaitAsync(TimeSpan.FromSeconds(10));
        ex.Which.InnerException.Should().BeOfType<TimeoutException>();
    }

    [Fact]
    public async Task MissingResponseHeaders_FailWithTimeout()
    {
        using var client = CreateClient(async ct =>
        {
            await Task.Delay(Timeout.Infinite, ct);
            throw new InvalidOperationException("unreachable");
        });

        var act = () => client.SendAsync(
            new HttpRequestMessage(HttpMethod.Post, "http://execd.local/command"),
            HttpCompletionOption.ResponseHeadersRead);

        var ex = await act.Should().ThrowAsync<TaskCanceledException>().WaitAsync(TimeSpan.FromSeconds(10));
        ex.Which.InnerException.Should().BeOfType<TimeoutException>();
    }

    [Fact]
    public async Task CallerCancellation_IsNotReportedAsTimeout()
    {
        var stream = new ScriptedStream((TimeSpan.Zero, ": ping\n\n")) { HonorCancellation = true };
        using var client = CreateClient(_ => Task.FromResult(StreamResponse(stream)), TimeSpan.FromSeconds(30));
        using var cts = new CancellationTokenSource();

        using var response = await client.SendAsync(
            new HttpRequestMessage(HttpMethod.Post, "http://execd.local/command"),
            HttpCompletionOption.ResponseHeadersRead);
        var body = await response.Content.ReadAsStreamAsync();
        var buffer = new byte[64];
        (await body.ReadAsync(buffer, 0, buffer.Length, cts.Token)).Should().BeGreaterThan(0);

        var read = body.ReadAsync(buffer, 0, buffer.Length, cts.Token);
        cts.Cancel();
        var act = () => read;

        var ex = await act.Should().ThrowAsync<OperationCanceledException>().WaitAsync(TimeSpan.FromSeconds(10));
        (ex.Which.InnerException is TimeoutException).Should().BeFalse();
    }

    [Fact]
    public async Task SynchronousSend_BoundsHeadersAndBodyReads()
    {
        using var silent = CreateClient(async ct =>
        {
            await Task.Delay(Timeout.Infinite, ct);
            throw new InvalidOperationException("unreachable");
        });
        var sendSilent = () => silent.Send(
            new HttpRequestMessage(HttpMethod.Post, "http://execd.local/command"),
            HttpCompletionOption.ResponseHeadersRead);
        await Task.Run(() => sendSilent.Should().Throw<TaskCanceledException>().WithInnerException<TimeoutException>())
            .WaitAsync(TimeSpan.FromSeconds(10));

        var stream = new ScriptedStream((TimeSpan.Zero, ": ping\n\n"));
        using var stalled = CreateClient(_ => Task.FromResult(StreamResponse(stream)));
        using var response = await Task.Run(() => stalled.Send(
            new HttpRequestMessage(HttpMethod.Post, "http://execd.local/command"),
            HttpCompletionOption.ResponseHeadersRead)).WaitAsync(TimeSpan.FromSeconds(10));
        var body = response.Content.ReadAsStream();
        var buffer = new byte[64];
        body.Read(buffer, 0, buffer.Length).Should().BeGreaterThan(0);

        var readStalled = () => body.Read(buffer, 0, buffer.Length);
        await Task.Run(() => readStalled.Should().Throw<TaskCanceledException>().WithInnerException<TimeoutException>())
            .WaitAsync(TimeSpan.FromSeconds(10));
    }

    [Theory]
    [InlineData(5, 30)]
    [InlineData(120, 120)]
    public void SseReadTimeout_IsTheRequestTimeoutWithAFloor(int requestTimeoutSeconds, int expectedSeconds)
    {
        var config = new ConnectionConfig(new ConnectionConfigOptions { RequestTimeoutSeconds = requestTimeoutSeconds });

        config.SseReadTimeout.Should().Be(TimeSpan.FromSeconds(expectedSeconds));
    }

    private static HttpClient CreateClient(
        Func<CancellationToken, Task<HttpResponseMessage>> respond, TimeSpan? readTimeout = null) =>
        new(new StreamReadTimeoutHandler(new StubHandler(respond), readTimeout ?? ReadTimeout))
        {
            Timeout = Timeout.InfiniteTimeSpan
        };

    private static HttpResponseMessage StreamResponse(Stream stream)
    {
        var content = new StreamContent(stream);
        content.Headers.ContentType = new System.Net.Http.Headers.MediaTypeHeaderValue("text/event-stream");
        return new HttpResponseMessage(HttpStatusCode.OK) { Content = content };
    }

    private sealed class StubHandler : HttpMessageHandler
    {
        private readonly Func<CancellationToken, Task<HttpResponseMessage>> _respond;

        public StubHandler(Func<CancellationToken, Task<HttpResponseMessage>> respond) => _respond = respond;

        protected override Task<HttpResponseMessage> SendAsync(HttpRequestMessage request, CancellationToken cancellationToken) =>
            _respond(cancellationToken);

        protected override HttpResponseMessage Send(HttpRequestMessage request, CancellationToken cancellationToken) =>
            _respond(cancellationToken).GetAwaiter().GetResult();
    }

    /// <summary>
    /// Never produces data; a pending read fails with an IOException, inline,
    /// when its cancellation token fires.
    /// </summary>
    private sealed class FailsOnCancelStream : Stream
    {
        public override Task<int> ReadAsync(byte[] buffer, int offset, int count, CancellationToken cancellationToken)
        {
            var read = new TaskCompletionSource<int>();
            cancellationToken.Register(() => read.TrySetException(new IOException("socket aborted")));
            return read.Task;
        }

        public override int Read(byte[] buffer, int offset, int count) =>
            ReadAsync(buffer, offset, count, CancellationToken.None).GetAwaiter().GetResult();

        public override bool CanRead => true;
        public override bool CanSeek => false;
        public override bool CanWrite => false;
        public override long Length => throw new NotSupportedException();
        public override long Position
        {
            get => throw new NotSupportedException();
            set => throw new NotSupportedException();
        }
        public override void Flush() { }
        public override long Seek(long offset, SeekOrigin origin) => throw new NotSupportedException();
        public override void SetLength(long value) => throw new NotSupportedException();
        public override void Write(byte[] buffer, int offset, int count) => throw new NotSupportedException();
    }

    /// <summary>
    /// Returns each chunk after its delay, then ends the stream or blocks until it is disposed.
    /// Unless <see cref="HonorCancellation"/> is set, a blocked read ends only through disposal,
    /// like a read issued by a StreamReader.ReadLineAsync overload that takes no token.
    /// </summary>
    private sealed class ScriptedStream : Stream
    {
        private readonly Queue<(TimeSpan Delay, byte[] Bytes)> _chunks = new();
        private readonly TaskCompletionSource<int> _disposed = new(TaskCreationOptions.RunContinuationsAsynchronously);

        public ScriptedStream(params (TimeSpan Delay, string Text)[] chunks)
        {
            foreach (var (delay, text) in chunks)
            {
                _chunks.Enqueue((delay, Encoding.UTF8.GetBytes(text)));
            }
        }

        public bool CloseAfterScript { get; init; }

        public bool HonorCancellation { get; init; }

        public override async Task<int> ReadAsync(byte[] buffer, int offset, int count, CancellationToken cancellationToken)
        {
            if (_chunks.Count == 0)
            {
                if (CloseAfterScript)
                {
                    return 0;
                }
                if (HonorCancellation)
                {
                    await Task.WhenAny(_disposed.Task, Task.Delay(Timeout.Infinite, cancellationToken)).ConfigureAwait(false);
                    cancellationToken.ThrowIfCancellationRequested();
                }
                return await _disposed.Task.ConfigureAwait(false);
            }
            var (delay, bytes) = _chunks.Dequeue();
            if (delay > TimeSpan.Zero)
            {
                await Task.WhenAny(Task.Delay(delay), _disposed.Task).ConfigureAwait(false);
            }
            if (_disposed.Task.IsCompleted)
            {
                return await _disposed.Task.ConfigureAwait(false);
            }
            Array.Copy(bytes, 0, buffer, offset, bytes.Length);
            return bytes.Length;
        }

        public override int Read(byte[] buffer, int offset, int count) =>
            ReadAsync(buffer, offset, count, CancellationToken.None).GetAwaiter().GetResult();

        protected override void Dispose(bool disposing)
        {
            _disposed.TrySetException(new ObjectDisposedException(nameof(ScriptedStream)));
            base.Dispose(disposing);
        }

        public override bool CanRead => true;
        public override bool CanSeek => false;
        public override bool CanWrite => false;
        public override long Length => throw new NotSupportedException();
        public override long Position
        {
            get => throw new NotSupportedException();
            set => throw new NotSupportedException();
        }
        public override void Flush() { }
        public override long Seek(long offset, SeekOrigin origin) => throw new NotSupportedException();
        public override void SetLength(long value) => throw new NotSupportedException();
        public override void Write(byte[] buffer, int offset, int count) => throw new NotSupportedException();
    }
}
