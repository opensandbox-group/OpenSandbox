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

using System.Globalization;
using System.Text.Json.Serialization;

namespace OpenSandbox.Models;

/// <summary>Recovery scope for one execd lifetime.</summary>
public sealed class ExecutionInstance
{
    [JsonPropertyName("instance_id")]
    public required string InstanceId { get; init; }
    [JsonPropertyName("issued_at")]
    public long IssuedAt { get; init; }
    [JsonPropertyName("retention_seconds")]
    public long RetentionSeconds { get; init; }
    [JsonPropertyName("capacity")]
    public int Capacity { get; init; }

    /// <summary>Generate once and persist before creation. Never refresh during recovery.</summary>
    public string NewOperationId() => $"{InstanceId}.{IssuedAt.ToString(CultureInfo.InvariantCulture)}.{Guid.NewGuid():N}";
}

/// <summary>Creation acknowledgement only; Created does not mean script success.</summary>
public sealed class ExecutionOperation
{
    [JsonPropertyName("id")]
    public required string Id { get; init; }
    [JsonPropertyName("kind")]
    public required string Kind { get; init; }
    [JsonPropertyName("state")]
    public required string State { get; init; }
    [JsonPropertyName("expires_at")]
    public DateTimeOffset ExpiresAt { get; init; }
}
