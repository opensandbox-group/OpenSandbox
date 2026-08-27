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

using System;
using System.Collections.Concurrent;
using System.Collections.Generic;
using System.Diagnostics;
using System.Threading;
using System.Threading.Tasks;
using OpenSandbox.Models;

namespace OpenSandbox.Core;

internal readonly record struct EndpointCacheKey(string SandboxId, int Port, bool UseServerProxy);

/// <summary>
/// Thread-safe LRU + TTL endpoint cache with inflight deduplication.
/// </summary>
internal sealed class EndpointCache
{
    private readonly int _maxSize;
    private readonly TimeSpan _ttl;
    private readonly object _lock = new();
    private readonly Dictionary<EndpointCacheKey, LinkedListNode<CacheEntry>> _entries = new();
    private readonly LinkedList<CacheEntry> _order = new();
    private readonly ConcurrentDictionary<EndpointCacheKey, Lazy<Task<Endpoint>>> _inflight = new();
    private long _generation;

    private sealed record CacheEntry(EndpointCacheKey Key, Endpoint Endpoint, long ExpiresAtTimestamp);

    public EndpointCache(int maxSize = 1024, int ttlSeconds = 600)
    {
        _maxSize = maxSize > 0 ? maxSize : 1024;
        _ttl = TimeSpan.FromSeconds(ttlSeconds > 0 ? ttlSeconds : 600);
    }

    private static long GetTimestamp() => Stopwatch.GetTimestamp();

    private long TtlInTicks => (long)(_ttl.TotalSeconds * Stopwatch.Frequency);

    public Endpoint? Get(EndpointCacheKey key)
    {
        lock (_lock)
        {
            if (!_entries.TryGetValue(key, out var node))
                return null;

            if (GetTimestamp() >= node.Value.ExpiresAtTimestamp)
            {
                RemoveLocked(node);
                return null;
            }

            _order.Remove(node);
            _order.AddFirst(node);
            return node.Value.Endpoint;
        }
    }

    public void Put(EndpointCacheKey key, Endpoint endpoint)
    {
        lock (_lock)
        {
            PutLocked(key, endpoint);
        }
    }

    private void PutLocked(EndpointCacheKey key, Endpoint endpoint)
    {
        if (_entries.TryGetValue(key, out var existing))
        {
            _order.Remove(existing);
            _entries.Remove(key);
        }

        while (_order.Count >= _maxSize)
        {
            var last = _order.Last;
            if (last == null) break;
            RemoveLocked(last);
        }

        var entry = new CacheEntry(key, endpoint, GetTimestamp() + TtlInTicks);
        var node = _order.AddFirst(entry);
        _entries[key] = node;
    }

    public void Invalidate(string sandboxId)
    {
        lock (_lock)
        {
            _generation++;
            var toRemove = new List<LinkedListNode<CacheEntry>>();
            foreach (var kvp in _entries)
            {
                if (kvp.Key.SandboxId == sandboxId)
                    toRemove.Add(kvp.Value);
            }

            foreach (var node in toRemove)
                RemoveLocked(node);
        }
        foreach (var key in _inflight.Keys)
        {
            if (key.SandboxId == sandboxId)
                _inflight.TryRemove(key, out _);
        }
    }

    public async Task<Endpoint> GetOrFetchAsync(
        EndpointCacheKey key,
        Func<Task<Endpoint>> fetcher,
        CancellationToken cancellationToken = default)
    {
        var cached = Get(key);
        if (cached != null)
            return cached;

        long genBefore;
        lock (_lock) { genBefore = _generation; }
        var lazy = _inflight.GetOrAdd(key, _ => new Lazy<Task<Endpoint>>(() => FetchAndCache(key, fetcher, genBefore)));

        try
        {
            var fetchTask = lazy.Value;
            if (cancellationToken.CanBeCanceled)
            {
                var tcs = new TaskCompletionSource<bool>();
                using (cancellationToken.Register(() => tcs.TrySetResult(true)))
                {
                    if (await Task.WhenAny(fetchTask, tcs.Task).ConfigureAwait(false) == tcs.Task)
                    {
                        cancellationToken.ThrowIfCancellationRequested();
                    }
                }
            }
            return await fetchTask.ConfigureAwait(false);
        }
        finally
        {
            _inflight.TryRemove(key, out _);
        }
    }

    private async Task<Endpoint> FetchAndCache(EndpointCacheKey key, Func<Task<Endpoint>> fetcher, long genBefore)
    {
        var endpoint = await fetcher().ConfigureAwait(false);
        lock (_lock)
        {
            if (_generation == genBefore)
                PutLocked(key, endpoint);
        }
        return endpoint;
    }

    public int Count
    {
        get { lock (_lock) { return _order.Count; } }
    }

    private void RemoveLocked(LinkedListNode<CacheEntry> node)
    {
        _entries.Remove(node.Value.Key);
        _order.Remove(node);
    }
}
