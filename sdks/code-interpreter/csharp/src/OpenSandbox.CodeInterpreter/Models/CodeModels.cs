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

using System.Text.Json.Serialization;

namespace OpenSandbox.CodeInterpreter.Models;

/// <summary>
/// Supported programming languages for code execution.
/// </summary>
public static class SupportedLanguage
{
    /// <summary>
    /// Python language.
    /// </summary>
    public const string Python = "python";

    /// <summary>
    /// Java language.
    /// </summary>
    public const string Java = "java";

    /// <summary>
    /// Go language.
    /// </summary>
    public const string Go = "go";

    /// <summary>
    /// TypeScript language.
    /// </summary>
    public const string TypeScript = "typescript";

    /// <summary>
    /// JavaScript language.
    /// </summary>
    public const string JavaScript = "javascript";

    /// <summary>
    /// Bash shell.
    /// </summary>
    public const string Bash = "bash";
}

/// <summary>
/// Represents a code execution context.
/// </summary>
public class CodeContext
{
    /// <summary>
    /// Gets or sets the context ID.
    /// </summary>
    [JsonPropertyName("id")]
    public string? Id { get; set; }

    /// <summary>
    /// Gets or sets the programming language.
    /// </summary>
    [JsonPropertyName("language")]
    public required string Language { get; set; }
}

/// <summary>
/// Request to run code.
/// </summary>
public class RunCodeRequest
{
    /// <summary>
    /// Gets or sets the code to execute.
    /// </summary>
    [JsonPropertyName("code")]
    public required string Code { get; set; }

    /// <summary>
    /// Gets or sets the execution context.
    /// </summary>
    [JsonPropertyName("context")]
    public required CodeContext Context { get; set; }
}

/// <summary>
/// Options for running code.
/// </summary>
public class RunCodeOptions
{
    /// <summary>
    /// Gets or sets the execution context. If provided, code runs in this context.
    /// </summary>
    public CodeContext? Context { get; set; }

    /// <summary>
    /// Gets or sets the language for a new ephemeral context.
    /// Cannot be used together with Context.
    /// </summary>
    /// <remarks>
    /// When only <see cref="Language"/> is provided and <see cref="Context"/> is null, execd creates or reuses
    /// a default session for that language, so state can persist across runs.
    /// </remarks>
    public string? Language { get; set; }

    /// <summary>
    /// Gets or sets the execution event handlers.
    /// </summary>
    public OpenSandbox.Models.ExecutionHandlers? Handlers { get; set; }
}

/// <summary>
/// Request to create a code context.
/// </summary>
internal class CreateContextRequest
{
    /// <summary>
    /// Gets or sets the programming language.
    /// </summary>
    [JsonPropertyName("language")]
    public required string Language { get; set; }
}
