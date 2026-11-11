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

using System.Buffers;
using System.Net;

namespace OpenSandbox.Internal;

/// <summary>
/// Streams one upload file part with caller cancellation. The copy is a manual loop that passes a
/// token to both the source read and the destination write, so a read that blocks on the producer
/// stream is interruptible on every target framework, including netstandard2.0/2.1 where the
/// <see cref="HttpContent"/> copy APIs carry no token. The file part stays streamed - nothing is
/// buffered in memory - and a seekable source keeps the length delimited like
/// <see cref="StreamContent"/> does.
/// <para>
/// The transport serializes a request body once per hop of a 307/308 redirect. A seekable source
/// is rewound to its initial position before every serialization, so the redirected hop resends
/// the whole file part instead of trailing off at EOF. A non-seekable source cannot be replayed
/// without buffering it, so a second serialization fails with an
/// <see cref="InvalidOperationException"/> instead of silently sending an empty part.
/// </para>
/// </summary>
internal sealed class UploadFileContent : HttpContent
{
    private const int CopyBufferSize = 81920;

    private readonly Stream _source;
    private readonly CancellationToken _callerToken;
    private readonly bool _canSeek;
    private readonly long _initialPosition;
    private int _serializations;

    public UploadFileContent(Stream source, CancellationToken callerToken)
    {
        _source = source ?? throw new ArgumentNullException(nameof(source));
        _callerToken = callerToken;
        _canSeek = _source.CanSeek;
        _initialPosition = _canSeek ? _source.Position : 0L;

        // Mirror StreamContent: a seekable source lets the multipart content compute a total
        // Content-Length, so the upload stays length-delimited instead of chunked.
        if (_canSeek)
        {
            Headers.ContentLength = _source.Length - _initialPosition;
        }
    }

    protected override Task SerializeToStreamAsync(Stream stream, TransportContext? context) =>
        // The pre-.NET 5 API surface has no token here; the caller token still reaches the copy
        // below, and modern runtimes additionally hand us the transport token.
        CopySourceToAsync(stream, CancellationToken.None);

#if NET5_0_OR_GREATER
    protected override Task SerializeToStreamAsync(
        Stream stream,
        TransportContext? context,
        CancellationToken cancellationToken) =>
        CopySourceToAsync(stream, cancellationToken);
#endif

    /// <summary>
    /// Internal for tests: the manual copy loop every target framework runs. Both the read from
    /// the producer stream and the write to the transport are bound to the caller token (linked
    /// with the transport token where one exists), so cancellation reaches a blocked read
    /// instead of only the socket.
    /// </summary>
    internal async Task CopySourceToAsync(Stream destination, CancellationToken transportToken)
    {
        ReplayOrThrow();

        using var linked = CancellationTokenSource.CreateLinkedTokenSource(_callerToken, transportToken);
        var buffer = ArrayPool<byte>.Shared.Rent(CopyBufferSize);
        try
        {
            int read;
#if NETSTANDARD2_1_OR_GREATER || NET5_0_OR_GREATER
            // The memory-based overloads are the ones a stream can implement cancellably; the
            // array-based default keeps an abandoned read running in the background.
            while ((read = await _source.ReadAsync(buffer.AsMemory(0, buffer.Length), linked.Token).ConfigureAwait(false)) > 0)
            {
                await destination.WriteAsync(buffer.AsMemory(0, read), linked.Token).ConfigureAwait(false);
            }
#else
            // netstandard2.0 has no memory-based overloads; the token still reaches streams that
            // implement the array-based asynchronous methods (the only cancellable API there).
            while ((read = await _source.ReadAsync(buffer, 0, buffer.Length, linked.Token).ConfigureAwait(false)) > 0)
            {
                await destination.WriteAsync(buffer, 0, read, linked.Token).ConfigureAwait(false);
            }
#endif
        }
        finally
        {
            ArrayPool<byte>.Shared.Return(buffer);
        }
    }

    protected override bool TryComputeLength(out long length)
    {
        if (_canSeek)
        {
            length = _source.Length - _initialPosition;
            return true;
        }

        length = -1L;
        return false;
    }

    /// <summary>
    /// Prepares the source for one serialization. A seekable source is rewound to the position it
    /// had when the content was created, so every hop of a redirect resends the complete part with
    /// the length announced up front. A non-seekable source can only be streamed once; refusing
    /// the replay is better than resending an empty part under the original Content-Length.
    /// </summary>
    private void ReplayOrThrow()
    {
        var serialization = Interlocked.Increment(ref _serializations);
        if (_canSeek)
        {
            _source.Position = _initialPosition;
            return;
        }

        if (serialization > 1)
        {
            throw new InvalidOperationException(
                "The upload source stream cannot be replayed, but the transport tried to serialize "
                + "the request body again (a 307/308 redirect resends the body once per hop). Pass "
                + "a seekable stream so it can be rewound, or disable redirects on the transfer client.");
        }
    }

    protected override void Dispose(bool disposing)
    {
        if (disposing)
        {
            // StreamContent owns and disposes its source stream; keep that contract.
            _source.Dispose();
        }

        base.Dispose(disposing);
    }
}
