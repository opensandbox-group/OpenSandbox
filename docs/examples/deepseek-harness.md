---
title: DeepSeek Harness
description: Install and run a remote-only DeepSeek Harness consumer with explicit sandbox and model lifecycle.
---

# DeepSeek Harness embedded example

::: tip Choose the right entry point
This page is for application developers embedding a private Agent in Node.
If you use the native DSH Web application, follow the
[native plugin installation guide](/guides/deepseek-harness) instead.
The native bundle preserves DSH's existing session and model; this example
creates a separate private headless session.
:::

This example composes the actual DeepSeek Harness Agent with OpenSandbox-bound
bash/read/write/edit providers. Each session owns a private Cordis Context and an
explicit sandbox ID and remote cwd. Bash stays foreground-only; jobs, host
subprocess/filesystem providers, policy escalation, persistence, subagents, and
image tools are not mounted.

Source: [integration](https://github.com/opensandbox-group/OpenSandbox/tree/main/integrations/deepseek-harness)
and [examples](https://github.com/opensandbox-group/OpenSandbox/tree/main/examples/deepseek-harness).

## Pinned prerequisites

- Node `^22.19.0 || >=24.0.0`; pnpm `11.7.0`
- OpenSandbox JavaScript SDK `1.1.1`
- DeepSeek Harness packages `0.2.1-alpha.1`; Cordis `4.0.5-alpha.1`
- Live sandbox: Linux, bash, Python 3.8+ standard library, and an existing writable
  absolute cwd. The integration checks these prerequisites and does not install them.

The integration is not published yet. From a checkout, install and build it first:

```sh
cd integrations/deepseek-harness
pnpm install --frozen-lockfile
pnpm run build
pnpm run pack:check
cd ../../examples/deepseek-harness
pnpm install --frozen-lockfile
pnpm run typecheck
pnpm test
pnpm smoke
```

The example dependency is a local package install, using its compiled exports,
without TypeScript source aliases. `pack:check` separately builds an actual
npm tarball, checks its file allowlist and byte-identical Python helper, installs
it in an isolated temporary consumer, imports every public ESM subpath, and
compiles a consumer against the installed declarations. Consumer dependencies
come from the official npm registry; nothing is published.

## Pull request CI

The `DeepSeek Harness Tests` workflow runs frozen-lockfile installs, typechecks,
build, integration and Python helper tests, strict tarball checks, and consumer
tests/smoke on hosted Linux with Node 24, Python 3.12, and pnpm 11.7.0. Its path
scope covers both package trees, the workflow/detector, and shared contract/test
inputs. The `DSH CI` summary fails if relevant tests or change detection fail,
and accepts skipped tests only for unrelated changes.
The full strict tarball consumer runs once through `pnpm test` in
`tests/package.test.ts`; `pnpm run pack:check` remains a standalone equivalent.
Thus the full integration test suite also requires npm registry access.

These checks use the pinned published SDK and require access to the public npm
registry. They do not provision a sandbox/cluster or enable paid model calls;
real deployment validation remains a separate explicit opt-in.

## Offline smoke: contract-only

`pnpm smoke` uses the actual installed Agent loop and a scripted LlmAdapter with
an explicitly selected local contract transport. It executes the bundled Python
helper and a fixed scripted bash/read/edit sequence in disposable local namespace
directories, then round-trips bytes containing NUL and invalid UTF-8 via
SdkTransport. This transport is neither a security sandbox nor a networked
OpenSandbox deployment. It accepts only its expected smoke commands.

It has no model/server credentials and makes no network requests. Successful
output says `contract-only`; it proves composition and byte-transfer contracts,
not deployed connectivity or real-model behavior. The real entry never imports
this transport, and connection/model failures never trigger a smoke fallback.

## Opt-in real headless session

`pnpm headless` refuses to start unless `DSH_REAL_MODEL=1`. Configure all required
values in your own environment before opting in. The prompt and resulting remote
tool outputs can be sent to the selected DeepSeek endpoint and incur model charges.
Review both the prompt and sandbox contents before running it.

| Variable | Meaning |
| --- | --- |
| `DSH_REAL_MODEL` | Must be exactly `1` to permit real service/model calls |
| `OPEN_SANDBOX_DOMAIN` | Explicit OpenSandbox SDK domain, such as `localhost:8080` |
| `OPEN_SANDBOX_PROTOCOL` | Explicit `http` or `https` |
| `OPEN_SANDBOX_API_KEY` | Optional server API key; credentials never enter the descriptor |
| `DSH_SESSION_ID` | Explicit unique session ID; not an ambient routing hint |
| `DSH_REMOTE_CWD` | Existing writable absolute Linux cwd in the sandbox |
| `DSH_SANDBOX_MODE` | `create-image`, `create-template`, or `connect` |
| `DSH_IMAGE` | Required for `create-image`; choose a Linux image with Python 3.8+ and bash |
| `DSH_TEMPLATE_ID` | Required for `create-template`; SDK uses `createFromTemplate` |
| `DSH_SANDBOX_ID` | Required for `connect`; must name a live sandbox |
| `DSH_TTL_SECONDS` | Required positive integer for either create mode |
| `DSH_CLEANUP` | Required `close` or `kill`; `kill` explicitly deletes the bound sandbox |
| `DEEPSEEK_API_KEY` | Required DeepSeek API key, read only from environment |
| `DEEPSEEK_BASE_URL` | Explicit HTTPS Messages API root, usually `https://api.deepseek.com/anthropic` |
| `DSH_MODEL` | Explicit wire model name accepted by your endpoint; no default model |
| `DSH_USER_ID` | Explicit stable anonymous UUID used by the official adapter |
| `DSH_PROMPT` | Explicit prompt; shell/file writes are allowed inside your chosen sandbox |

The real path uses the published official `DeepSeekAdapter`, not an invented model
transport. It is text-only, caps output at 4096 tokens, sets adapter `maxRetries` to zero, and mounts no model-retry plugin.
Use only an endpoint you trust to receive
your API key and sandbox context. Do not place secrets in prompts, remote files,
or shell output.

`session.run()` awaits whole-Agent quiescence. Resolution alone does not mean the
turn succeeded. Both entries inspect the current turn's terminal reason and all
committed tool results plus `agent/error` events; failure gives a nonzero exit
status. Real output prints only bounded structural outcome facts, without raw
errors, model output, prompts, or credentials.

## Lifecycle and reconnect

`create-image` invokes SDK `create`; `create-template` invokes SDK
`createFromTemplate`; `connect` reuses a live sandbox using fresh connection
configuration. SDK readiness and application prerequisites precede Agent creation.
A failed requested connection has no creation or local fallback.

The optional `preflightTimeoutSeconds` defaults to 10 seconds and must be positive
and finite. Its local deadline is rounded up to milliseconds and must not exceed
2,147,483,647 ms (2,147,483.647 seconds), the Node timer limit. Larger values are
rejected before any SDK create or connect call; they are not silently capped.

At shutdown the example first disposes the Agent/Context, then performs the
explicit `DSH_CLEANUP` operation. `close` marks the binding closed and releases its
SDK-owned transport, leaving borrowed transport to its owner and the remote sandbox
alive; reconnect later with `connect`, the same sandbox ID,
and fresh SDK configuration. It does not restore the prior in-memory Agent log.
`kill` deletes the bound sandbox while its handle is open, then closes local
resources. For attached sandboxes, choose `kill` only when you intend to delete
that sandbox and all its data.

`SessionStore` holds the Agent's session log in client memory. This composition
mounts no session-persistence plugin, so it writes no session log to either the
client filesystem or the sandbox. `ShellEnv.dshHome` sets the remote shell's
`DSH_HOME` environment value to the remote cwd; it does not relocate the session
store or enable persistence.

::: warning External idle pause/wake
Automatic idle pause/wake is backend lifecycle policy, not part of this plugin's
supported reconnect guarantee. On deployments that support it, the caller or
operator must configure it separately through the public lifecycle API, for
example `idleTimeoutSeconds: 300`, `onIdle: pause`, and `wakeOnAccess: true`.
The pinned SDK 1.1.1 creation options do not expose this policy. Neither local
`close`, sandbox TTL, nor the DSH session configuration enables it.

Restoring the same sandbox does not guarantee that its first tool request
succeeds. In a [maintainer-reported validation](https://github.com/opensandbox-group/OpenSandbox/pull/2163#discussion_r4366851332),
remote files and in-memory session events survived while the DSH host process
stayed alive, but the first read returned `FS_IO_ERROR` with an unknown outcome;
a later read succeeded. The reported 6.7766-second wake exceeded the deployed
5-second gateway parking budget, but no raw HTTP status was captured, so budget
exhaustion is a plausible explanation, not a confirmed diagnosis. This does not
establish transparent wake, safe command replay, or session recovery after a
DSH host-process restart.

Any future wake acceptance test must declare the gateway parking and client
request/operation budgets, cover both within-budget and over-budget wake, and
record sanitized HTTP/tool outcomes plus independent remote-state evidence.
The plugin does not automatically replay failed tool operations whose outcomes
are unknown, including reads that stage helper requests and launch remote commands.
Tests must show how the caller surfaces and reconciles those failures. A later
successful request neither establishes the first request's outcome nor erases
the original failed tool result.
:::

Shell stdout and stderr have independent capture budgets. `stdoutMaxBytes`
overrides stdout only; stderr retains the executor's `maxOutputBytes` limit
(64,000 bytes by default), as specified by the pinned DSH contract. Truncated
results and lossy incremental reads report that output was omitted.

The pinned remote execution API reports numeric exit outcomes without a
terminating signal name. The provider preserves those numbers and reports
`signal: null` (not reported); it does not infer a signal from `-1` or `128 + N`.
Use `aborted` and `timedOut` for the provider's first stop cause, and command-status
evidence for confirmed termination. These fields do not establish which POSIX
signal terminated a process.

Await `session.dispose()` and handle its rejection before choosing binding
cleanup. The headless entry explicitly joins providers, continues remaining
cleanup after a failure, and reports incomplete disposal with an `AggregateError`.
For direct Cordis plugin consumers, explicitly await both `RemoteShell.dispose()`
and `RemoteFileSystem.dispose()`:
Cordis logs effect-disposer errors during Context teardown, so successful
`fiber.dispose()` alone does not prove that remote commands stopped.
Filesystem disposal retains ownership of issued helper commands even after their
requests settle. If termination remains unconfirmed, disposal rejects with a
sanitized `FS_IO_ERROR` and unknown remote state rather than reporting successful
cleanup. A helper may still publish after that failure; preserve the failure,
continue the caller's chosen binding cleanup, and inspect remote state. Do not
automatically replay the failed request.

Connection ownership follows SDK 1.1.1. Reusing plain options or a fresh,
uninitialized `ConnectionConfig` gives each binding an independent transport;
the SDK clones an uninitialized instance before initializing its transport.
Reusing a transport-initialized `ConnectionConfig` shares its dispatcher, but the
borrowing binding does not own it by default. Closing a borrower, including
cleanup after a failed opening or attachment, leaves that dispatcher open. The
caller must call `connectionConfig.closeTransport()` after all borrowers finish
when it initialized that transport itself.
For `kind: 'connect'`, the inherited SDK option `ownsTransport: true` explicitly
makes that binding responsible for closing the transport, so its close affects
every sharer. If the configuration came from a sandbox that owns the transport,
closing that original owner can also invalidate borrowers. In either
case, a borrower can still report `state: 'open'` while its SDK operations fail.
Use plain options (as the examples do) or independent configurations per binding,
and fresh configuration after the owner closes a transport. The integration does
not change or freeze caller configuration fields or override SDK ownership.

Both creation modes reserve the metadata key `dsh-binding-request` for a fresh
request UUID. The integration replaces any caller value for that key without
mutating the caller's metadata; all other entries reach the SDK unchanged. The
key and UUID satisfy fast-sandbox's DNS-label key and Kubernetes-label value
rules. Caller metadata must still meet the selected backend's requirements.
Use this key with the UUID in `UnknownOutcomeError.correlationId` when inspecting
service metadata; do not use the earlier dotted key for new creation requests.

Creation failure with no returned ID retains `UnknownOutcomeError`. Its optional
`classification`, `statusCode`, and allowlisted `sdkCode` preserve safe SDK facts:
for example, HTTP 429 / `FSB::API_ERROR` reports `classification: 'api'`, while a
typed SDK `INVALID_ARGUMENT` reports `classification: 'invalid-argument'`.
No raw message, body, headers, request ID, URL, or cause is retained. Unrecognized
failures remain `transport`; the integration does not guess from error text.
Known SDK option checks (blank template ID, nonfinite image TTL, and invalid volume
backend/host path) fail locally with `BindingError.code: 'invalid_options'` before
opening. Image TTL `null` remains supported.

These diagnostics do not prove that creation had no side effects: SDK 1.1.1 also
resolves endpoints after a successful POST and may throw an API error at that
stage, including after unsuccessful SDK cleanup. Inspect service evidence before
retrying; neither an HTTP status nor a validation classification permits automatic
retry. Known created IDs failing readiness/preflight use the integration's
owned cleanup policy; connected sandboxes are never implicitly deleted by opening
failure. Unknown mutations/launch/deletion outcomes are not automatically retried.
A failed kill is reported as unconfirmed deletion. Retain the printed descriptor
and inspect remote state using a new connection; do not claim total rollback.

A shell output-observation failure settles the local result without proving remote
termination. If the launched command has not been confirmed stopped, the caller
signal or handle `kill()` still accepts the first explicit stop and attempts one
interrupt when its ID is known, with bounded independent status observation.
The original failed result remains failed; the command is never relaunched or automatically retried.

Filesystem atomicity and observed-version guards cover cooperating helper writers,
not arbitrary shell processes. Their persistent coordination locks live in the
sandbox's private `/tmp/.opensandbox-dsh-locks-v2-<parent-hash>` namespace, keyed
by the physical target parent, not beside edited files. Normal repository edits
therefore do not stage helper lock artifacts. Helper processes must share that
sandbox's `/tmp` namespace; a different UID cannot use the owner-private lock
directory. This is not a distributed lock for shared volumes mounted in separate
sandboxes: concurrent cross-sandbox writers are outside the guarded-edit contract.
Locks deliberately survive provider/session disposal: unlinking an inode while
another process holds or awaits it would break cross-process coordination.
Stop and confirm termination of all older helpers before switching from the
previous per-parent lock layout; do not mix layouts or remove active lock files.
Existing old-layout artifacts are not automatically deleted.

Reads/writes and output have finite limits; invalid
UTF-8 is handled as bytes, not silently decoded as text. Helper bootstrap and
request-directory creation require a zero exit and exactly one safe path record
in one stdout event. Execd emits nonempty line events without CR/LF; a transport
that includes one terminal LF is also accepted. Extra events, embedded line
delimiters, whitespace, unsafe names, and unexpected parents are rejected with
an unknown outcome and retained artifacts rather than retried.
Linux helper prerequisites
and package/local contract checks do not establish a real Ubuntu deployment.
For the separate real, deterministic SDK/Agent validation and user-operated deployment,
see [Ubuntu validation](/examples/deepseek-harness-ubuntu). It requires an explicit
`DSH_LIVE_E2E=1` opt-in and emits credential-free stage and deletion evidence.
