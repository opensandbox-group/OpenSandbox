---
title: Native DeepSeek Harness plugin
description: Install the OpenSandbox preset in a dedicated native DeepSeek Harness Web profile.
---

# Native DeepSeek Harness plugin

Use this guide if you use **DeepSeek Harness itself** and want its existing
Agent and configured model to execute shell and file tools in OpenSandbox.
Install `@opensandbox/deepseek-harness-native` into a dedicated native Web
profile, then select its `opensandbox` preset. You do not write a Node
application or start a second/headless Agent.

For application developers embedding an Agent, the separate
[headless integration example](/examples/deepseek-harness) remains available.
The [Ubuntu deployment recipe](/examples/deepseek-harness-ubuntu) validates that
embedded integration, not this native plugin's installation or Web workflow.

## Supported profile and prerequisites

- Official npm CLI `@deepseek-ai/dsh@0.2.1-alpha.1`, Node
  `^22.19.0 || >=24.0.0`, and pnpm `11.7.0`.
- A **new, dedicated Web profile** with the supplied persistent safety patch.
  Existing stock profiles, Desktop, ACP, custom presets, hot migration of running
  Agents, and third-party same-process plugins are not covered.
- A reachable OpenSandbox API, via its normal public endpoint/Ingress. SDK
  `1.1.1` is included. This guide does not use direct FastPath endpoints.
- Either a provisioned template ID or an explicit image. The sandbox must have
  Linux, bash, Python 3.8+ and an existing writable absolute working directory.
- The deployed execd must accept the `argv` form of `POST /command`. SDK `1.1.1` alone does not establish that server capability. A command-only execd rejects prerequisite and helper calls with HTTP 400; use a compatible test backend rather than changing the plugin to replay them through a shell-string fallback.
- Your normal native DSH model/provider configuration. This plugin does not
  install a model adapter, change the selected model, or configure model keys.

The native artifact consumes the host's DeepSeek/Cordis interfaces. The verified
npm host uses DSH `0.2.1-alpha.1`, Cordis `4.0.5`, and Schemastery `3.18.5`.
The embedded package retains its separate exact-alpha dependency set; do not
install that package alongside this bundle in the native profile. No dependency
force flags, consumer overrides, or version exemptions are needed.

::: warning Dedicated profile, not a host security boundary
The profile disables unverified local execution, terminals, stock presets,
subagents, jobs, PTC, skills, workspace previews, file uploads and Open In actions.
It retains native session/model configuration and history storage on the host.
The bundle supplies the browser's required `fileUpload` service, but that service
rejects uploads locally without reading or sending file bytes. The stock upload
and local attachment-storage plugins remain disabled. File attachments and image
paste/drop are unsupported; the host rejects non-text prompt attachments too.
Restored history containing an image cannot read that image through this profile;
there is no fallback to host attachment storage. Text history and remote tool
results remain available.
It is not a sandbox for arbitrary plugins or protection from deliberate edits
that re-enable host capabilities. Do not add stock or third-party presets.
Supported tools execute with the configured sandbox credentials/permissions;
this profile does not provide DSH host sandbox-policy or per-command approval UI.
The delete tool's `confirm_sandbox_id` is an ID-match guard, not an interactive
human-approval prompt. Review tasks before authorizing the Agent to run them.
:::

## 1. Build the unpublished bundle

Neither integration package is published yet. From the repository checkout:

```sh
cd integrations/deepseek-harness/native
pnpm install --frozen-lockfile
pnpm run build
pnpm pack --pack-destination /tmp
```

Use the absolute tarball path printed by `pnpm pack` below. The resulting package
is already compiled; installing it into DSH does not require a consumer build.
The optional development/acceptance checks are listed separately at the end.

## 2. Create the dedicated profile, then install

Use the exact alpha CLI above, not an unpinned latest launcher. These commands use
a new profile named `opensandbox`; stop if that profile already contains your own
configuration. Do not overwrite an existing profile.

```sh
export DSH_HOME="${DSH_HOME:-$HOME/.dsh}"
(
set -eu
test ! -e "$DSH_HOME/profiles/opensandbox"
dsh --profile opensandbox --from-default-profile web --dump-config >/dev/null
cp examples/opensandbox-web.patch.yml "$DSH_HOME/profiles/opensandbox/cordis.patch.yml"
dsh plugin --profile opensandbox add /absolute/path/to/opensandbox-deepseek-harness-native-0.1.0.tgz
)
```

Run these commands from the native package directory. Initialization dumps the
configuration without starting the application. **Copy the safety patch before
first startup.** Its required-service gates live in the profile, independently
of the bundle: an absent, disabled or invalid plugin must prevent the required
native execution/session entry points from starting, not restore local tools.
Keep the patch when disabling or uninstalling the bundle.

When updating a profile created with an earlier version of this PR, stop DSH,
update the installed bundle, and update the safety rows from the new example
while preserving your explicit connection configuration. In particular retain
`file-upload.disabled: true` and add `attachment-local.disabled: true`. Do not
re-enable the stock upload plugin to repair a missing browser service: it also
enables host upload storage. The updated bundle provides the browser service.
Also copy the `workspace-controller` row, which places the native local workspace
metadata under the dedicated profile rather than depending on an OS Documents
directory. Preserve its `ctx.get('profileContext').dir` expression.

This version uses native CLI plugin installation and a profile configuration
file. It does not ship a custom plugin-page configuration form. The profile
disables file-watcher hot reload and the plugin-management service. Native model
settings remain available; live OpenSandbox configuration edits are unsupported.
For connection/lifecycle changes, stop DSH, edit the profile, then restart.

## 3. Configure the OpenSandbox connection

The bundle's defaults reference environment values instead of storing a literal
API key in YAML:

```sh
export OPENSANDBOX_DOMAIN='sandbox-api.example.com'
export OPENSANDBOX_TEMPLATE_ID='your-existing-template-id'
# Supply OPENSANDBOX_API_KEY through your normal secret environment mechanism.
```

`domain` is an authority (`host` or `host:port`), without a URL scheme, path,
query, or embedded credentials. The default protocol is `https`. To change the
protocol, use image creation, or change other fields, append the following
**complete** plugin config row to the profile's `cordis.patch.yml`, after the
existing safety rows. Native loader config replacement is not a deep merge.

```yaml
- id: opensandbox-native
  config:
    domain: !!js process.env.OPENSANDBOX_DOMAIN
    protocol: https
    apiKey: !!js process.env.OPENSANDBOX_API_KEY
    kind: create-template
    templateId: !!js process.env.OPENSANDBOX_TEMPLATE_ID
    remoteCwd: /tmp
    ttlSeconds: 600
    requestTimeoutSeconds: 15
    readyTimeoutSeconds: 30
    preflightTimeoutSeconds: 10
    settlementTimeoutMs: 3000
```

For image creation, set `kind: create-image`, replace `templateId` with
`image: your-linux-image:tag`, and retain the remaining fields. Do not specify
both an image and a template. Configuration is locally validated before native
readiness; validation does not create or connect a sandbox. The key is a secret
schema field, but do not share raw configuration dumps or diagnostics.

The timeout fields govern API requests, readiness, prerequisites and command
settlement, respectively. TTL bounds the remote sandbox lifetime; it is not an
idle-pause timer. Configuration changes require a restart and do not migrate
existing bindings silently.

The default TTL is 600 seconds, with no automatic renewal. Size it for the whole sandbox session, including preparation, gaps between turns, commands and artifact retrieval, with a margin. A local timeout does not prove expiry, and local status with `remoteExistence: unverified` does not prove the sandbox is healthy or still exists.

## 4. Start DSH and run a task

Start the dedicated profile:

```sh
dsh --profile opensandbox --no-open
```

Open the local Web URL printed by DSH. Configure/select your usual provider and
model through native settings if it is not already inherited from your DSH home
configuration. Do not copy another profile's entire patch or tool presets.
Create a new session with preset `opensandbox`; the profile selects it by default.
The profile automatically prepares a local metadata workspace under
`<profile>/workspace-metadata/deepseek-harness/default-workspace`, so a headless
Linux host does not need `xdg-user-dir` or a desktop Documents folder. This local
directory identifies native sessions/history; it is not the remote `remoteCwd`,
does not synchronize files, and does not enable host shell/file tools.

Installing/enabling the plugin, starting DSH, selecting a model and creating a
session do **not** create a sandbox. The first `bash`, `read`, `write`, or `edit`
operation starts creation and waits for readiness/prerequisites. Concurrent first
file/shell operations share one creation. A failed or uncertain creation is not
a reason to create a replacement automatically.

For a first, file-only task, send this prompt:

> Use write to create report.txt containing exactly "native sandbox proof 2163"
> followed by a newline. Use read to show it, then opensandbox_status to report
> the sandbox ID. Do not run bash first.

Expect the write/read results to contain that text and a known sandbox ID from
status. A file-first task is supported; no preliminary shell command is required.
Then send a second turn in the same session:

> Run bash with command "pwd; cat report.txt", then report opensandbox_status.

With the default configuration, expect `/tmp`, `native sandbox proof 2163`, and
the **same sandbox ID**. For a command-first check, create a separate new session
and ask for `bash` with `pwd; test ! -e report.txt` before any file tool. Expect
`/tmp`, exit code zero, and a **different sandbox ID**. A template/image must not
already contain this example file; otherwise choose a unique filename.
The four tools remain the native tool definitions, routed through the session's
remote providers. Missing bash working directories and relative file paths are
interpreted against `remoteCwd`, not the native workspace directory.

## 5. Retrieve results and continue the session

Use the Agent's `read` tool to display a text result and copy/save the returned
text. For large or binary artifacts use the OpenSandbox SDK's file-download API
with the sandbox ID reported by `opensandbox_status`; choose the destination
explicitly. For example, from the native package directory with the same
connection environment, save this known text artifact through the SDK without
creating or deleting a sandbox:

```sh
export DSH_SANDBOX_ID='the-id-from-opensandbox-status'
export DSH_DOWNLOAD_PATH='/absolute/new/path/report.txt'
node --input-type=module <<'JS'
import { Sandbox } from '@alibaba-group/opensandbox';
import { writeFile } from 'node:fs/promises';
let sandbox;
try {
  if (!process.env.DSH_SANDBOX_ID || !process.env.DSH_DOWNLOAD_PATH || !process.env.OPENSANDBOX_DOMAIN) throw new Error();
  sandbox = await Sandbox.connect({
    sandboxId: process.env.DSH_SANDBOX_ID,
    connectionConfig: {
      domain: process.env.OPENSANDBOX_DOMAIN, protocol: 'https',
      apiKey: process.env.OPENSANDBOX_API_KEY, requestTimeoutSeconds: 15,
      disableMetrics: true,
    },
  });
  const bytes = await sandbox.files.readBytes('/tmp/report.txt');
  await writeFile(process.env.DSH_DOWNLOAD_PATH, bytes, { flag: 'wx', mode: 0o600 });
  console.log('Artifact saved without overwriting an existing file.');
} catch {
  console.error('Artifact download failed; no automatic retry.');
  process.exitCode = 1;
} finally {
  try { await sandbox?.close(); } catch { console.error('Local connection close failed.'); process.exitCode = 1; }
}
JS
```

Adjust the protocol/path only to match your configuration; the destination parent
must already exist. This explicitly downloads bytes to the host. Verify the
content and sandbox ID independently through the normal OpenSandbox API/Ingress
or your operator's backend inspection. Status and native history alone are not
proof that a tool ran remotely. Native sidebar previews, uploads, local file links and Open In are
not remote artifact retrieval and are disabled in this profile.

Continue in the **same native session** and ask it to edit/read the existing
file. Subsequent turns use the same sandbox and files; finishing a turn does not
close or delete it. Another new session receives a separate binding, even when
its native workspace/cwd is identical. Stock subagent/delegation workflows are
not supported in this profile; programmatic session-isolation tests do not imply
support for those workflows.

The native Agent, selected model, conversation history and local workspace
identity remain native DSH state. Only the supported execution/file tools are
remote. Native host history surviving is not proof that a remote sandbox still
exists.

## 6. Inspect, reconnect and clean up explicitly

Three management tools are available to the same native Agent:

- `opensandbox_status`: local binding facts and known sandbox ID only. It never
  creates a sandbox or proves remote health/TTL survival.
- `opensandbox_reconnect` with `sandbox_id`: explicitly connect this session to
  that existing sandbox. Failure never falls back to creation or local tools.
- `opensandbox_delete` with `confirm_sandbox_id`: explicitly delete the known
  matching sandbox, including its files. It never creates an unopened sandbox.
  Confirm the ID and save needed artifacts before requesting deletion.

Deleting, expiring or losing a sandbox does not automatically provision another
one. Start a new native session for a new sandbox. A create request whose outcome
is unknown needs operator reconciliation using independent backend evidence;
do not blindly retry it. An uncertain delete likewise remains uncertain until
reconciled; a successful later request does not prove the earlier outcome.
While an unresolved deletion record is retained, reconnect and execution remain
blocked. An explicit matching delete retry is possible only while its usable
handle is still owned; after a remount loses that handle, reconcile cleanup
externally and use a new session rather than reviving the uncertain binding.

Closing a session or deleting its native conversation history does not delete
the remote sandbox. Save its ID and request explicit sandbox deletion before
removing the conversation if you no longer need the files. Provider teardown on
Agent disposal stops that Agent's operations; a known same-process binding can
remain available for an explicit native resume.

Stopping/unloading the plugin closes admission, drains its providers and closes
local SDK handles. It does not normally delete the remote sandbox: use the
explicit delete tool or the deployment's lifecycle API, rather than relying only
on TTL. Newly allocated sandboxes can be deleted by failed-readiness/preflight
cleanup before they become usable. Incomplete provider shutdown is a reported
failure, not evidence that remote commands stopped.

Same-process session reactivation may reuse a known retained binding. After a
DSH process restart, reopening native history requires explicit reconnect to a
known sandbox ID, or a new session. Even a restored session that never used tools
is refused automatic creation when no binding record is retained. Do not assume
transparent process-restart persistence, automatic replay, or sandbox recovery.

To disable/remove the plugin, stop the profile first, preserve the safety patch,
and use the native CLI to remove the bundle. The fenced profile then fails
closed until the plugin is configured again. Do not hot-switch an existing stock
session into this profile or remove the safety fence to make startup succeed.

### Diagnose failures without exposing credentials

Local invalid configuration is rejected before SDK calls. On an API/transport
failure, use the safe classification/status/code fields and creation correlation
token from the native status result. A `401`/`403` can guide checking the API key;
transport failures call for checking the authority, protocol and reachability.
Neither proves that a timed-out creation did not happen. Reconcile the creation
token with the operator before another action, and do not share raw SDK errors,
headers, configuration dumps or credentials.

Readiness/preflight failures report the known cleanup state when available.
A failed provider drain blocks more execution but still permits local status and
an explicit confirmed deletion of its known sandbox. Within the same host
process, a plugin remount with a changed endpoint/protocol or remote cwd refuses
retained bindings as a configuration mismatch. Restore the original
configuration or start a new session. These binding/tombstone records are
in-memory, not durable across a process restart: before explicit reconnect,
independently verify the destination, cwd, sandbox ID and any earlier uncertain
creation/deletion. Restarting does not establish an unknown operation's outcome.
Rotating a key for the same destination still requires explicit reconnect after
restart.

### External idle pause/wake

Idle pause/wake is an external backend policy, configured separately by its
operator. SDK 1.1.1 creation configuration does not enable it. A restored sandbox
can still fail its first request if gateway parking or client operation budgets
expire during wake. The plugin does not automatically replay commands or helper
requests with unknown outcomes. Declare both gateway and client budgets and
reconcile remote state before deciding on a new action. See the
[observed failure boundaries](/examples/deepseek-harness#lifecycle-and-reconnect);
the reported wake timing was a plausible explanation, not a captured HTTP diagnosis.

## Validation boundary

Repository CI checks frozen installs, shared backend/helper/consumer regressions,
native typechecks/build, exact native tarball contents, a clean native
profile/controller, and actual Chromium Web startup/text conversations with
offline sandbox/model fixtures. The browser test loads the installed client
plugin graph, creates a session through the UI, sends two text turns, and checks
upload rejection and unchanged remote binding identity. Those tests establish
installation and routing contracts, not deployed network isolation or real-model
quality. Live acceptance must separately use the actual native installed entry,
your configured model, and the existing OpenSandbox Ingress on ACK fast-sandbox.
Record the backend explicitly for any additional create-image validation; that evidence does not establish FastSandbox or Firecracker acceptance.
Record first-use creation, a file-first task, multiround reuse, cross-session
isolation, artifacts, explicit cleanup and failure/no-host-fallback evidence.
No live deployment or model result is implied by the offline checks.

### Maintainer/developer offline checks

From `integrations/deepseek-harness/native`:

```sh
pnpm install --frozen-lockfile
pnpm run typecheck
pnpm run build
pnpm test
pnpm run pack:check
pnpm run test:profile
pnpm exec playwright install --with-deps chromium
pnpm run test:browser
```

`pack:check` validates the artifact allowlist and unchanged shared source/Python
helper bytes. `test:profile` starts with an ordinary actual native CLI
installation and exercises the host entry with offline fixtures; it is not a
browser test. `test:browser` separately loads the real Web UI from an ordinary
installed tarball. Browser binaries are a development/CI prerequisite, not part
of the published bundle. The native bundle intentionally receives framework peers from the DSH host;
this is not a standalone npm Agent application. No special peer-resolution flags
or consumer overrides are used. The separate embedded package retains its
strict standalone npm tarball-consumer check. Registry access is required;
these checks do not provision a sandbox or call a real model.
