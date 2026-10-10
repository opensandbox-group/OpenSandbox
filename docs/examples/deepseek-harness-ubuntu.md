---
title: DeepSeek Harness Ubuntu validation
description: Prepare a dedicated Ubuntu KVM workspace and run separately evidenced official-stack, SDK, and remote-only plugin checks.
---

# DeepSeek Harness Ubuntu validation

This is a user-operated runbook for a dedicated Ubuntu x86_64 KVM host. The
[consumer guide](/examples/deepseek-harness) covers installation and the separate
paid-model entry. This guide's `pnpm run e2e` uses a deterministic `LlmAdapter`
with the real OpenSandbox JavaScript SDK and actual Agent bash/read/edit tools.
It creates and deletes two real template sandboxes and never contacts a model
provider. Ordinary unit tests and the offline smoke do not run this sequence.

No real deployment or live plugin E2E has been validated by preparing this guide.
A cloud machine without accessible KVM or Docker is blocked; local simulation,
Docker-only substitution, or a paid-model call cannot replace the real run.

## Review host changes and permissions first

::: warning Execution approval and private credentials
Do not run `up` until the host owner has reviewed the exact paths and changes.
If an assistant executes it, obtain the user's action-time approval for generating
and configuring the OpenSandbox signing key at `WORK/opensandbox-signing-key`,
the dedicated kind cluster's credentials, and the pull/publish Kubernetes Secrets
and `WORK/agent-registry.json`. This creates persistent access. Generic permission
to prepare an environment does not authorize these credential steps.

The official `status` prints the server API key, and `up` prints it too. Raw
`WORK/run.log`, `WORK/logs/`, generated manifests, signing files, kubeconfig and
private captures may contain credentials. Keep them private, never attach them as
public evidence, and never capture raw `status` output into an evidence file.
The commands below retain only selected credential-free status fields.
:::

Review the pinned script, including its failure and teardown paths, before execution:

- `sudo`, tool installation under `/usr/local/bin`, and package-manager activity
- Changing `fs.inotify.max_user_instances`; recording and restoring its old value
- A sparse 24 GiB XFS loop image, formatting, mounting, reflink probes and cache
  scrubbing. The mount and every destructive path must belong to this run
- Privileged kind nodes, `/dev/kvm` passthrough, Firecracker, Docker builds and
  container/network creation; do not weaken host security to make a check pass
- Host ports `18080`, `18081`, `19000`, `19001`; these commands bind the service
  ports to loopback. Check for other listeners first
- Deletion of the named cluster/container, `WORK/rustfs-data`, `WORK/rc-config`,
  `WORK/gen`, `WORK/agent-registry.json`, `WORK/opensandbox-signing-key`, the
  fast-sandbox generated directory, XFS loop file and owned per-node caches
- Go/npm/Python downloads, built images, build caches and retained source/downloads

`SKIP_TOOL_INSTALL=1` skips the official general-tool installer, but is not a
universal installation guard: `ensure_xfsprogs` can still install `xfsprogs`.
Preinstall and verify `mkfs.xfs` explicitly. Keep `SKIP_LEFTOVER_CLEAN=1`; it stops
`up` from automatically deleting a prior named environment. Do not add
`--auto-clean`. No blanket Docker pruning or deletion is part of this runbook.

If KVM access or cgroup v2 is missing, stop and ask the host administrator for an
appropriate host. Do not automatically change kernel options, disable security,
or reboot. A passed preflight does not itself authorize any host changes.

## Versions and prerequisite tools

The reviewed integration is based on OpenSandbox
`4a7fcba5a4ded67e3856180936371673d390399b`, with dsh source reference
`5badb15009ae1756c3afe0ae0cef1faafc290ccc`. Run the reviewed integration commit,
not the baseline alone, which does not contain the new consumer. Record its actual
HEAD. The repository's `manifests/third-party/fast-sandbox.commit` must contain
`b702fbe71593df4f36b591e540445c251a698e7c`.

- Node `^22.19.0 || >=24.0.0`, pnpm `11.7.0`
- OpenSandbox SDK `1.1.0`, dsh packages `0.2.1-alpha.1`, Cordis `4.0.5-alpha.1`
- Go `>=1.25`, Python 3, `uv`, GNU bash/coreutils, git, curl, make, a C compiler,
  jq, openssl, util-linux (`findmnt`, `losetup`), iproute2 (`ss`), `xfsprogs`
- Reachable Docker daemon, cgroup v2, and readable/writable `/dev/kvm`
- Preinstalled kind `v0.24.0`, kubectl `v1.31.0`, helm `v3.16.4`, matching the
  official installer defaults. Record actual versions if using other releases
- Internet access to the official source/package/image registries and sufficient
  RAM/CPU for the two-node environment
- At least 20 GiB free on every WORK, download and Docker filesystem. This is a
  fail-fast floor, not a complete capacity estimate; allocate generous additional
  space for Docker layers, sources, RustFS and the XFS image

Install missing tools yourself from official sources after reviewing their
permissions. This preflight does not install anything. pnpm `9.15.0` is used only
if building the docs site; it is not the consumer package manager.

Floating defaults include `debian:bookworm-slim`, RustFS/RC `latest`,
`opensandbox/execd:latest`, kind node images and local `dev`/`env` tags. A source
SHA alone does not make the infrastructure fully reproducible. Retain actual
image IDs and registry digests, including Kubernetes runtime `imageID` values;
locally built images may have an empty `RepoDigests` list.

## 1. Prepare separate source, environment, evidence, downloads and host observation

Use a fresh root on a sufficiently large filesystem. Replace `SOURCE_CHECKOUT`
with the absolute path of the reviewed local checkout containing this example.
Cloning a reviewed local checkout also works before the integration is published.
These are explicit user-run filesystem changes; they are not done by preflight.

```sh
set -euo pipefail
umask 077
export RUN_ID="$(date -u +%Y%m%dt%H%M%Sz)"
export DSH_WORKSPACE_ROOT="$HOME/dsh-ubuntu-e2e-$RUN_ID"
export SOURCE_CHECKOUT=/absolute/path/to/reviewed/OpenSandbox
export SOURCE_ROOT="$DSH_WORKSPACE_ROOT/source/OpenSandbox"
export WORK="$DSH_WORKSPACE_ROOT/env"
export DSH_EVIDENCE_PATH="$DSH_WORKSPACE_ROOT/evidence/plugin-e2e.json"
export DSH_DOWNLOADS_DIR="$DSH_WORKSPACE_ROOT/downloads"
export DSH_HOST_OBSERVATION_DIR="$DSH_WORKSPACE_ROOT/host-observation"
export DSH_PNPM_EXECUTABLE=/absolute/path/to/already-installed/pnpm/bin/pnpm.cjs
pnpm() { npm_config_manage_package_manager_versions=false node "$DSH_PNPM_EXECUTABLE" "$@"; }

test ! -e "$DSH_WORKSPACE_ROOT"
test ! -L "$DSH_WORKSPACE_ROOT"
mkdir -p "$DSH_WORKSPACE_ROOT/source" "$DSH_WORKSPACE_ROOT/evidence" "$DSH_DOWNLOADS_DIR" "$DSH_HOST_OBSERVATION_DIR"
test -z "$(git -C "$SOURCE_CHECKOUT" status --porcelain)"
git clone --no-hardlinks "$SOURCE_CHECKOUT" "$SOURCE_ROOT"
git -C "$SOURCE_ROOT" checkout --detach "$(git -C "$SOURCE_CHECKOUT" rev-parse HEAD)"
test -z "$(git -C "$SOURCE_ROOT" status --porcelain)"
git -C "$SOURCE_ROOT" merge-base --is-ancestor 4a7fcba5a4ded67e3856180936371673d390399b HEAD
test "$(sed -n 's/^commit: //p' "$SOURCE_ROOT/manifests/third-party/fast-sandbox.commit")" = b702fbe71593df4f36b591e540445c251a698e7c

export FSB_DIR="$WORK/fast-sandbox"
export XFS_LOOP_FILE="$WORK/fast-sandbox.img"
export XFS_MOUNT_POINT="$WORK/stateroot"
export XFS_STATEROOT=1
export XFS_SIZE=24G
export KIND_CLUSTER="dsh-ubuntu-e2e-$RUN_ID"
export RUSTFS_CONTAINER="dsh-ubuntu-e2e-rustfs-$RUN_ID"
export KUBECONFIG="$WORK/kubeconfig"
export SERVER_HOST_PORT=18080 GATEWAY_HOST_PORT=18081
export RUSTFS_PORT=19000 RUSTFS_CONSOLE_PORT=19001
export SKIP_TOOL_INSTALL=1 SKIP_LEFTOVER_CLEAN=1
export SBX_IMAGE=opensandbox/fsb-sandbox-golden:latest
```

Set `DSH_PNPM_EXECUTABLE` to an already installed real pnpm module, never an
unresolved Corepack/Volta/download shim. The shell function above uses that exact
module for subsequent commands. Preflight inspects its resolved pnpm-package
identity before querying its version; it refuses unidentified shims without
invoking them or creating a version-manager cache.

The lowercase timestamp keeps resource names compatible with kind. Preflight rejects
`KIND_CLUSTER` values outside `^[a-z0-9.-]+$` before any `up` command; it also
retains the existing resource-collision checks.

The source checkout is retained separately. `WORK` is initially absent. Evidence
and downloads are siblings of `env/` and survive its teardown. The separate
`host-observation/` directory is an explicitly owned, readable local observation
area; the E2E never writes or removes test files there. The private
kubeconfig is scoped to this run rather than the user's default kubeconfig.
Never point `FSB_DIR`, XFS or cleanup paths at unrelated data. Existing paths,
symlink aliases, prior named clusters/containers or an existing download artifact
must be investigated; do not automatically overwrite or remove them.

Keep the source SHA, fast-sandbox pin, official script hash and non-secret
configuration outside WORK. Never capture the complete environment:

```sh
{
  printf 'OpenSandbox reviewed HEAD: '; git -C "$SOURCE_ROOT" rev-parse HEAD
  printf 'OpenSandbox baseline: 4a7fcba5a4ded67e3856180936371673d390399b\n'
  printf 'dsh source reference: 5badb15009ae1756c3afe0ae0cef1faafc290ccc\n'
  cat "$SOURCE_ROOT/manifests/third-party/fast-sandbox.commit"
  sha256sum "$SOURCE_ROOT/scripts/fast-sandbox-env/integration-env.sh"
  printf 'WORK=%s\nFSB_DIR=%s\nXFS_LOOP_FILE=%s\nXFS_MOUNT_POINT=%s\nKIND_CLUSTER=%s\nRUSTFS_CONTAINER=%s\n' \
    "$WORK" "$FSB_DIR" "$XFS_LOOP_FILE" "$XFS_MOUNT_POINT" "$KIND_CLUSTER" "$RUSTFS_CONTAINER"
  printf 'inotify before up: '; cat /proc/sys/fs/inotify/max_user_instances
  node --version; pnpm --version; GOTOOLCHAIN=local go version
  docker --version; kind --version; kubectl version --client; helm version --short; uv --version
} > "$DSH_WORKSPACE_ROOT/evidence/versions-and-paths.txt"
```

Expected: an isolated checkout at the reviewed HEAD, correct source pin, and a
credential-free version/path record. Retain this checkout and configuration for
`down`; do not upgrade the teardown script between `up` and `down`.

## 2. Read-only host preflight

```sh
bash "$SOURCE_ROOT/examples/deepseek-harness/scripts/preflight-ubuntu.sh" \
  | tee "$DSH_WORKSPACE_ROOT/evidence/preflight.log"
```

Expected: exit `0`, zero blockers, unused dedicated WORK and four ports, usable
KVM, reachable Docker/cgroup v2, Go >=1.25, tools and disk headroom. Exit `1`
prints independent blockers. No sudo, install, directory creation or host
reconfiguration occurs. Resolve blockers manually and rerun before `up`.
Preflight is for a fresh run; it intentionally refuses WORK after deployment.
`FSB_DIR`, `XFS_LOOP_FILE`, `XFS_MOUNT_POINT`, `KIND_CLUSTER` and
`RUSTFS_CONTAINER` must be explicit. Missing exports are blockers; preflight
never substitutes a safe invented default for the official shared defaults.
`/var/lib/fast-sandbox` is refused because it is outside dedicated WORK.

## 3. Build the repository-provided sandbox test image and consumer

After reviewing Docker build and package-download activity:

```sh
cd "$SOURCE_ROOT"
docker build -f scripts/fast-sandbox-env/Dockerfile.sandbox-test-image \
  -t "$SBX_IMAGE" scripts/fast-sandbox-env
cd "$SOURCE_ROOT/integrations/deepseek-harness"
pnpm install --frozen-lockfile
pnpm run build
pnpm run typecheck
pnpm test
pnpm run test:helper
cd "$SOURCE_ROOT/examples/deepseek-harness"
pnpm install --frozen-lockfile
pnpm run typecheck
pnpm test
```

Expected: the test image exists and offline package checks pass. The image uses
Debian with Python 3, a bash user shell and `/srv/app` owned by `app`. Its OCI
`WORKDIR` is `/srv/app`; the remote integration independently verifies Linux,
bash, Python standard-library modules, and an existing writable cwd. The golden
image tag alone is not proof of these prerequisites. Do not assume `/workspace`
exists or replace the test image with a minimal image lacking Python/bash.

## 4. Review authorization, then start the official environment

Before this command, the user/local agent must review and authorize the concrete
credential, privileged-container, sysctl, XFS and deletion risks listed above.
Preinstall xfsprogs even with `SKIP_TOOL_INSTALL=1`. Retain private logs locally;
only the resulting exit status is copied into public evidence.

```sh
mkdir -- "$WORK"
cd "$SOURCE_ROOT"
if scripts/fast-sandbox-env/integration-env.sh up > "$WORK/up.private.log" 2>&1; then
  printf 'official up: exit=0; built-in stack self-tests passed\n' \
    > "$DSH_WORKSPACE_ROOT/evidence/official-up-result.txt"
else
  printf 'official up: failed; inspect private WORK logs locally\n' \
    > "$DSH_WORKSPACE_ROOT/evidence/official-up-result.txt"
  exit 1
fi
test -s "$WORK/template-id"
```

Expected: the official two-node stack, template and pool become ready; built-in
create → signed gateway → execd ping, pause/resume, and snapshot/restore checks
all finish. Private stage logs are under `WORK/logs/`, with `WORK/run.log` and
`WORK/up.private.log`. An exit-zero self-test is separate from SDK or plugin E2E.
A failed/partial `up` is not an invitation to retry creation or delete unrelated
resources. Inspect private logs and actual resource ownership before deciding
whether the same pinned `down` is appropriate.

Capture only the health allowlist from `status`, discarding the API-key line
before any public write:

```sh
scripts/fast-sandbox-env/integration-env.sh status 2>&1 \
  | awk '/^[[:space:]]+(server health:|gateway health:)/ { print }' \
  | tee "$DSH_WORKSPACE_ROOT/evidence/status-health.txt"
grep -q 'server health:.*OK' "$DSH_WORKSPACE_ROOT/evidence/status-health.txt"
grep -q 'gateway health:.*OK' "$DSH_WORKSPACE_ROOT/evidence/status-health.txt"
```

Expected: both health lines say `OK`. An empty file or an unreachable service is
failure even if `status` exits zero. Never use raw `status` with `tee` or append it
to a report. Keep the default server API key out of public artifacts.

Record actual image metadata and container-runtime IDs, excluding environment,
Secret data, full pod specs or Docker inspect configuration:

```sh
docker image ls --quiet --no-trunc | sort -u \
  | xargs -r docker image inspect --format '{{json .Id}} {{json .RepoTags}} {{json .RepoDigests}}' \
  > "$DSH_WORKSPACE_ROOT/evidence/docker-image-ids-digests.txt"
kubectl get pods -A -o json \
  | jq '[.items[] | {namespace: .metadata.namespace, name: .metadata.name, containers: \
      [(.status.initContainerStatuses[]?, .status.containerStatuses[]?) | {name, image, imageID}]}]' \
  > "$DSH_WORKSPACE_ROOT/evidence/runtime-image-ids.json"
docker image inspect "$SBX_IMAGE" --format '{{json .Id}} {{json .RepoTags}} {{json .RepoDigests}}' \
  > "$DSH_WORKSPACE_ROOT/evidence/sandbox-test-image.txt"
```

These are local verification records. On a shared host, scope image collection
to the images used by this dedicated run before sharing, and review all artifacts
for unrelated resource names. Record null/empty digests honestly, not as a pinned
registry artifact. A future identical tag can resolve to a different image.

## 5. Official Python SDK E2E

`uv` must already be on PATH. The official script reads `WORK/template-id`, sets
its own `OPENSANDBOX_TEST_*` configuration, and invokes
`tests/python/tests/test_fsb_e2e.py`. These names belong to that SDK test runner;
they are distinct from the plugin's `OPEN_SANDBOX_*` names.

```sh
if scripts/fast-sandbox-env/integration-env.sh sdk-e2e > "$WORK/sdk-e2e.private.log" 2>&1; then
  printf 'official Python SDK E2E: exit=0\n' > "$DSH_WORKSPACE_ROOT/evidence/sdk-e2e-result.txt"
else
  printf 'official Python SDK E2E: failed; inspect private WORK logs locally\n' \
    > "$DSH_WORKSPACE_ROOT/evidence/sdk-e2e-result.txt"
  exit 1
fi
```

Expected: the real Python SDK suite passes against the live stack. Private logs
stay under WORK. This proves neither a dsh Agent turn nor the plugin lifecycle.
Do not publish raw logs or failure dumps without a separate secret review.

## 6. Explicit real plugin E2E

Read the template from the official file, not a guessed pool/template name.
`OPEN_SANDBOX_API_KEY` below is the official script's local test credential;
keep it private and do not reuse it for a remotely exposed service.

```sh
export OPEN_SANDBOX_DOMAIN=127.0.0.1:18080
export OPEN_SANDBOX_PROTOCOL=http
export OPEN_SANDBOX_API_KEY=fast-sandbox-env
export DSH_TEMPLATE_ID="$(cat "$WORK/template-id")"
export DSH_SESSION_ID="dsh-e2e-$RUN_ID"
export DSH_REMOTE_CWD=/srv/app
export DSH_TTL_SECONDS=600
export DSH_LIVE_E2E=1
cd "$SOURCE_ROOT/examples/deepseek-harness"
pnpm run e2e > "$DSH_WORKSPACE_ROOT/evidence/plugin-console.json.log" 2>&1
```

If overriding the server port, change `OPEN_SANDBOX_DOMAIN` to match. The key must
be explicitly set; this entry never silently uses an unauthenticated default.
The live opt-in is independent of the paid-model entry. It needs no model key,
model endpoint, or model charges. The same cwd and filename are intentionally
used in both fresh sandboxes, which are owned by this disposable run.

Expected: exit `0` and `DSH_EVIDENCE_PATH` containing `schemaVersion: 2`,
`mode: real`, `success: true`, version pins, and all fourteen stage results:

1. The per-run unique note/binary filenames are absent at the harness process cwd,
   the corresponding absolute remote-cwd path on the host, and the explicitly
   owned `DSH_HOST_OBSERVATION_DIR`. Missing paths count as absent only after the nearest existing ancestor is
   confirmed accessible; inaccessible/unverified ancestors fail
2. The plugin opens a primary `create-template` binding and returns a known ID
3. The plugin's real SDK readiness succeeds
4. The plugin's Linux/Python/bash/writable-cwd prerequisite check succeeds
5. Eight binary bytes, including NUL and invalid UTF-8, are uploaded
6. The actual Agent executes exactly bash, read and guarded edit. The successful
   read value and its durable committed read payload both match the primary's
   unique marker, with a completed turn and successful tool results
7. Full and streamed downloads match the original SHA-256 and the edited note
   is independently verified. The intentional binary download is
   `downloads/<run-uuid>.download.bin`; `download_hash.facts.downloadFile` names it
8. Agent disposal and local close precede plugin reconnect to the same live ID;
   uploaded bytes survive
9. The plugin creates a distinct secondary template binding
10. Secondary plugin readiness succeeds
11. Secondary plugin prerequisites succeed
12. Concurrent independent Agent contexts use identical cwd/unique filename.
    Each actual read value and committed payload matches its own marker; direct
    SDK reads also verify each sandbox's final edited note
13. The live session's public shell service executes a long command. A transparent
    SDK event observer acknowledges its real init ID before the plugin's input
    AbortSignal is triggered. The plugin's own cancellation must return
    `aborted: true` and `timedOut: false` within five seconds, while a separate
    status observation proves `running: false` for that same ID. Interrupt
    attempt/acknowledgement facts are recorded without issuing a rescue SDK
    launch or interrupt. Failed/unresponsive interrupt acknowledgement never
    suppresses independent status observation. A confirmed stopped state plus
    valid plugin result can establish cancellation despite an unknown interrupt
    acknowledgement; otherwise the stage fails. The observation window is
    shorter than the command's 30-second natural completion and 45-second deadline
14. After Agent disposal and all cleanup attempts, the same required host paths
    are checked again, independently even after an earlier stage failure. Unexpected local artifacts remain untouched and make
    acceptance fail; inaccessible/unverified paths also fail. Intentional evidence
    and download outputs have different paths/names and are excluded

The forwarding SDK facade passes real SDK options, requests, results and stream
events through unchanged. It observes returned IDs before plugin preparation can
fail, readiness, command acknowledgements and actual DELETE attempts; it does
not replace the plugin or SDK transport. These host checks cover the named
candidate paths, not every possible file on the machine.

Cleanup independently disposes every session and normally calls the current
plugin binding's `kill()`. If plugin opening already attempted owned cleanup,
or a plugin kill has an unknown outcome, no second DELETE is automatically sent.
The SDK manager is used for an owned ID only when there was no prior deletion
attempt and no usable open binding, such as failed reconnect after local close.
An independent lifecycle GET waits up to 60 seconds for an actual typed HTTP
`404`. `cleanup.sandboxes` separately records `deletionAttempted`,
`sdkDeleteObserved`, `killPath` (`binding-kill`, `opener-kill` or `manager-cleanup`),
kill acceptance/unconfirmed outcome, typed-404/still-present/unconfirmed lookup,
and local-close results. A later 404 cannot erase a failed plugin lifecycle
acceptance result.

A generic failed reconnect, authentication error, timeout or missing output is
never accepted as deletion. Session disposal has a 10-second observation budget;
DELETE has a 15-second budget and local handle/manager close has a 5-second budget.
The E2E SDK facade also bounds the opener's owned DELETE and local close, so an
unresponsive cleanup cannot block the remaining finalizers. Each handle's close
is observed once; a timed-out result stays unconfirmed even if it settles later.
A timeout does not suppress other independent attempts. Each stage
has a separate passed/failed/skipped result; failed acceptance or cleanup gives
exit `1`. Typed opening failures retain their known ID, readiness/preflight stage
and opener cleanup state. Unknown creation outcomes retain the request UUIDs from
the reserved `dsh-binding-request` metadata key without automatically retrying.
Both image and template creation use a fresh UUID under this key, which satisfies
fast-sandbox's DNS-label constraints. Other caller metadata is passed unchanged.
For new attempts, correlate service metadata with `dsh-binding-request=<UUID>`.
The earlier candidate used `dsh.binding.request`, which the fast-sandbox create
mapping rejects with HTTP `400` before returning a sandbox ID. When inspecting
an earlier attempt, use its recorded key and UUID.
The current OpenSandbox list path filters Sandbox CR metadata and can return
HTTP `200` with zero matches for the dotted key. That result alone does not prove
absence; inspect the full list and independent Sandbox CRs. Raw FastPath Create
and List enforce their own
[DNS-label metadata contract](https://github.com/opensandbox-group/fast-sandbox/blob/b702fbe71593df4f36b591e540445c251a698e7c/internal/controlplane/fastpath/server.go#L1132).
Each creation attempt has its own stage. A secondary response that repeats an
already-owned ID fails that creation stage, skips its dependent work and retains
complete failed evidence; cleanup still addresses that unique owned ID only once.

Evidence contains structural facts, hashes and IDs, never API keys, headers,
raw SDK diagnostics, prompts, file contents or arbitrary model output. The JSON
file is reserved before network activity and existing evidence files are not
overwritten. Per-run artifact names and exclusive download writes avoid reuse.
All parent directories must already exist; canonical evidence/download/host
observation paths inside WORK are refused. Keep a fresh workspace per run.
Cancellation or an unknown deletion result still needs manual inspection; do not
infer absence from an unreachable API or remove a leaked host artifact to make
the next check pass.

## 7. Teardown with the retained script and configuration

After reviewing the exact recorded targets and confirming they are still owned
by this run, unset only the plugin live opt-in and use the same official script:

```sh
unset DSH_LIVE_E2E
cd "$SOURCE_ROOT"
if scripts/fast-sandbox-env/integration-env.sh down > "$WORK/down.private.log" 2>&1; then
  printf 'official down: exit=0\n' > "$DSH_WORKSPACE_ROOT/evidence/down-result.txt"
else
  printf 'official down: failed; cleanup is unconfirmed\n' > "$DSH_WORKSPACE_ROOT/evidence/down-result.txt"
  exit 1
fi
kind get clusters > "$DSH_WORKSPACE_ROOT/evidence/clusters-after-down.txt"
docker ps -a --filter "name=$RUSTFS_CONTAINER" --format '{{.Names}} {{.Status}}' \
  > "$DSH_WORKSPACE_ROOT/evidence/rustfs-after-down.txt"
findmnt -rn -o TARGET --target "$XFS_MOUNT_POINT" \
  > "$DSH_WORKSPACE_ROOT/evidence/mount-after-down.txt" || true
cat /proc/sys/fs/inotify/max_user_instances \
  > "$DSH_WORKSPACE_ROOT/evidence/inotify-after-down.txt"
test ! -e "$XFS_LOOP_FILE"
test ! -e "$WORK/opensandbox-signing-key"
test ! -e "$WORK/agent-registry.json"
```

Expected: the named kind cluster and RustFS container are absent, the dedicated
XFS loop is removed/unmounted, and the old inotify value is restored. `findmnt
--target` can report the enclosing filesystem after unmount; verify the recorded
TARGET is not the dedicated `XFS_MOUNT_POINT`, rather than assuming an empty
file. Compare inotify with the pre-run host value, not a guessed default.

`down` is not full host rollback. Its own success message cannot prove tools were
uninstalled, every sysctl restoration succeeded, all mounts or loop devices are
gone, or every host change was undone. Built/pulled Docker images, build caches,
package downloads, source checkouts, private logs, the kubeconfig file and some
empty directories may remain. Keep source/evidence/downloads for review; remove
only specifically reviewed owned leftovers in a separately authorized step.
Never apply blanket cleanup to the machine.

Keep the four outcomes separate in your report: read-only preflight; official
stack self-test; real Python SDK E2E; real plugin stage/deletion evidence. If any
was not run, say so explicitly. Passing offline unit/helper tests or preparing
this runbook does not establish real Ubuntu, KVM, gateway or plugin validation.
