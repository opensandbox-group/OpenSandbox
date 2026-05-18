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

using OpenSandbox.Models;

namespace OpenSandbox.Internal;

/// <summary>
/// Dispatches streamed execution events to handlers and builds the execution result.
/// </summary>
internal sealed class ExecutionEventDispatcher
{
    private readonly Execution _execution;
    private readonly ExecutionHandlers? _handlers;

    public ExecutionEventDispatcher(Execution execution, ExecutionHandlers? handlers = null)
    {
        _execution = execution ?? throw new ArgumentNullException(nameof(execution));
        _handlers = handlers;
    }

    public async Task DispatchAsync(ServerStreamEvent ev)
    {
        if (ev.Eid.HasValue && ev.Eid.Value > _execution.LastEid)
        {
            _execution.LastEid = ev.Eid.Value;
        }

        var timestamp = ev.Timestamp ?? DateTimeOffset.UtcNow.ToUnixTimeMilliseconds();

        switch (ev.Type)
        {
            case ServerStreamEventTypes.Init:
                await HandleInitAsync(ev, timestamp).ConfigureAwait(false);
                break;

            case ServerStreamEventTypes.Stdout:
                await HandleStdoutAsync(ev, timestamp).ConfigureAwait(false);
                break;

            case ServerStreamEventTypes.Stderr:
                await HandleStderrAsync(ev, timestamp).ConfigureAwait(false);
                break;

            case ServerStreamEventTypes.Result:
                await HandleResultAsync(ev, timestamp).ConfigureAwait(false);
                break;

            case ServerStreamEventTypes.ExecutionCount:
                HandleExecutionCount(ev);
                break;

            case ServerStreamEventTypes.ExecutionComplete:
                await HandleExecutionCompleteAsync(ev, timestamp).ConfigureAwait(false);
                break;

            case ServerStreamEventTypes.Error:
                await HandleErrorAsync(ev, timestamp).ConfigureAwait(false);
                break;
        }
    }

    private async Task HandleInitAsync(ServerStreamEvent ev, long timestamp)
    {
        var id = ev.Text ?? string.Empty;
        if (!string.IsNullOrEmpty(id))
        {
            _execution.Id = id;
        }

        var init = new ExecutionInit
        {
            Id = id,
            Timestamp = timestamp
        };

        if (_handlers?.OnInit != null)
        {
            await _handlers.OnInit(init).ConfigureAwait(false);
        }
    }

    private async Task HandleStdoutAsync(ServerStreamEvent ev, long timestamp)
    {
        var msg = new OutputMessage
        {
            Text = ev.Text ?? string.Empty,
            Timestamp = timestamp,
            IsError = false
        };

        _execution.Logs.Stdout.Add(msg);

        if (_handlers?.OnStdout != null)
        {
            await _handlers.OnStdout(msg).ConfigureAwait(false);
        }
    }

    private async Task HandleStderrAsync(ServerStreamEvent ev, long timestamp)
    {
        var msg = new OutputMessage
        {
            Text = ev.Text ?? string.Empty,
            Timestamp = timestamp,
            IsError = true
        };

        _execution.Logs.Stderr.Add(msg);

        if (_handlers?.OnStderr != null)
        {
            await _handlers.OnStderr(msg).ConfigureAwait(false);
        }
    }

    private async Task HandleResultAsync(ServerStreamEvent ev, long timestamp)
    {
        var text = ExtractText(ev.Results);
        var result = new ExecutionResult
        {
            Text = text,
            Timestamp = timestamp,
            Raw = ev.Results?.ToDictionary(kv => kv.Key, kv => (object)kv.Value)
        };

        _execution.Results.Add(result);

        if (_handlers?.OnResult != null)
        {
            await _handlers.OnResult(result).ConfigureAwait(false);
        }
    }

    private void HandleExecutionCount(ServerStreamEvent ev)
    {
        if (ev.ExecutionCount.HasValue)
        {
            _execution.ExecutionCount = ev.ExecutionCount.Value;
        }
    }

    private async Task HandleExecutionCompleteAsync(ServerStreamEvent ev, long timestamp)
    {
        var complete = new ExecutionComplete
        {
            Timestamp = timestamp,
            ExecutionTimeMs = ev.ExecutionTime ?? 0
        };

        _execution.Complete = complete;

        if (_handlers?.OnExecutionComplete != null)
        {
            await _handlers.OnExecutionComplete(complete).ConfigureAwait(false);
        }
    }

    private async Task HandleErrorAsync(ServerStreamEvent ev, long timestamp)
    {
        if (ev.Error == null)
            return;

        var error = new ExecutionError
        {
            Name = GetStringValue(ev.Error, "ename") ?? GetStringValue(ev.Error, "name") ?? string.Empty,
            Value = GetStringValue(ev.Error, "evalue") ?? GetStringValue(ev.Error, "value") ?? string.Empty,
            Timestamp = timestamp,
            Traceback = GetStringArrayValue(ev.Error, "traceback") ?? Array.Empty<string>()
        };

        _execution.Error = error;

        if (_handlers?.OnError != null)
        {
            await _handlers.OnError(error).ConfigureAwait(false);
        }
    }

    private static string? ExtractText(Dictionary<string, object>? results)
    {
        if (results == null)
            return null;

        if (results.TryGetValue("text/plain", out var textPlain))
            return textPlain?.ToString();

        if (results.TryGetValue("text", out var text))
            return text?.ToString();

        if (results.TryGetValue("textPlain", out var textPlain2))
            return textPlain2?.ToString();

        return null;
    }

    private static string? GetStringValue(Dictionary<string, object> dict, string key)
    {
        if (dict.TryGetValue(key, out var value))
            return value?.ToString();
        return null;
    }

    private static IReadOnlyList<string>? GetStringArrayValue(Dictionary<string, object> dict, string key)
    {
        if (!dict.TryGetValue(key, out var value))
            return null;

        if (value is IEnumerable<object> enumerable)
            return enumerable.Select(x => x?.ToString() ?? string.Empty).ToList();

        if (value is System.Text.Json.JsonElement jsonElement && jsonElement.ValueKind == System.Text.Json.JsonValueKind.Array)
            return jsonElement.EnumerateArray().Select(x => x.GetString() ?? string.Empty).ToList();

        return null;
    }
}
