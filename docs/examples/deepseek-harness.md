---
title: DeepSeek Harness
description: Install and run a remote-only DeepSeek Harness consumer with explicit sandbox and model lifecycle.
---

# DeepSeek Harness

This example composes the actual DeepSeek Harness Agent with OpenSandbox-bound
bash/read/write/edit providers. Each session owns a private Cordis Context and an
explicit sandbox ID and remote cwd. Bash stays foreground-only; jobs, host
subprocess/filesystem providers, policy escalation, persistence, subagents, and
image tools are not mounted.

Source: [integration](https://github.com/opensandbox-group/OpenSandbox/tree/main/integrations/deepseek-harness)
and [examples](https://github.com/opensandbox-group/OpenSandbox/tree/main/examples/deepseek-harness).

## Pinned prerequisites

- Node `^22.19.0 || >=24.0.0`; pnpm `11.7.0`
- OpenSandbox JavaScript SDK `1.1.0`
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
explicit `DSH_CLEANUP` operation. `close` releases the local SDK handle and leaves
the remote sandbox alive; reconnect later with `connect`, the same sandbox ID,
and fresh SDK configuration. It does not restore the prior in-memory Agent log.
`kill` deletes the bound sandbox while its handle is open, then closes local
resources. For attached sandboxes, choose `kill` only when you intend to delete
that sandbox and all its data.

Connection ownership follows SDK 1.1.0. Reusing plain options or a fresh,
uninitialized `ConnectionConfig` gives each binding an independent transport;
the SDK clones an uninitialized instance before initializing its transport.
Reusing a transport-initialized `ConnectionConfig`, including another sandbox's
`connectionConfig`, shares its dispatcher. Closing either binding closes that
dispatcher for every sharer, including cleanup after a failed opening or attachment.
Another binding can still report `state: 'open'` while its SDK operations fail.
Use plain options (as the examples do) or independently initialized configurations
per binding, and fresh configuration after close. The integration does not change
or freeze caller configuration fields or override this SDK sharing behavior.

Both creation modes reserve the metadata key `dsh-binding-request` for a fresh
request UUID. The integration replaces any caller value for that key without
mutating the caller's metadata; all other entries reach the SDK unchanged. The
key and UUID satisfy fast-sandbox's DNS-label key and Kubernetes-label value
rules. Caller metadata must still meet the selected backend's requirements.
Use this key with the UUID in `UnknownOutcomeError.correlationId` when inspecting
service metadata; do not use the earlier dotted key for new creation requests.

Creation failure with no returned ID has unknown outcome; inspect service evidence
before retrying. Known created IDs failing readiness/preflight use the integration's
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
not arbitrary shell processes. Reads/writes and output have finite limits; invalid
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
