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
/// An output message from command execution.
/// </summary>
public class OutputMessage
{
    /// <summary>
    /// Gets or sets the text content.
    /// </summary>
    public required string Text { get; set; }

    /// <summary>
    /// Gets or sets the timestamp in milliseconds.
    /// </summary>
    public required long Timestamp { get; set; }

    /// <summary>
    /// Gets or sets whether this is an error message.
    /// </summary>
    public bool IsError { get; set; }
}

/// <summary>
/// A result from command execution.
/// </summary>
public class ExecutionResult
{
    /// <summary>
    /// Gets or sets the text content.
    /// </summary>
    public string? Text { get; set; }

    /// <summary>
    /// Gets or sets the timestamp in milliseconds.
    /// </summary>
    public required long Timestamp { get; set; }

    /// <summary>
    /// Gets or sets the raw mime map from execd event.
    /// </summary>
    public IReadOnlyDictionary<string, object>? Raw { get; set; }
}

/// <summary>
/// An error from command execution.
/// </summary>
public class ExecutionError
{
    /// <summary>
    /// Gets or sets the error name.
    /// </summary>
    public required string Name { get; set; }

    /// <summary>
    /// Gets or sets the error value.
    /// </summary>
    public required string Value { get; set; }

    /// <summary>
    /// Gets or sets the timestamp in milliseconds.
    /// </summary>
    public required long Timestamp { get; set; }

    /// <summary>
    /// Gets or sets the traceback lines.
    /// </summary>
    public required IReadOnlyList<string> Traceback { get; set; }
}

/// <summary>
/// Completion information for command execution.
/// </summary>
public class ExecutionComplete
{
    /// <summary>
    /// Gets or sets the timestamp in milliseconds.
    /// </summary>
    public required long Timestamp { get; set; }

    /// <summary>
    /// Gets or sets the execution time in milliseconds.
    /// </summary>
    public required long ExecutionTimeMs { get; set; }
}

/// <summary>
/// Initialization information for command execution.
/// </summary>
public class ExecutionInit
{
    /// <summary>
    /// Gets or sets the execution ID.
    /// </summary>
    public required string Id { get; set; }

    /// <summary>
    /// Gets or sets the timestamp in milliseconds.
    /// </summary>
    public required long Timestamp { get; set; }
}

/// <summary>
/// Logs from command execution.
/// </summary>
public class ExecutionLogs
{
    /// <summary>
    /// Gets the stdout messages.
    /// </summary>
    public List<OutputMessage> Stdout { get; } = new();

    /// <summary>
    /// Gets the stderr messages.
    /// </summary>
    public List<OutputMessage> Stderr { get; } = new();
}

/// <summary>
/// Result of a command execution.
/// </summary>
public class Execution
{
    /// <summary>
    /// Gets or sets the execution ID.
    /// </summary>
    public string? Id { get; set; }

    /// <summary>
    /// Gets or sets the execution count.
    /// </summary>
    public int? ExecutionCount { get; set; }

    /// <summary>
    /// Gets the execution logs.
    /// </summary>
    public ExecutionLogs Logs { get; } = new();

    /// <summary>
    /// Gets the execution results.
    /// </summary>
    public List<ExecutionResult> Results { get; } = new();

    /// <summary>
    /// Gets or sets the execution error.
    /// </summary>
    public ExecutionError? Error { get; set; }

    /// <summary>
    /// Gets or sets the completion information.
    /// </summary>
    public ExecutionComplete? Complete { get; set; }

    /// <summary>
    /// Gets or sets the command exit code when available.
    /// </summary>
    public int? ExitCode { get; set; }

    /// <summary>
    /// Gets or sets the highest event ID seen, used for SSE resume.
    /// </summary>
    public long LastEid { get; set; }
}

/// <summary>
/// Handlers for execution events.
/// </summary>
public class ExecutionHandlers
{
    /// <summary>
    /// Gets or sets the handler for stdout messages.
    /// </summary>
    public Func<OutputMessage, Task>? OnStdout { get; set; }

    /// <summary>
    /// Gets or sets the handler for stderr messages.
    /// </summary>
    public Func<OutputMessage, Task>? OnStderr { get; set; }

    /// <summary>
    /// Gets or sets the handler for execution results.
    /// </summary>
    public Func<ExecutionResult, Task>? OnResult { get; set; }

    /// <summary>
    /// Gets or sets the handler for execution completion.
    /// </summary>
    public Func<ExecutionComplete, Task>? OnExecutionComplete { get; set; }

    /// <summary>
    /// Gets or sets the handler for execution errors.
    /// </summary>
    public Func<ExecutionError, Task>? OnError { get; set; }

    /// <summary>
    /// Gets or sets the handler for execution initialization.
    /// </summary>
    public Func<ExecutionInit, Task>? OnInit { get; set; }
}
