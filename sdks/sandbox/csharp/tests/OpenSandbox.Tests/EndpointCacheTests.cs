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

using OpenSandbox.Core;
using OpenSandbox.Models;
using Xunit;

namespace OpenSandbox.Tests;

public class EndpointCacheTests
{
    private static Endpoint Ep(string addr) => new() { EndpointAddress = addr, Headers = new Dictionary<string, string>() };

    [Fact]
    public void Get_ReturnsNull_OnMiss()
    {
        var cache = new EndpointCache(maxSize: 10, ttlSeconds: 60);
        var key = new EndpointCacheKey("sb-1", 8080, false);
        Assert.Null(cache.Get(key));
    }

    [Fact]
    public void Get_ReturnsEndpoint_AfterPut()
    {
        var cache = new EndpointCache(maxSize: 10, ttlSeconds: 60);
        var key = new EndpointCacheKey("sb-1", 8080, false);
        cache.Put(key, Ep("localhost:8080"));
        Assert.Equal("localhost:8080", cache.Get(key)?.EndpointAddress);
    }

    [Fact]
    public void Invalidate_RemovesAllEntriesForSandbox()
    {
        var cache = new EndpointCache(maxSize: 10, ttlSeconds: 60);
        cache.Put(new EndpointCacheKey("sb-1", 8080, false), Ep("a"));
        cache.Put(new EndpointCacheKey("sb-1", 18080, false), Ep("b"));
        cache.Put(new EndpointCacheKey("sb-2", 8080, false), Ep("c"));

        cache.Invalidate("sb-1");

        Assert.Null(cache.Get(new EndpointCacheKey("sb-1", 8080, false)));
        Assert.Null(cache.Get(new EndpointCacheKey("sb-1", 18080, false)));
        Assert.NotNull(cache.Get(new EndpointCacheKey("sb-2", 8080, false)));
    }

    [Fact]
    public async Task GetOrFetchAsync_DeduplicatesConcurrentRequests()
    {
        var cache = new EndpointCache(maxSize: 10, ttlSeconds: 60);
        var key = new EndpointCacheKey("sb-1", 8080, false);
        var fetchCount = 0;

        async Task<Endpoint> Fetcher()
        {
            Interlocked.Increment(ref fetchCount);
            await Task.Delay(50);
            return Ep("result");
        }

        var tasks = Enumerable.Range(0, 5)
            .Select(_ => cache.GetOrFetchAsync(key, Fetcher))
            .ToArray();

        var results = await Task.WhenAll(tasks);

        Assert.Equal(1, fetchCount);
        Assert.All(results, r => Assert.Equal("result", r.EndpointAddress));
    }

    [Fact]
    public async Task GetOrFetchAsync_ReturnsCachedValue_WithoutCallingFetcher()
    {
        var cache = new EndpointCache(maxSize: 10, ttlSeconds: 60);
        var key = new EndpointCacheKey("sb-1", 8080, false);
        cache.Put(key, Ep("cached"));
        var called = false;

        var result = await cache.GetOrFetchAsync(key, () =>
        {
            called = true;
            return Task.FromResult(Ep("fetched"));
        });

        Assert.Equal("cached", result.EndpointAddress);
        Assert.False(called);
    }

    [Fact]
    public async Task GetOrFetchAsync_DoesNotCache_OnError()
    {
        var cache = new EndpointCache(maxSize: 10, ttlSeconds: 60);
        var key = new EndpointCacheKey("sb-1", 8080, false);

        await Assert.ThrowsAsync<InvalidOperationException>(() =>
            cache.GetOrFetchAsync(key, () => throw new InvalidOperationException("network error")));

        Assert.Equal(0, cache.Count);
    }

    [Fact]
    public void LruEviction_WhenAtCapacity()
    {
        var cache = new EndpointCache(maxSize: 3, ttlSeconds: 60);
        cache.Put(new EndpointCacheKey("sb-0", 8080, false), Ep("h0"));
        cache.Put(new EndpointCacheKey("sb-1", 8080, false), Ep("h1"));
        cache.Put(new EndpointCacheKey("sb-2", 8080, false), Ep("h2"));

        // Access sb-0 to make it recently used
        cache.Get(new EndpointCacheKey("sb-0", 8080, false));
        // Insert 4th — sb-1 should be evicted
        cache.Put(new EndpointCacheKey("sb-3", 8080, false), Ep("h3"));

        Assert.Null(cache.Get(new EndpointCacheKey("sb-1", 8080, false)));
        Assert.NotNull(cache.Get(new EndpointCacheKey("sb-0", 8080, false)));
    }

    [Fact]
    public async Task GetOrFetchAsync_RemovesEntry_WhenAllWaitersCancelBeforeFault()
    {
        // Regression: the old finally-based removal left the entry behind
        // when all waiters cancelled; a later caller then got the cached
        // fault instead of a fresh fetch.
        var cache = new EndpointCache(maxSize: 10, ttlSeconds: 60);
        var key = new EndpointCacheKey("sb-1", 8080, false);
        var release = new TaskCompletionSource<bool>(TaskCreationOptions.RunContinuationsAsynchronously);
        var fetchCount = 0;

        async Task<Endpoint> Fetcher()
        {
            Interlocked.Increment(ref fetchCount);
            await release.Task;
            throw new InvalidOperationException("boom");
        }

        using var cts = new CancellationTokenSource();
        var waiter = cache.GetOrFetchAsync(key, Fetcher, cts.Token);
        await cts.CancelAsync();
        await Assert.ThrowsAnyAsync<OperationCanceledException>(() => waiter);

        // The fetch faults with every waiter gone; the entry must be dropped.
        release.TrySetResult(true);
        await Task.Yield();
        await Task.Delay(50);

        // A cached fault would surface "boom" here instead of refetching.
        var result = await cache.GetOrFetchAsync(
            key,
            () => { Interlocked.Increment(ref fetchCount); return Task.FromResult(Ep("fresh")); });
        Assert.Equal(2, fetchCount);
        Assert.Equal("fresh", result.EndpointAddress);
    }

    [Fact]
    public async Task GetOrFetchAsync_RemovesSettledEntry_EvenWithoutWaiters()
    {
        // Regression: a settled entry used to linger in the inflight map
        // when its only waiter cancelled, so a later cache-miss caller was
        // served the stale endpoint instead of triggering a fresh fetch.
        var cache = new EndpointCache(maxSize: 1, ttlSeconds: 60);
        var key = new EndpointCacheKey("sb-1", 8080, false);
        var other = new EndpointCacheKey("sb-2", 8080, false);
        var fetchCount = 0;
        var release = new TaskCompletionSource<bool>(TaskCreationOptions.RunContinuationsAsynchronously);

        async Task<Endpoint> Fetcher()
        {
            Interlocked.Increment(ref fetchCount);
            // Gate the fetch so it is still in flight when the waiter cancels.
            await release.Task;
            return Ep("first");
        }

        using (var cts = new CancellationTokenSource())
        {
            var waiter = cache.GetOrFetchAsync(key, Fetcher, cts.Token);
            await cts.CancelAsync();
            await Assert.ThrowsAnyAsync<OperationCanceledException>(() => waiter);
            // The fetch settles with every waiter gone.
            release.TrySetResult(true);
        }

        await Task.Delay(50);
        // Evict the cached endpoint so the next lookup is a cache miss; a
        // lingering inflight entry would serve "first" without refetching.
        cache.Put(other, Ep("other"));

        var result = await cache.GetOrFetchAsync(
            key,
            () => { Interlocked.Increment(ref fetchCount); return Task.FromResult(Ep("second")); });
        Assert.Equal(2, fetchCount);
        Assert.Equal("second", result.EndpointAddress);
    }

    [Fact]
    public async Task GetOrFetchAsync_StaleSettledRemoval_DoesNotEvictNewerFetch()
    {
        // Invalidate() during a fetch installs a new generation; the stale
        // removal must only delete its own entry.
        var cache = new EndpointCache(maxSize: 10, ttlSeconds: 60);
        var key = new EndpointCacheKey("sb-1", 8080, false);
        var firstRelease = new TaskCompletionSource<bool>(TaskCreationOptions.RunContinuationsAsynchronously);
        var fetchCount = 0;

        Task<Endpoint> Fetcher()
        {
            var n = Interlocked.Increment(ref fetchCount);
            return n == 1
                ? firstRelease.Task.ContinueWith(_ => Ep("stale"))
                : Task.FromResult(Ep("fresh"));
        }

        var stale = cache.GetOrFetchAsync(key, Fetcher);
        cache.Invalidate("sb-1");
        var fresh = cache.GetOrFetchAsync(key, Fetcher);

        firstRelease.TrySetResult(true);
        await stale;

        // Served from the fresh fetch's cache entry; a third fetch here
        // would mean the stale removal evicted it.
        var third = await cache.GetOrFetchAsync(key, Fetcher);
        Assert.Equal(2, fetchCount);
        Assert.Equal("fresh", third.EndpointAddress);
    }
}
