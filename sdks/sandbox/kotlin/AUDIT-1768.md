# Kotlin SDK Audit — issue #1768 (Phase 0 + cleanup pass)

Scope: `sdks/sandbox/kotlin/**` (sandbox, code-interpreter, sandbox-pool-redis).
This pass intentionally changed **no runtime logic**; it only landed
documentation/comment/dead-code cleanups. Logic-level findings are listed below
for triage in follow-up per-issue PRs.

## 1. Completed in this pass (no behavior change)

### Documentation / KDoc
- `Sandbox.getEndpoint` / `Sandbox.getMetrics` KDoc were copy-pastes of
  `getInfo`; rewritten to describe their actual behavior.
- `SandboxUnhealthyException` KDoc was a copy of `SandboxReadyTimeoutException`;
  corrected.
- `SandboxState`: added `RESUMING` constant and fixed the transition doc
  (`Paused → Resuming → Running`) to match the lifecycle spec. Additive only.
- `SandboxFilter.page` / `PaginationInfo.page` documented as 0-indexed; spec and
  builder validation are 1-indexed. Fixed. `states` example now points at
  `SandboxState` constants (values are `Running`, not `RUNNING`).
- `SetPermissionEntry` KDoc claimed "only specified properties will be changed"
  while `mode` is always sent (API-required); doc now states this explicitly.
- `EntryInfo` KDoc: added missing `@property type`.
- `ExecutionComplete.executionTimeInMillis`: "mills" → milliseconds.
- `LifecycleHook` / `PeriodicLifecycleHook`: documented server-side defaults
  (60s) and accepted ranges (1..10800 / 1..300).
- `IsolatedBackgroundRun` / `IsolatedRunStatus`: documented that `startedAt` is
  spec-required but parsed as nullable for older execd builds.
- `PoolCreationSpec`: removed stale "fixed 24h timeout" comment (timeout is
  `PoolConfig.idleTimeout`, default 24h).
- `InMemoryPoolStateStore`: KDoc claimed "no external lock" while several
  operations are `synchronized`; doc now matches implementation.
- `SandboxPool.warmupConnectionConfig`: added note that both branches produce
  the same value when a shared pool is set (kept code untouched).
- `RetryInterceptor`: comment claimed OkHttp `with*Timeout` can only lower a
  timeout; it actually *replaces* (can raise). Comment corrected.
- `HttpClientProvider`: removed leftover numbered ("1./2./3.", "Now we can…")
  revision comments.
- `ConnectionConfig.getBaseUrl`: comment now distinguishes Python-compatible
  behavior from the extra normalization Kotlin applies.
- Code-interpreter KDoc: class example called nonexistent `interpreter.kill()`
  → `interpreter.sandbox().kill()`; builder example called nonexistent
  `.connectionConfig(...)` → removed; `CodeContext`/`RunCodeRequest` docs
  referenced the removed `cwd` field → removed; `context` doc claimed "if null,
  a temporary context is created" (it is non-null with a default Python context)
  → rewritten; language list claim ("Java, Kotlin") aligned with
  `SupportedLanguage` constants.
- `sandbox-pool-redis/README.md`: "the pool does not silently bypass shared
  state" contradicted the documented DIRECT_CREATE fallthrough
  (SandboxPool.kt, OSEP-0005); wording fixed.

### Dead code / duplication
- Removed unused private `EgressAdapter.optionalIntArray`.
- Removed `@author/@since` personal tag from `FilesystemConverter`.
- Removed duplicate instance loggers shadowing identical companion loggers in
  `Sandbox` and `CodeInterpreter`.
- `CodeExecutionConverter`: removed redundant `?.` on a non-null field.

## 2. Logic-level findings (NOT changed — need decision / follow-up PRs)

### High
1. **code-interpreter `run()` does not strip SSE `data:` frame prefixes** —
   `CodesAdapter.kt:148-159` parses each line as raw JSON. The sandbox SDK's
   `CommandsAdapter.decodeEventLine` and the Python/JS code-interpreter SDKs
   all handle standard SSE framing. If a response arrives framed as
   `data: {...}`, all events are silently dropped (empty `Execution`, no
   error). Fix: reuse the `decodeEventLine` logic.

### Medium
2. **`IsolatedFilesystemAdapter.replaceContentsDetailed` swallows HTTP errors**
   — `IsolatedFilesystemAdapter.kt:314-332`: the `WithHttpInfo` variant returns
   `ClientError`/`ServerError` wrappers instead of throwing, so 404/5xx map to
   `emptyList()`. The sibling `replaceContents` correctly throws.
3. **`callTimeout` caps the whole retry budget** — `HttpClientProvider.kt`
   `applyStandardTimeouts()` sets OkHttp `callTimeout = requestTimeout` (30s
   default). `RetryInterceptor` (application interceptor) runs inside that
   budget, so retries + backoff are truncated to a single request timeout; in
   Python `request_timeout` is per-attempt and only `overall_deadline` bounds
   the call. Fix: `callTimeout(0)` when the retry interceptor is installed.
4. **Per-attempt timeout clamp can raise the client baseline** —
   `RetryInterceptor.kt` uses `chain.with*Timeout(ms)` directly; OkHttp replaces
   (not min-merges) the value, so `perAttemptTimeout > clientTimeout` raises
   the effective timeout. Python clamps with `min()` only. Fix: take
   `minOf(ms, chain.*TimeoutMillis())`. (Comment corrected this pass.)
5. **Isolated session URLs interpolate raw ids** — `IsolatedSessionsAdapter`
   builds `"$execdBaseUrl/.../session/$sessionId"` in 10 places; an id
   containing `/ ? #` re-routes the request. `CommandsAdapter.runInSession`
   correctly uses `addPathSegment`. Fix: build URLs via `addPathSegment`.
6. **code-interpreter run request always serializes `"id": null`** —
   `CodesAdapter` JSON config lacks `explicitNulls = false` (the sandbox
   `CommandsAdapter` has it); Python/JS omit the field, spec types `id` as
   `string`. Fix: align JSON config.
7. **Blank code throws stdlib `IllegalArgumentException`** —
   `CodeModels.kt` builder `require(...)`; Python/JS throw SDK
   `InvalidArgumentException`, so callers cannot `catch (SandboxException)`.
8. **Pool orphan warmup task race** — `SandboxPool.scheduleAt` vs
   `retireRun`/`cancelDelayedWarmups`: a task offered to `delayQueue` after the
   run retired is never consumed; `warmingCount`/`inFlightOperations` leak and
   an already-created sandbox is never killed. Fix: re-check run liveness after
   `offer` (or drain queue under the run fence write lock).
9. **Redis module has zero default CI coverage** — all tests gated on
   `OPENSANDBOX_TEST_REDIS_URL`; Lua scripts/lock semantics unverified without
   a Redis. Fix: embedded-redis/testcontainers default.

### Low (selection)
10. `RetryInterceptor.kt:~100` — `perAttemptMs.toInt()` Long→Int overflow for
    >24.8-day deadlines (all attempts become 1ms timeouts).
11. `computePerAttemptTimeoutMs` returns `null` for sub-ms limits, discarding
    the clamp (attempt falls back to 30s client default).
12. `EndpointCache.getOrFetch` — `finally` removes `inflight[key]`
    unconditionally (identity check missing → concurrent invalidate + refetch
    can double-fetch); leader/follower share one un-cloned mutable
    `SandboxEndpoint` (`get()` clones).
13. `ExecutionEventDispatcher` — `eventNode.error!!` NPEs on a malformed error
    event (event dropped, exit code degrades); `executionCount` unconditionally
    overwritten (a null clears a previously seen count). Python guards both.
14. Exit-code inference drift: `CommandsAdapter` doesn't `trim()` error value,
    `IsolatedSessionsAdapter` does (`"1\n"` → null vs 1).
15. `runInSession` posts `"cwd":null,"timeout":null` (`run` uses
    `explicitNulls=false`).
16. `ReadinessBudget.run()` checks `remaining()` before recording `lastError`,
    so a deadline-exceeded timeout can lose the cause/message (Python records
    first).
17. `renew()` uses local-zone `OffsetDateTime.now()` (spec requires UTC) and
    computes `now()` twice (log vs payload differ). Server currently
    normalizes, so cosmetic + robustness.
18. `LifecycleMetricsReporter` bypasses `ClientIpInterceptor` → metrics
    requests lack `OPEN-SANDBOX-CLIENT-IP` (Python includes it).
19. `ConnectionConfig.protocol()` accepts any string (Python validates
    http/https).
20. `Connector.connect()`/`Resumer.resume()` reject only `null` sandboxId,
    blank string proceeds to RPC (Python rejects blank).
21. `SandboxPool` idle path calls `ensurePoolNamespaceActiveOrDispose(sandbox)`
    without the acquire policy — under a store outage + DIRECT_CREATE the
    healthy just-acquired sandbox is killed, while the direct-create path keeps
    its sandbox. Same acquire, inconsistent semantics.
22. `ThrowableUtils` cause-chain walk lacks a depth bound (two-node cycle loops
    forever).
23. `toCommandTimeoutMillis` catch of `ArithmeticException` is dead code —
    `toMillis()` silently overflows instead of throwing.
24. `ClientIpDetector.isPrivateIpv4` misses CGNAT 100.64/10 etc. that Python's
    `ipaddress.is_private` covers.
25. Redis `removeIdle` (HDEL then LREM, two round trips) is not atomic vs a
    concurrent `putIdle`.
26. Pool reconcile ignores `renewPrimaryLock` failure for up to one heartbeat.
27. `Retry-After` HTTP-date parsing is strict `RFC_1123_DATE_TIME` (Python's
    `email.utils` accepts more); old-style dates fall back to computed backoff.
28. TLS-handshake `SocketTimeoutException("SSL handshake timed out")` lacks
    "connect" in the message → classified post-send (idempotent-only retry);
    Python classifies handshake as pre-send.
29. `WRITE_TIMEOUT` / `UNEXPECTED_EOF` `RetryCause` values are never produced
    (dead enum values, kept for API compat).
30. `SandboxCreateResponse` drops spec-required `status`/`createdAt`/
    `entrypoint` (public model — adding fields needs a compatibility decision).
31. `SearchEntry.pattern` is forced non-blank; spec allows omitting pattern
    (defaults `**`).
32. `EnvPassthroughSpec` defaults `mode="deny"` and the adapter fabricates
    `"deny"` when the echo omits it.
33. `RunCodeRequest`/`CodeContext` builders lack `language` blank validation
    (Python pydantic validates).
34. `DiagnosticsAdapter` wraps a client + explicit Accept interceptor that the
    generated `ApiClient` already applies.
35. Filesystem upload request bodies wrap one-shot `InputStream`s without
    `isOneShot()`; a transport-level replay would hit an exhausted stream.

## 3. Test coverage gaps (not changed)
- code-interpreter: no tests for `getContext`/`listContexts`/`deleteContext`,
  `ping`, SSE `data:` framing, run request body assertions.
- `SandboxPoolManagerTest` (2 cases): drain timeout → `PoolDestroyIncompleteException`
  + DESTROYING fence, kill-failure counting, already-destroyed early return.
- `InMemoryPoolStateStore`: tombstone TTL expiry → ACTIVE fallback;
  `beginDestroy` on DESTROYED.
- Idle-path renew-failure dispose; reconciler `reapExpiredIdle` kill loop.

## 4. Visibility hygiene (not changed — decision: leave as-is)
These public members are module-internal implementation details (undocumented,
same-module usage only) and could be `internal` in a future major version.
Tightening them on 1.0.x is binary-breaking for downstream, so they are kept:
- code-interpreter: `AdapterFactory`, `CodesAdapter` (only used by the internal
  `CodeInterpreter.create`)
- sandbox: `ExecutionConverter`, `FilesystemConverter`, `SandboxModelConverter`,
  `DiagnosticModelConverter`, `EndpointCache`
- sandbox: `AbstractUnknownPropertiesSerializer` (dead code, zero references)

Must stay public (verified): `HttpClientProvider`, `ExecutionEventDispatcher`,
`toSandboxException`/`toSandboxApiException` (used cross-module by
code-interpreter), `InMemoryPoolStateStore` (documented in README), the
transport config surface (`RetryPolicy`, `RetryInterceptor`, `RetryEvent`,
`StatusCode`), domain models and service interfaces.

## 5. Deduplication / refactoring opportunities (not changed)
- `FilesystemAdapter` vs `IsolatedFilesystemAdapter`: ~280 lines duplicated
  (~90%); extract shared transport/API facade.
- Endpoint-header interceptor + `"${protocol}://${endpoint.endpoint}"` builder
  repeated across 6+ adapters → `HttpClientProvider.endpointClient(...)` /
  `SandboxEndpoint.baseUrl`.
- SSE read loop + `decodeEventLine` + exit-code inference duplicated between
  `CommandsAdapter` and `IsolatedSessionsAdapter`.
- `ConnectionConfig.copyWithoutConnectionPool/copyWithConnectionPool/
  copyForSingleAttempt/copyForStagedWarmup`: four ~110-line field copies.
- `SandboxPool`: `releaseAllIdle()` x2; four near-identical reconcile teardown
  sequences.
- Build: root `build.gradle.kts` has an unused `buildscript` Jackson classpath;
  junit jupiter 5.10.1 vs junit-platform-launcher 1.13.4 version lines mixed;
  `sandbox-bom` does not constrain `jedis`; duplicate POM exclusion logic
  (GenerateMavenPom regex + vanniktech withXml); module-level `javaParameters`
  duplicating the root setting.

## 6. Verified as NOT issues (audit false positives)
- `listSandboxes` metadata filter encoding: Kotlin matches Python exactly —
  both pass the raw `k=v&k2=v2` string and let the HTTP client encode it once
  (Python `sandboxes_adapter.py` even comments "similar to Kotlin SDK").
- Retry defaults (codes {429,502,503}, idempotent-only, decorrelated jitter,
  Retry-After cap), SSE client exclusion, execd/egress wire field names,
  millisecond/second units, resource cleanup in adapters: verified aligned
  with specs and Python.

## 7. Cross-language gaps registered elsewhere (out of Kotlin scope)
- JavaScript SDK has no HTTP-transport retry at all (Python/Kotlin do).
- Kotlin typed exception subclasses (507, SANDBOX_NOT_FOUND,
  BACKEND_CONNECTION_FAILED) and different exception hierarchy for
  timeout/connection errors vs Python — needs a cross-language decision.
