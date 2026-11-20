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

namespace OpenSandbox.Internal;

/// <summary>
/// Bounds how long a streaming response may stay silent: the wait for response headers and
/// every read of the body each get <c>readTimeout</c>, so a stream that keeps sending (execd
/// pings its SSE streams every few seconds) runs as long as it needs while a stalled one
/// fails instead of blocking forever. The whole call is not bounded.
/// </summary>
internal sealed class StreamReadTimeoutHandler : DelegatingHandler
{
    private readonly TimeSpan _readTimeout;

    internal StreamReadTimeoutHandler(HttpMessageHandler innerHandler, TimeSpan readTimeout) : base(innerHandler)
    {
        _readTimeout = readTimeout;
    }

    protected override async Task<HttpResponseMessage> SendAsync(
        HttpRequestMessage request, CancellationToken cancellationToken)
    {
        HttpResponseMessage response;
        using (var headerDeadline = CancellationTokenSource.CreateLinkedTokenSource(cancellationToken))
        {
            headerDeadline.CancelAfter(_readTimeout);
            try
            {
                response = await base.SendAsync(request, headerDeadline.Token).ConfigureAwait(false);
            }
            catch (OperationCanceledException ex) when (!cancellationToken.IsCancellationRequested && headerDeadline.IsCancellationRequested)
            {
                throw ReadTimedOut(_readTimeout, "response headers", ex);
            }
        }

        return WrapBody(response, await response.Content.ReadAsStreamAsync().ConfigureAwait(false));
    }

#if NET5_0_OR_GREATER
    protected override HttpResponseMessage Send(HttpRequestMessage request, CancellationToken cancellationToken)
    {
        HttpResponseMessage response;
        using (var headerDeadline = CancellationTokenSource.CreateLinkedTokenSource(cancellationToken))
        {
            headerDeadline.CancelAfter(_readTimeout);
            try
            {
                response = base.Send(request, headerDeadline.Token);
            }
            catch (OperationCanceledException ex) when (!cancellationToken.IsCancellationRequested && headerDeadline.IsCancellationRequested)
            {
                throw ReadTimedOut(_readTimeout, "response headers", ex);
            }
        }

        return WrapBody(response, response.Content.ReadAsStream(cancellationToken));
    }
#endif

    private HttpResponseMessage WrapBody(HttpResponseMessage response, Stream body)
    {
        var content = new StreamContent(new ReadTimeoutStream(response.Content, body, _readTimeout));
        foreach (var header in response.Content.Headers)
        {
            content.Headers.TryAddWithoutValidation(header.Key, header.Value);
        }
        response.Content = content;
        return response;
    }

    private static TaskCanceledException ReadTimedOut(TimeSpan timeout, string what, Exception inner)
    {
        // Same shape as HttpClient.Timeout on modern runtimes: a TaskCanceledException with a
        // TimeoutException inside.
        var message = FormattableString.Invariant($"The stream received no {what} within {timeout}.");
        return new TaskCanceledException(message, new TimeoutException(message, inner));
    }

    /// <summary>
    /// Fails a read that gets no bytes within the timeout. The timer disposes the inner stream,
    /// which ends the pending read on every target framework, including those where
    /// StreamReader.ReadLineAsync passes no cancellation token down.
    /// </summary>
    private sealed class ReadTimeoutStream : Stream
    {
        private readonly HttpContent _owner;
        private readonly Stream _inner;
        private readonly TimeSpan _timeout;
        private volatile bool _timedOut;

        internal ReadTimeoutStream(HttpContent owner, Stream inner, TimeSpan timeout)
        {
            _owner = owner;
            _inner = inner;
            _timeout = timeout;
        }

        public override async Task<int> ReadAsync(byte[] buffer, int offset, int count, CancellationToken cancellationToken)
        {
            using var deadline = new CancellationTokenSource(_timeout);
            using var abort = deadline.Token.Register(() =>
            {
                _timedOut = true;
                _inner.Dispose();
            });
            using var linked = CancellationTokenSource.CreateLinkedTokenSource(cancellationToken, deadline.Token);
            try
            {
                return await _inner.ReadAsync(buffer, offset, count, linked.Token).ConfigureAwait(false);
            }
            // The deadline's own state is checked because the inner read can fail on the linked
            // token before the callback above sets _timedOut. _timedOut covers a deadline that
            // fired just after an earlier read returned: the inner stream is disposed by then,
            // and this read fails because of it.
            catch (Exception ex) when ((deadline.IsCancellationRequested || _timedOut) && !cancellationToken.IsCancellationRequested)
            {
                throw ReadTimedOut(_timeout, "data", ex);
            }
        }

        public override int Read(byte[] buffer, int offset, int count) =>
            ReadAsync(buffer, offset, count, CancellationToken.None).GetAwaiter().GetResult();

        protected override void Dispose(bool disposing)
        {
            if (disposing)
            {
                _inner.Dispose();
                _owner.Dispose();
            }
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
