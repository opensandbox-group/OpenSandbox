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

using System.Text.Json.Serialization;
using OpenSandbox.Internal;
using OpenSandbox.Models;
using OpenSandbox.Services;

namespace OpenSandbox.Adapters;

/// <summary>
/// Adapter for the execd interactive PTY session service.
/// </summary>
internal sealed class PtyAdapter : IExecdPty
{
    private readonly HttpClientWrapper _client;

    public PtyAdapter(HttpClientWrapper client)
    {
        _client = client ?? throw new ArgumentNullException(nameof(client));
    }

    public Task<PtySession> CreateSessionAsync(
        string? cwd = null,
        string? command = null,
        CancellationToken cancellationToken = default)
    {
        var body = new PtyCreateRequest { Cwd = cwd, Command = command };
        return _client.PostAsync<PtySession>("/pty", body, cancellationToken);
    }

    public Task<PtySessionStatus> GetSessionAsync(string sessionId, CancellationToken cancellationToken = default)
    {
        return _client.GetAsync<PtySessionStatus>(
            $"/pty/{Uri.EscapeDataString(sessionId)}",
            cancellationToken: cancellationToken);
    }

    public Task DeleteSessionAsync(string sessionId, CancellationToken cancellationToken = default)
    {
        return _client.DeleteAsync(
            $"/pty/{Uri.EscapeDataString(sessionId)}",
            cancellationToken: cancellationToken);
    }

    private sealed class PtyCreateRequest
    {
        [JsonPropertyName("cwd")]
        public string? Cwd { get; set; }

        [JsonPropertyName("command")]
        public string? Command { get; set; }
    }
}
