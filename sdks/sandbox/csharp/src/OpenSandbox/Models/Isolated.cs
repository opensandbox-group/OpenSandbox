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

public record IsolatedWorkspaceSpec(
    [property: JsonPropertyName("path")] string Path,
    [property: JsonPropertyName("mode")] string? Mode = null
);

public record EnvPassthroughSpec(
    [property: JsonPropertyName("mode")] string? Mode = "deny",
    [property: JsonPropertyName("keys")] List<string>? Keys = null
);

public record BindMount(
    [property: JsonPropertyName("source")] string Source,
    [property: JsonPropertyName("dest")] string? Dest = null,
    [property: JsonPropertyName("readonly")] bool? ReadOnly = null
);

public record CreateIsolatedSessionRequest(
    [property: JsonPropertyName("workspace")] IsolatedWorkspaceSpec Workspace,
    [property: JsonPropertyName("profile")] string? Profile = null,
    [property: JsonPropertyName("extra_writable")] List<string>? ExtraWritable = null,
    [property: JsonPropertyName("binds")] List<BindMount>? Binds = null,
    [property: JsonPropertyName("share_net")] bool? ShareNet = null,
    [property: JsonPropertyName("env_passthrough")] EnvPassthroughSpec? EnvPassthrough = null,
    [property: JsonPropertyName("uid")] int? Uid = null,
    [property: JsonPropertyName("gid")] int? Gid = null,
    [property: JsonPropertyName("uid_mode")] string? UidMode = null,
    [property: JsonPropertyName("idle_timeout_seconds")] int? IdleTimeoutSeconds = null
);

public record IsolatedSessionInfo(
    [property: JsonPropertyName("session_id")] string SessionId,
    [property: JsonPropertyName("created_at")] DateTimeOffset? CreatedAt = null
);

public record IsolatedSessionState(
    [property: JsonPropertyName("status")] string Status,
    [property: JsonPropertyName("created_at")] DateTimeOffset? CreatedAt = null,
    [property: JsonPropertyName("last_run_at")] DateTimeOffset? LastRunAt = null,
    [property: JsonPropertyName("idle_remaining_seconds")] int? IdleRemainingSeconds = null
);

public record IsolatedSessionSummary(
    [property: JsonPropertyName("session_id")] string SessionId,
    [property: JsonPropertyName("status")] string Status,
    [property: JsonPropertyName("created_at")] DateTimeOffset? CreatedAt = null,
    [property: JsonPropertyName("last_run_at")] DateTimeOffset? LastRunAt = null,
    [property: JsonPropertyName("idle_remaining_seconds")] int? IdleRemainingSeconds = null
);

public record ListIsolatedSessionsResponse(
    [property: JsonPropertyName("sessions")] List<IsolatedSessionSummary>? Sessions = null
);

public record IsolatedRunOpts(
    [property: JsonPropertyName("envs")] Dictionary<string, string>? Envs = null,
    [property: JsonPropertyName("timeout_seconds")] int? TimeoutSeconds = null
);

public record IsolatedCapabilities(
    [property: JsonPropertyName("available")] bool Available = false,
    [property: JsonPropertyName("isolator")] string? Isolator = null,
    [property: JsonPropertyName("version")] string? Version = null,
    [property: JsonPropertyName("message")] string? Message = null,
    [property: JsonPropertyName("commit_supported")] bool CommitSupported = false,
    [property: JsonPropertyName("diff_supported")] bool DiffSupported = false
);
