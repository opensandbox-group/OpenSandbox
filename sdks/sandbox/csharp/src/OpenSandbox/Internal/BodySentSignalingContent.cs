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

namespace OpenSandbox.Internal;

/// <summary>
/// Wraps request content to signal when a request-body serialization starts and when the body
/// has been fully written to the wire. <see cref="HttpClientWrapper.SendTransferAsync"/> uses
/// the signals to keep the response-header deadline of a file upload off while the body is being
/// sent: a slow upload is not aborted by the time its own body takes, the deadline is re-armed
/// per hop of a redirect, and a duplex transport that already delivered the response leaves the
/// deadline disarmed.
/// </summary>
internal sealed class BodySentSignalingContent : HttpContent
{
    private readonly HttpContent _inner;
    private readonly Action _onBodySendStarted;
    private readonly Action _onBodySent;

    public BodySentSignalingContent(HttpContent inner, Action onBodySendStarted, Action onBodySent)
    {
        _inner = inner ?? throw new ArgumentNullException(nameof(inner));
        _onBodySendStarted = onBodySendStarted ?? throw new ArgumentNullException(nameof(onBodySendStarted));
        _onBodySent = onBodySent ?? throw new ArgumentNullException(nameof(onBodySent));

        foreach (var header in inner.Headers)
        {
            Headers.TryAddWithoutValidation(header.Key, header.Value);
        }
    }

    protected override Task SerializeToStreamAsync(Stream stream, TransportContext? context) =>
        // The transport gives no token on the pre-.NET 5 API surface (netstandard2.0/2.1, i.e.
        // .NET Framework); the token-aware override below exists only where the runtime has it.
        SerializeCoreAsync(stream, context, CancellationToken.None);

#if NET5_0_OR_GREATER
    /// <summary>
    /// Forwards the transport's token so a caller cancellation still interrupts a body read that
    /// blocks (for example a slow producer stream); modern runtimes call this overload.
    /// </summary>
    protected override Task SerializeToStreamAsync(
        Stream stream,
        TransportContext? context,
        CancellationToken cancellationToken)
        => SerializeCoreAsync(stream, context, cancellationToken);
#endif

    private async Task SerializeCoreAsync(Stream stream, TransportContext? context, CancellationToken cancellationToken)
    {
        // A redirect serializes the content once per hop: clear the deadline armed by the
        // previous hop before this hop's body is sent, and re-arm it once the body is on the wire.
        _onBodySendStarted();
#if NET5_0_OR_GREATER
        await _inner.CopyToAsync(stream, context, cancellationToken).ConfigureAwait(false);
#else
        // HttpContent has no token-carrying copy overload before .NET 5, so the transport token
        // cannot be forwarded here. The file parts copy themselves with the caller token
        // (UploadFileContent), so cancellation of a blocked producer stays uniform across TFMs.
        await _inner.CopyToAsync(stream).ConfigureAwait(false);
#endif
        _onBodySent();
    }

    protected override bool TryComputeLength(out long length)
    {
        // HttpContent.TryComputeLength is protected and cannot be forwarded to another instance,
        // so go through the length the inner content reports in its headers; MultipartFormDataContent
        // and friends compute it when the request is prepared.
        var contentLength = _inner.Headers.ContentLength;
        length = contentLength ?? -1L;
        return contentLength.HasValue;
    }

    protected override Task<Stream> CreateContentReadStreamAsync() => _inner.ReadAsStreamAsync();

    protected override void Dispose(bool disposing)
    {
        if (disposing)
        {
            _inner.Dispose();
        }

        base.Dispose(disposing);
    }
}
