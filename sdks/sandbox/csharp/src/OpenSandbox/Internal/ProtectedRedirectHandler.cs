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

using System.Net.Http.Headers;

namespace OpenSandbox.Internal;

/// <summary>
/// Follows redirects without forwarding OpenSandbox credentials to another origin.
/// Runs below HttpClient so its default headers are merged only on the first send.
/// </summary>
internal sealed class ProtectedRedirectHandler : DelegatingHandler
{
    private const int MaxRedirects = 50; // HttpClientHandler's default.

    internal ProtectedRedirectHandler(HttpMessageHandler innerHandler) : base(innerHandler)
    {
    }

    protected override Task<HttpResponseMessage> SendAsync(HttpRequestMessage request, CancellationToken cancellationToken) =>
        SendCoreAsync(request, useAsync: true, cancellationToken);

#if NET5_0_OR_GREATER
    protected override HttpResponseMessage Send(HttpRequestMessage request, CancellationToken cancellationToken) =>
        SendCoreAsync(request, useAsync: false, cancellationToken).GetAwaiter().GetResult();
#endif

    private async Task<HttpResponseMessage> SendCoreAsync(
        HttpRequestMessage request, bool useAsync, CancellationToken cancellationToken)
    {
        var response = await SendNextAsync(request, useAsync, cancellationToken).ConfigureAwait(false);
        for (var redirects = 0; redirects < MaxRedirects; redirects++)
        {
            var location = GetRedirectUri(request.RequestUri!, response);
            if (location == null)
            {
                break;
            }

            var status = (int)response.StatusCode;
            response.Dispose();

            if (!SameOrigin(request.RequestUri!, location))
            {
                StripProtectedHeaders(request.Headers);
                if (request.Content != null)
                {
                    StripProtectedHeaders(request.Content.Headers);
                }
            }

            // Match HttpClientHandler: Authorization is cleared on every redirect.
            // Explicit Cookie headers and the transport's CookieContainer retain
            // their existing .NET behavior.
            request.Headers.Authorization = null;
            request.RequestUri = location;
            if ((status is 300 or 301 or 302 && request.Method == HttpMethod.Post) ||
                (status == 303 && request.Method != HttpMethod.Get && request.Method != HttpMethod.Head))
            {
                request.Method = HttpMethod.Get;
                request.Content = null;
                request.Headers.TransferEncodingChunked = false;
            }

            // Reuse content as the native redirect handler does. StreamContent
            // can rewind seekable streams; non-replayable uploads are never buffered.
            response = await SendNextAsync(request, useAsync, cancellationToken).ConfigureAwait(false);
        }

        return response;
    }

    private Task<HttpResponseMessage> SendNextAsync(HttpRequestMessage request, bool useAsync, CancellationToken cancellationToken)
    {
#if NET5_0_OR_GREATER
        // Sync callers use synchronous transport; the redirect loop's awaits
        // complete inline rather than blocking on asynchronous network I/O.
        if (!useAsync)
        {
            return Task.FromResult(base.Send(request, cancellationToken));
        }
#endif
        return base.SendAsync(request, cancellationToken);
    }

    private static bool IsProtectedHeader(string name) =>
        name.StartsWith("OPEN-SANDBOX-", StringComparison.OrdinalIgnoreCase) ||
        name.StartsWith("OPENSANDBOX-", StringComparison.OrdinalIgnoreCase) ||
        name.Equals("X-EXECD-ACCESS-TOKEN", StringComparison.OrdinalIgnoreCase) ||
        name.Equals("X-API-Key", StringComparison.OrdinalIgnoreCase);

    private static void StripProtectedHeaders(HttpHeaders headers)
    {
        foreach (var name in headers.Select(header => header.Key).Where(IsProtectedHeader).ToArray())
        {
            headers.Remove(name);
        }
    }

    internal static bool SameOrigin(Uri source, Uri target) =>
        string.Equals(source.Scheme, target.Scheme, StringComparison.OrdinalIgnoreCase) &&
        string.Equals(source.IdnHost, target.IdnHost, StringComparison.OrdinalIgnoreCase) &&
        source.Port == target.Port;

    private static Uri? GetRedirectUri(Uri source, HttpResponseMessage response)
    {
        if ((int)response.StatusCode is not (300 or 301 or 302 or 303 or 307 or 308) ||
            response.Headers.Location == null)
        {
            return null;
        }

        var target = response.Headers.Location;
        if (!target.IsAbsoluteUri)
        {
            target = new Uri(source, target);
        }

        // Modern .NET never follows a secure-to-insecure redirect.
        if (source.Scheme == Uri.UriSchemeHttps && target.Scheme != Uri.UriSchemeHttps)
        {
            return null;
        }

        if (source.Fragment.Length != 0 && target.Fragment.Length == 0)
        {
            target = new UriBuilder(target) { Fragment = source.Fragment }.Uri;
        }

        return target;
    }
}
