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

using System.Text.Json;
using System.Text.Json.Serialization;

namespace OpenSandbox.Models;

/// <summary>
/// A command inventory summary.
/// </summary>
public abstract class CommandSummary
{
    /// <summary>Gets the command identity, equal to the legacy command status ID.</summary>
    public required string Session { get; init; }

    /// <summary>Gets whether the command is still running.</summary>
    public abstract bool Running { get; }

    /// <summary>Gets whether the command runs in the background.</summary>
    public required bool Background { get; init; }

    /// <summary>
    /// Gets when the command started. The wire format accepts RFC3339Nano fractions through
    /// nine digits; this <see cref="DateTimeOffset"/> representation truncates fractions beyond
    /// its seven-digit precision.
    /// </summary>
    public required DateTimeOffset StartedAt { get; init; }
}

/// <summary>
/// A command inventory summary for a command that is still running.
/// </summary>
public sealed class RunningCommandSummary : CommandSummary
{
    /// <inheritdoc />
    public override bool Running => true;
}

/// <summary>
/// A command inventory summary for a command that has finished.
/// </summary>
public sealed class TerminalCommandSummary : CommandSummary
{
    /// <inheritdoc />
    public override bool Running => false;

    /// <summary>
    /// Gets when the command finished. The wire format accepts RFC3339Nano fractions through
    /// nine digits; this <see cref="DateTimeOffset"/> representation truncates fractions beyond
    /// its seven-digit precision.
    /// </summary>
    public required DateTimeOffset FinishedAt { get; init; }

    /// <summary>Gets the exit code, which is present on the wire and may be null.</summary>
    public int? ExitCode { get; init; }

    /// <summary>Gets the optional terminal error message.</summary>
    public string? Error { get; init; }
}

/// <summary>
/// Pagination metadata for a command inventory page.
/// </summary>
public sealed class CommandPagination
{
    /// <summary>Gets the page limit used by the server.</summary>
    public required int Limit { get; init; }

    /// <summary>Gets the opaque cursor for the next page, or null for the final page.</summary>
    public string? NextCursor { get; init; }
}

/// <summary>
/// A weakly consistent page of command inventory entries.
/// </summary>
[JsonConverter(typeof(ListCommandsPageJsonConverter))]
public sealed class ListCommandsPage
{
    /// <summary>Gets the command summaries in this page.</summary>
    public required IReadOnlyList<CommandSummary> Commands { get; init; }

    /// <summary>Gets the pagination metadata.</summary>
    public required CommandPagination Pagination { get; init; }
}

internal sealed class ListCommandsPageJsonConverter : JsonConverter<ListCommandsPage>
{
    private static readonly HashSet<string> RunningProperties = new(StringComparer.Ordinal)
    {
        "session", "running", "background", "started_at"
    };

    private static readonly HashSet<string> TerminalProperties = new(StringComparer.Ordinal)
    {
        "session", "running", "background", "started_at", "finished_at", "exit_code", "error"
    };

    public override ListCommandsPage Read(ref Utf8JsonReader reader, Type typeToConvert, JsonSerializerOptions options)
    {
        using var document = JsonDocument.ParseValue(ref reader);
        var root = document.RootElement;
        RequireObject(root, "command inventory page");

        var commandsElement = RequireProperty(root, "commands", "command inventory page");
        if (commandsElement.ValueKind != JsonValueKind.Array)
        {
            throw Invalid("command inventory page commands must be an array");
        }

        var commands = new List<CommandSummary>();
        foreach (var element in commandsElement.EnumerateArray())
        {
            commands.Add(ParseSummary(element));
        }

        return new ListCommandsPage
        {
            Commands = commands,
            Pagination = ParsePagination(RequireProperty(root, "pagination", "command inventory page"))
        };
    }

    public override void Write(Utf8JsonWriter writer, ListCommandsPage value, JsonSerializerOptions options)
    {
        writer.WriteStartObject();
        writer.WritePropertyName("commands");
        writer.WriteStartArray();
        foreach (var command in value.Commands)
        {
            writer.WriteStartObject();
            writer.WriteString("session", command.Session);
            writer.WriteBoolean("running", command.Running);
            writer.WriteBoolean("background", command.Background);
            writer.WriteString("started_at", command.StartedAt);
            if (command is TerminalCommandSummary terminal)
            {
                writer.WriteString("finished_at", terminal.FinishedAt);
                writer.WritePropertyName("exit_code");
                if (terminal.ExitCode.HasValue)
                {
                    writer.WriteNumberValue(terminal.ExitCode.Value);
                }
                else
                {
                    writer.WriteNullValue();
                }

                if (terminal.Error is not null)
                {
                    writer.WriteString("error", terminal.Error);
                }
            }

            writer.WriteEndObject();
        }

        writer.WriteEndArray();
        writer.WritePropertyName("pagination");
        writer.WriteStartObject();
        writer.WriteNumber("limit", value.Pagination.Limit);
        if (value.Pagination.NextCursor is not null)
        {
            writer.WriteString("nextCursor", value.Pagination.NextCursor);
        }

        writer.WriteEndObject();
        writer.WriteEndObject();
    }

    private static CommandPagination ParsePagination(JsonElement element)
    {
        RequireObject(element, "command inventory pagination");
        var limit = RequireProperty(element, "limit", "command inventory pagination");
        if (limit.ValueKind != JsonValueKind.Number || !limit.TryGetInt32(out var limitValue) || limitValue is < 1 or > 100)
        {
            throw Invalid("command inventory pagination limit must be an integer between 1 and 100");
        }

        string? nextCursor = null;
        if (element.TryGetProperty("nextCursor", out var cursor))
        {
            nextCursor = RequireString(cursor, "command inventory pagination nextCursor");
            if (string.IsNullOrWhiteSpace(nextCursor))
            {
                throw Invalid("command inventory pagination nextCursor must not be blank");
            }
        }

        return new CommandPagination { Limit = limitValue, NextCursor = nextCursor };
    }

    private static CommandSummary ParseSummary(JsonElement element)
    {
        RequireObject(element, "command summary");
        var running = RequireProperty(element, "running", "command summary");
        if (running.ValueKind is not JsonValueKind.True and not JsonValueKind.False)
        {
            throw Invalid("command summary running must be a boolean");
        }

        var isRunning = running.GetBoolean();
        RequireOnlyProperties(element, isRunning ? RunningProperties : TerminalProperties, "command summary");
        var session = RequireString(RequireProperty(element, "session", "command summary"), "command summary session");
        var background = RequireBoolean(RequireProperty(element, "background", "command summary"), "command summary background");
        var startedAt = RequireDateTime(RequireProperty(element, "started_at", "command summary"), "command summary started_at");

        if (isRunning)
        {
            return new RunningCommandSummary
            {
                Session = session,
                Background = background,
                StartedAt = startedAt
            };
        }

        var exitCode = RequireProperty(element, "exit_code", "terminal command summary");
        if (exitCode.ValueKind is not JsonValueKind.Null and not JsonValueKind.Number)
        {
            throw Invalid("terminal command summary exit_code must be an integer or null");
        }

        int? exitCodeValue = null;
        if (exitCode.ValueKind == JsonValueKind.Number)
        {
            if (!exitCode.TryGetInt32(out var parsedExitCode))
            {
                throw Invalid("terminal command summary exit_code must be an integer or null");
            }

            exitCodeValue = parsedExitCode;
        }

        string? error = null;
        if (element.TryGetProperty("error", out var errorElement))
        {
            error = RequireString(errorElement, "terminal command summary error");
        }

        return new TerminalCommandSummary
        {
            Session = session,
            Background = background,
            StartedAt = startedAt,
            FinishedAt = RequireDateTime(RequireProperty(element, "finished_at", "terminal command summary"), "terminal command summary finished_at"),
            ExitCode = exitCodeValue,
            Error = error
        };
    }

    private static void RequireObject(JsonElement element, string name)
    {
        if (element.ValueKind != JsonValueKind.Object)
        {
            throw Invalid($"{name} must be an object");
        }
    }

    private static void RequireOnlyProperties(JsonElement element, ISet<string> allowedProperties, string name)
    {
        var seen = new HashSet<string>(StringComparer.Ordinal);
        foreach (var property in element.EnumerateObject())
        {
            if (!allowedProperties.Contains(property.Name) || !seen.Add(property.Name))
            {
                throw Invalid($"{name} contains an unsupported property: {property.Name}");
            }
        }
    }

    private static JsonElement RequireProperty(JsonElement element, string propertyName, string name)
    {
        return element.TryGetProperty(propertyName, out var property)
            ? property
            : throw Invalid($"{name} is missing required property: {propertyName}");
    }

    private static string RequireString(JsonElement element, string name)
    {
        if (element.ValueKind != JsonValueKind.String)
        {
            throw Invalid($"{name} must be a string");
        }

        return element.GetString()!;
    }

    private static bool RequireBoolean(JsonElement element, string name)
    {
        if (element.ValueKind is not JsonValueKind.True and not JsonValueKind.False)
        {
            throw Invalid($"{name} must be a boolean");
        }

        return element.GetBoolean();
    }

    private static DateTimeOffset RequireDateTime(JsonElement element, string name)
    {
        var value = RequireString(element, name);
        if (!TryNormalizeRfc3339Nano(value, out var normalized))
        {
            throw Invalid($"{name} must be an RFC3339 date-time");
        }

        if (!DateTimeOffset.TryParseExact(
                normalized,
                new[] { "yyyy-MM-dd'T'HH:mm:ssK", "yyyy-MM-dd'T'HH:mm:ss.FFFFFFFK" },
                System.Globalization.CultureInfo.InvariantCulture,
                System.Globalization.DateTimeStyles.None,
                out var parsed))
        {
            throw Invalid($"{name} must be an RFC3339 date-time");
        }

        return parsed;
    }

    private static bool TryNormalizeRfc3339Nano(string value, out string normalized)
    {
        normalized = value;
        if (value.Length < 20 || value[4] != '-' || value[7] != '-' || value[10] != 'T' ||
            value[13] != ':' || value[16] != ':')
        {
            return false;
        }

        if (!AllAsciiDigits(value, 0, 4) || !AllAsciiDigits(value, 5, 2) ||
            !AllAsciiDigits(value, 8, 2) || !AllAsciiDigits(value, 11, 2) ||
            !AllAsciiDigits(value, 14, 2) || !AllAsciiDigits(value, 17, 2))
        {
            return false;
        }

        var position = 19;
        if (position < value.Length && value[position] == '.')
        {
            var fractionStart = ++position;
            while (position < value.Length && value[position] is >= '0' and <= '9')
            {
                position++;
            }

            var fractionLength = position - fractionStart;
            if (fractionLength is < 1 or > 9)
            {
                return false;
            }

            if (fractionLength > 7)
            {
                normalized = value.Remove(fractionStart + 7, fractionLength - 7);
            }
        }

        if (position == value.Length - 1 && value[position] == 'Z')
        {
            return true;
        }

        if (position + 6 != value.Length || (value[position] != '+' && value[position] != '-') ||
            value[position + 3] != ':' || !AllAsciiDigits(value, position + 1, 2) ||
            !AllAsciiDigits(value, position + 4, 2))
        {
            return false;
        }

        return true;
    }

    private static bool AllAsciiDigits(string value, int start, int length)
    {
        for (var index = start; index < start + length; index++)
        {
            if (value[index] is < '0' or > '9')
            {
                return false;
            }
        }

        return true;
    }

    private static JsonException Invalid(string message) => new(message);
}
