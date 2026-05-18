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
/// A server-sent event from command execution.
/// </summary>
public class ServerStreamEvent
{
    /// <summary>
    /// Gets or sets the event type.
    /// </summary>
    [JsonPropertyName("type")]
    public required string Type { get; set; }

    /// <summary>
    /// Gets or sets the timestamp in milliseconds.
    /// </summary>
    [JsonPropertyName("timestamp")]
    public long? Timestamp { get; set; }

    /// <summary>
    /// Gets or sets the text content.
    /// </summary>
    [JsonPropertyName("text")]
    public string? Text { get; set; }

    /// <summary>
    /// Gets or sets the results map.
    /// </summary>
    [JsonPropertyName("results")]
    public Dictionary<string, object>? Results { get; set; }

    /// <summary>
    /// Gets or sets the error information.
    /// </summary>
    [JsonPropertyName("error")]
    public Dictionary<string, object>? Error { get; set; }

    /// <summary>
    /// Gets or sets the execution count.
    /// </summary>
    [JsonPropertyName("execution_count")]
    public int? ExecutionCount { get; set; }

    /// <summary>
    /// Gets or sets the execution time in milliseconds.
    /// </summary>
    [JsonPropertyName("execution_time")]
    public long? ExecutionTime { get; set; }

    /// <summary>
    /// Gets or sets the monotonic event ID for SSE resume.
    /// </summary>
    [JsonPropertyName("eid")]
    public long? Eid { get; set; }
}

/// <summary>
/// Known event types for server stream events.
/// </summary>
public static class ServerStreamEventTypes
{
    /// <summary>
    /// Initialization event.
    /// </summary>
    public const string Init = "init";

    /// <summary>
    /// Standard output event.
    /// </summary>
    public const string Stdout = "stdout";

    /// <summary>
    /// Standard error event.
    /// </summary>
    public const string Stderr = "stderr";

    /// <summary>
    /// Result event.
    /// </summary>
    public const string Result = "result";

    /// <summary>
    /// Execution count event.
    /// </summary>
    public const string ExecutionCount = "execution_count";

    /// <summary>
    /// Execution complete event.
    /// </summary>
    public const string ExecutionComplete = "execution_complete";

    /// <summary>
    /// Error event.
    /// </summary>
    public const string Error = "error";
}

/// <summary>
/// Request to run a command.
/// </summary>
public class RunCommandRequest
{
    /// <summary>
    /// Gets or sets the command to run.
    /// </summary>
    [JsonPropertyName("command")]
    public required string Command { get; set; }

    /// <summary>
    /// Gets or sets the working directory.
    /// </summary>
    [JsonPropertyName("cwd")]
    public string? Cwd { get; set; }

    /// <summary>
    /// Gets or sets whether to run in background.
    /// </summary>
    [JsonPropertyName("background")]
    public bool? Background { get; set; }

    /// <summary>
    /// Gets or sets the maximum execution time in milliseconds.
    /// </summary>
    [JsonPropertyName("timeout")]
    public long? Timeout { get; set; }

    /// <summary>
    /// Gets or sets the Unix user ID used to run the command process.
    /// </summary>
    [JsonPropertyName("uid")]
    public int? Uid { get; set; }

    /// <summary>
    /// Gets or sets the Unix group ID used to run the command process.
    /// Requires <see cref="Uid"/> to be set.
    /// </summary>
    [JsonPropertyName("gid")]
    public int? Gid { get; set; }

    /// <summary>
    /// Gets or sets environment variables injected into the command process.
    /// </summary>
    [JsonPropertyName("envs")]
    public Dictionary<string, string>? Envs { get; set; }
}

/// <summary>
/// Options for running a command.
/// </summary>
public class RunCommandOptions
{
    /// <summary>
    /// Gets or sets the working directory for command execution.
    /// </summary>
    public string? WorkingDirectory { get; set; }

    /// <summary>
    /// Gets or sets whether to run the command in detached mode.
    /// </summary>
    public bool Background { get; set; }

    /// <summary>
    /// Gets or sets the maximum execution time in seconds.
    /// The server terminates the command when this duration is reached.
    /// </summary>
    public int? TimeoutSeconds { get; set; }

    /// <summary>
    /// Gets or sets the Unix user ID used to run the command process.
    /// </summary>
    public int? Uid { get; set; }

    /// <summary>
    /// Gets or sets the Unix group ID used to run the command process.
    /// Requires <see cref="Uid"/> to be set.
    /// </summary>
    public int? Gid { get; set; }

    /// <summary>
    /// Gets or sets environment variables injected into the command process.
    /// </summary>
    public Dictionary<string, string>? Envs { get; set; }
}

/// <summary>
/// Status information for a foreground or background command.
/// </summary>
public class CommandStatus
{
    /// <summary>
    /// Gets or sets the command ID.
    /// </summary>
    [JsonPropertyName("id")]
    public string? Id { get; set; }

    /// <summary>
    /// Gets or sets the original command text.
    /// </summary>
    [JsonPropertyName("content")]
    public string? Content { get; set; }

    /// <summary>
    /// Gets or sets whether the command is still running.
    /// </summary>
    [JsonPropertyName("running")]
    public bool? Running { get; set; }

    /// <summary>
    /// Gets or sets the exit code when the command has finished.
    /// </summary>
    [JsonPropertyName("exit_code")]
    public int? ExitCode { get; set; }

    /// <summary>
    /// Gets or sets the error message if the command failed.
    /// </summary>
    [JsonPropertyName("error")]
    public string? Error { get; set; }

    /// <summary>
    /// Gets or sets the command start time in RFC3339 format.
    /// </summary>
    [JsonPropertyName("started_at")]
    public DateTime? StartedAt { get; set; }

    /// <summary>
    /// Gets or sets the command finish time in RFC3339 format.
    /// </summary>
    [JsonPropertyName("finished_at")]
    public DateTime? FinishedAt { get; set; }
}

/// <summary>
/// Background command logs and incremental cursor.
/// </summary>
public class CommandLogs
{
    /// <summary>
    /// Gets or sets raw stdout/stderr content.
    /// </summary>
    public required string Content { get; set; }

    /// <summary>
    /// Gets or sets the latest cursor for incremental log polling.
    /// </summary>
    public long? Cursor { get; set; }
}

/// <summary>
/// Supported programming languages for code execution.
/// </summary>
public static class SupportedLanguages
{
    /// <summary>
    /// Python language.
    /// </summary>
    public const string Python = "python";

    /// <summary>
    /// Go language.
    /// </summary>
    public const string Go = "go";

    /// <summary>
    /// JavaScript language.
    /// </summary>
    public const string JavaScript = "javascript";

    /// <summary>
    /// TypeScript language.
    /// </summary>
    public const string TypeScript = "typescript";

    /// <summary>
    /// Bash shell.
    /// </summary>
    public const string Bash = "bash";

    /// <summary>
    /// Java language.
    /// </summary>
    public const string Java = "java";
}

/// <summary>
/// Raw metrics from the execd service.
/// </summary>
public class Metrics
{
    /// <summary>
    /// Gets or sets the CPU count.
    /// </summary>
    [JsonPropertyName("cpu_count")]
    public int? CpuCount { get; set; }

    /// <summary>
    /// Gets or sets the CPU usage percentage.
    /// </summary>
    [JsonPropertyName("cpu_used_pct")]
    public double? CpuUsedPct { get; set; }

    /// <summary>
    /// Gets or sets the total memory in MiB.
    /// </summary>
    [JsonPropertyName("mem_total_mib")]
    public double? MemTotalMib { get; set; }

    /// <summary>
    /// Gets or sets the used memory in MiB.
    /// </summary>
    [JsonPropertyName("mem_used_mib")]
    public double? MemUsedMib { get; set; }

    /// <summary>
    /// Gets or sets the timestamp.
    /// </summary>
    [JsonPropertyName("timestamp")]
    public long? Timestamp { get; set; }
}

/// <summary>
/// Normalized sandbox metrics.
/// </summary>
public class SandboxMetrics
{
    /// <summary>
    /// Gets or sets the CPU count.
    /// </summary>
    public int CpuCount { get; set; }

    /// <summary>
    /// Gets or sets the CPU usage percentage.
    /// </summary>
    public double CpuUsedPercentage { get; set; }

    /// <summary>
    /// Gets or sets the total memory in MiB.
    /// </summary>
    public double MemoryTotalMiB { get; set; }

    /// <summary>
    /// Gets or sets the used memory in MiB.
    /// </summary>
    public double MemoryUsedMiB { get; set; }

    /// <summary>
    /// Gets or sets the timestamp.
    /// </summary>
    public long Timestamp { get; set; }
}

/// <summary>
/// Response from ping endpoint.
/// </summary>
public class PingResponse
{
    // Empty response - ping just returns 200 OK
}

// --- Bash session API (create_session, run_in_session, delete_session) ---

/// <summary>
/// Options for creating a bash session.
/// </summary>
public class CreateSessionOptions
{
    /// <summary>
    /// Gets or sets the optional working directory for the session.
    /// </summary>
    public string? WorkingDirectory { get; set; }
}

/// <summary>
/// Response from create_session (POST /session).
/// </summary>
public class CreateSessionResponse
{
    /// <summary>
    /// Gets or sets the session ID for run_in_session and delete_session.
    /// </summary>
    [JsonPropertyName("session_id")]
    public required string SessionId { get; set; }
}

/// <summary>
/// Options for running code in an existing bash session.
/// </summary>
public class RunInSessionOptions
{
    /// <summary>
    /// Gets or sets the optional working directory override for this run.
    /// </summary>
    public string? WorkingDirectory { get; set; }

    /// <summary>
    /// Gets or sets the maximum execution time in seconds.
    /// </summary>
    public int? TimeoutSeconds { get; set; }

}

/// <summary>
/// Request body for run_in_session (POST /session/{sessionId}/run).
/// </summary>
internal class RunInSessionRequest
{
    [JsonPropertyName("command")]
    public required string Command { get; set; }

    [JsonPropertyName("cwd")]
    public string? Cwd { get; set; }

    [JsonPropertyName("timeout")]
    public long? Timeout { get; set; }
}
