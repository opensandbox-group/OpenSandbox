---
title: DeerFlow
description: Run DeerFlow agent turns on OpenSandbox through the OpenSandboxProvider shipped in DeerFlow 2.1.0.
---

# DeerFlow + OpenSandbox Example

[DeerFlow](https://github.com/bytedance/deer-flow) is an open-source deep-research agent
framework. Since 2.1.0 it ships an OpenSandbox sandbox provider, so the `bash`, file, and
search tools an agent calls run inside a sandbox on your OpenSandbox server instead of on the
gateway host.

The provider lives in DeerFlow (`deerflow.community.opensandbox:OpenSandboxProvider`) and
drives the OpenSandbox Python SDK. Nothing extra is needed on the OpenSandbox side: this
example starts a server, points DeerFlow's `sandbox` section at it, and runs sandbox work
through DeerFlow's own provider.

## Start OpenSandbox server [local]

Start a local OpenSandbox server, logs will be visible in the terminal:

```shell
uv pip install opensandbox-server
opensandbox-server init-config ~/.sandbox.toml --example docker
opensandbox-server
```

::: info Docker runtime requirement
The server uses `runtime.type = "docker"` by default, so it **must** be able to reach a running
Docker daemon. Docker Desktop users should confirm with `docker version`; on Colima (macOS)
export `DOCKER_HOST="unix://${HOME}/.colima/default/docker.sock"` before starting the server.
:::

## Set up DeerFlow with the OpenSandbox provider

DeerFlow is a cloned application rather than a PyPI library — the harness and its
`deerflow-extension-api` companion are uv workspace members — so the provider extra is
installed from a checkout. DeerFlow requires Python 3.12 or later.

```shell
git clone https://github.com/bytedance/deer-flow.git
cd deer-flow

# optional: the full local workspace setup (backend, frontend, and pre-commit hooks)
make install

# backend dependencies plus the OpenSandbox provider
cd backend && uv sync --all-packages --extra opensandbox && cd ..
```

The provider is imported only when `sandbox.use` selects it.

::: warning Re-add the extra after a plain sync
`opensandbox` is an optional extra, and a plain `uv sync --locked` — which is what `make install`
runs for the backend — prunes it from the environment. If a later `make install` or `uv sync`
removes it, run `cd backend && uv sync --all-packages --extra opensandbox` again. The
docker-compose deployment is unaffected: it installs the extra at image build time from
`UV_EXTRAS=opensandbox` in `.env`.
:::

## Run the example

The example drives DeerFlow's provider directly, so it needs no model configuration and no
gateway process. It writes a minimal `config.yaml` containing only the `sandbox` section and
points `DEER_FLOW_CONFIG_PATH` at it.

Run it with the DeerFlow backend environment, from your OpenSandbox checkout:

```shell
uv run --project ../deer-flow/backend python examples/deer-flow/main.py
```

Any interpreter that can `import deerflow` works — for example a shell where the DeerFlow
checkout's `backend/.venv` is activated.

The script creates a sandbox through `provider.acquire()`, runs a command, writes a Python
script with DeerFlow's file API, executes it, then exercises `read_file`, `list_dir`, `glob`,
`grep`, and the `/mnt/user-data`-restricted `download_file`. The sandbox is released to the
warm pool and destroyed by `provider.shutdown()` on exit.

You should see output similar to:

```text
[config] DeerFlow config: /tmp/opensandbox-deerflow-xxxx/config.yaml
[create] DeerFlow sandbox c37d0a7b8a926499 bound to remote b5419ac9-f80b-4263-b403-9bd74ee54c43
[command] Linux-6.6.87.2-microsoft-standard-WSL2-x86_64-with-glibc2.39
[file] Wrote /mnt/user-data/workspace/fib.py
[command] [0, 1, 1, 2, 3, 5, 8, 13, 21, 34]
[file] Read back: def fibonacci(count):
[list_dir] ['/mnt/user-data/workspace', '/mnt/user-data/workspace/fib.py']
[glob] ['/mnt/user-data/workspace/fib.py']
[grep] /mnt/user-data/workspace/fib.py:1: def fibonacci(count):
[download] 170 bytes fetched from /mnt/user-data/workspace/fib.py
[cleanup] Sandbox destroyed
```

The sandbox id, remote id, and platform string differ on each run.

::: tip
DeerFlow logs `No models are configured ...` while loading this generated config. That notice
is expected on the provider-only path — the script never invokes a model.
:::

## Point the DeerFlow agent at OpenSandbox

For agent runs, put the sandbox section in the DeerFlow checkout's `config.yaml` (`make config`
creates one from `config.example.yaml`):

```yaml
sandbox:
  use: deerflow.community.opensandbox:OpenSandboxProvider
  image: python:3.11
  domain: localhost:8080
  protocol: http
  ready_timeout: 120        # create/readiness deadline; covers a first-run image pull
  sandbox_timeout: 14400    # remote lifetime in seconds; 0 means explicit cleanup only
  bash_command_timeout: 600 # default deadline for commands run in the sandbox
  replicas: 3               # active + warm sandboxes per gateway process
  idle_timeout: 600         # destroy a warm sandbox after this many idle seconds; 0 disables
```

| Option | Default | Description |
|--------|---------|-------------|
| `image` | `python:3.11` | Sandbox image. Any image with a POSIX shell works. |
| `domain` | `OPEN_SANDBOX_DOMAIN`, else `localhost:8080` | OpenSandbox server address |
| `api_key` | `OPEN_SANDBOX_API_KEY` | API key, if the server requires authentication |
| `protocol` | `http` | Use `https` for any non-loopback domain |
| `request_timeout` | `30` | Management API request timeout in seconds |
| `ready_timeout` | `30` | Create and readiness deadline in seconds |
| `use_server_proxy` | `false` | Route execd and file traffic through the server |
| `sandbox_timeout` | `14400` | Server-side lifetime in seconds; `0` disables renewal |
| `bash_command_timeout` | `600` | Default command deadline in seconds |
| `replicas` | `3` | Active plus warm sandbox cap per gateway process |
| `idle_timeout` | `600` | Idle seconds before a warm sandbox is destroyed; `0` disables |
| `environment` | `{}` | Environment variables injected into sandbox commands. A value starting with `$` is resolved from the gateway process environment. |

`api_key` and `domain` may be omitted when `OPEN_SANDBOX_API_KEY` and `OPEN_SANDBOX_DOMAIN` are
exported, which is how the example passes them. Set `use_server_proxy: true` when the gateway
can reach the OpenSandbox management service but cannot reach sandbox `execd` endpoints
directly.

Then run one headless agent turn. The prompt makes the agent call its `bash` tool, which now
executes inside OpenSandbox:

```shell
deerflow --print "Run python3 -c 'print(2 ** 16)' and report the output"
```

`deerflow` opens the terminal workbench when run interactively, while `--print` and `--json` run
one headless turn. The gateway and web UI start with `make dev`, and the docker-compose
deployment reaches the provider through the `UV_EXTRAS=opensandbox` setting in `.env`.

## Behavior notes

- The provider creates one sandbox per effective `(user_id, thread_id)` scope and parks it in an
  in-process warm pool after each turn. Only the same scope can reclaim it, and a reclaim runs a
  health check first.
- Create returns only after the SDK readiness check and DeerFlow's
  `/mnt/user-data/{workspace,uploads,outputs}` bootstrap succeed. A bootstrap failure destroys
  the newly created remote sandbox.
- Every operation renews the sandbox's server-side lifetime; commands that carry no explicit
  timeout use `bash_command_timeout`. Operations on one sandbox are serialized so a short
  renewal cannot shorten the horizon of an in-flight long command.
- `execute_command` forwards per-call environment variables and timeouts, and preserves stdout,
  stderr, and non-zero exit status in the returned text. File reads and writes use OpenSandbox's
  native filesystem API.
- `list_dir`, `glob`, and `grep` run portable `find`/`grep` commands inside the sandbox. All
  paths must be absolute and traversal-free; artifact downloads are restricted to
  `/mnt/user-data`.
- A command-path HTTP 404 or 410, an unhealthy session, or a broken transport evicts the dead
  client so the next acquire cold-starts a replacement.
- `replicas` is a per-gateway-process soft cap: DeerFlow does not coordinate sandbox ownership
  between processes yet, so one gateway process per OpenSandbox-backed deployment keeps the cap
  meaningful.

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `SANDBOX_DOMAIN` | `localhost:8080` | Server address (host and optional port) |
| `SANDBOX_PROTOCOL` | `http` | Protocol used to reach the server (`http` or `https`) |
| `SANDBOX_API_KEY` | _(optional for local)_ | API key if your server requires authentication |
| `SANDBOX_IMAGE` | `python:3.11` | Sandbox image used by this example |
| `OPEN_SANDBOX_DOMAIN` | _(unset)_ | Server address when the DeerFlow config omits `domain` |
| `OPEN_SANDBOX_API_KEY` | _(unset)_ | API key when the DeerFlow config omits `api_key` |

## References

- [DeerFlow](https://github.com/bytedance/deer-flow) - Open-source deep-research agent framework
- [DeerFlow OpenSandbox provider](https://github.com/bytedance/deer-flow/blob/main/backend/packages/harness/deerflow/community/opensandbox/README.md) - Provider documentation
- [deer-flow PR #4877](https://github.com/bytedance/deer-flow/pull/4877) - OpenSandbox provider contribution
- [OpenSandbox Python SDK](https://pypi.org/project/opensandbox/)
- [Source code on GitHub](https://github.com/opensandbox-group/OpenSandbox/tree/main/examples/deer-flow)
