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

namespace OpenSandbox.Models;

/// <summary>
/// A created PTY session. The shell starts on the first WebSocket attach.
/// </summary>
public sealed class PtySession
{
    /// <summary>
    /// Gets or sets the server-assigned identifier of the PTY session.
    /// </summary>
    [JsonPropertyName("session_id")]
    public required string SessionId { get; set; }
}

/// <summary>
/// Current status of a PTY session.
/// </summary>
public sealed class PtySessionStatus
{
    /// <summary>
    /// Gets or sets the identifier of the PTY session.
    /// </summary>
    [JsonPropertyName("session_id")]
    public required string SessionId { get; set; }

    /// <summary>
    /// Gets or sets whether the underlying shell process is alive.
    /// </summary>
    [JsonPropertyName("running")]
    public bool Running { get; set; }

    /// <summary>
    /// Gets or sets the byte offset of buffered output; pass it as <c>since</c>
    /// on reconnect to replay scrollback from that point.
    /// </summary>
    [JsonPropertyName("output_offset")]
    public long OutputOffset { get; set; }
}
