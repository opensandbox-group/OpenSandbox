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

using System.Diagnostics;
using OpenSandbox.Core;

namespace OpenSandbox.Internal;

internal sealed class ReadinessBudget : IDisposable
{
    private readonly Stopwatch _clock = Stopwatch.StartNew();
    private readonly TimeSpan _timeout;
    private readonly CancellationToken _caller;
    private readonly CancellationTokenSource _source;
    private Exception? _lastError;
    private string? _context;
    private int _attempts;

    internal ReadinessBudget(double seconds, CancellationToken caller)
    {
        _timeout = TimeSpan.FromSeconds(seconds);
        _caller = caller;
        _source = CancellationTokenSource.CreateLinkedTokenSource(caller);
        _source.CancelAfter(_timeout > TimeSpan.Zero ? _timeout : TimeSpan.Zero);
    }

    internal void HealthContext(string context)
    {
        _context = context;
        _lastError = null;
    }
    internal void Attempt() => _attempts++;

    internal void Record(Exception? error) => _lastError = error;

    internal TimeSpan Remaining()
    {
        _caller.ThrowIfCancellationRequested();
        var remaining = _timeout - _clock.Elapsed;
        if (remaining <= TimeSpan.Zero || _source.IsCancellationRequested)
            throw new SandboxReadyTimeoutException(_context == null
                ? $"Sandbox readiness timed out. Last error: {_lastError?.Message ?? "Endpoint not yet available."}"
                : $"Sandbox health check timed out after {_timeout.TotalSeconds}s ({_attempts} attempts). {(_lastError == null ? "Health check returned false continuously." : $"Last health check error: {_lastError.Message}")} Connection context: {_context}.", _lastError);
        return remaining;
    }

    internal async Task<T> Run<T>(Func<CancellationToken, Task<T>> action)
    {
        Remaining();
        try
        {
            var task = action(_source.Token);
            var cancelled = new TaskCompletionSource<bool>(TaskCreationOptions.RunContinuationsAsynchronously);
            using var registration = _source.Token.Register(() => cancelled.TrySetResult(true));
            if (await Task.WhenAny(task, cancelled.Task).ConfigureAwait(false) != task)
            {
                _ = task.ContinueWith(t => { _ = t.Exception; }, TaskContinuationOptions.OnlyOnFaulted);
                throw new OperationCanceledException(_source.Token);
            }
            var result = await task.ConfigureAwait(false);
            Remaining();
            return result;
        }
        catch (Exception)
        {
            Remaining();
            throw;
        }
    }

    internal async Task Pause(int milliseconds)
    {
        var delay = TimeSpan.FromMilliseconds(Math.Min(milliseconds, Remaining().TotalMilliseconds));
        try { await Task.Delay(delay, _source.Token).ConfigureAwait(false); }
        catch (OperationCanceledException) { Remaining(); throw; }
        Remaining();
    }

    internal async Task<T> Endpoint<T>(Func<CancellationToken, Task<T>> action, int interval)
    {
        while (true)
        {
            try { return await Run(action).ConfigureAwait(false); }
            catch (SandboxApiException error) when (error.StatusCode == 404 && error.Error.Code == "KUBERNETES::POD_IP_NOT_AVAILABLE")
            { Record(error); }
            await Pause(interval).ConfigureAwait(false);
        }
    }

    public void Dispose() => _source.Dispose();
}
