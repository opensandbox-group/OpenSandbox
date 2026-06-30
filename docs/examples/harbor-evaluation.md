---
title: Harbor Evaluation
description: Run a Harbor agent evaluation on OpenSandbox, provisioning one sandbox per trial.
---

# Harbor Evaluation on OpenSandbox

Run a [Harbor](https://github.com/harbor-framework/harbor) agent evaluation on
OpenSandbox infrastructure. Harbor provisions one OpenSandbox container per
trial, runs the agent inside it, executes the task's verifier, and collects the
reward plus logs and artifacts.

Harbor gained native OpenSandbox support in
[harbor-framework/harbor#2054](https://github.com/harbor-framework/harbor/pull/2054),
which adds the `opensandbox` environment backend and the `harbor[opensandbox]`
install extra.

This example ships a minimal, self-contained task (`hello-opensandbox`) plus a
job config that selects the `opensandbox` environment. The task uses a prebuilt
image and an offline verifier so it runs anywhere without extra dependencies.

## Start OpenSandbox server [local]

Start a local OpenSandbox server (Docker runtime):

```shell
uv pip install opensandbox-server
opensandbox-server init-config ~/.sandbox.toml --example docker
opensandbox-server
```

## Install Harbor

The OpenSandbox backend was merged into Harbor's `main` branch
([#2054](https://github.com/harbor-framework/harbor/pull/2054)) but is not yet in
a published release. Until a release ships with it, install Harbor from git:

```shell
uv pip install "harbor[opensandbox] @ git+https://github.com/harbor-framework/harbor"
```

Once a released Harbor version includes the backend, the plain extra will work:

```shell
uv pip install "harbor[opensandbox]"   # after the next Harbor release
```

## Configure the connection

Harbor reads the server connection from environment variables (or from the
`domain` / `api_key` kwargs in `config.yaml`):

```shell
export OPENSANDBOX_DOMAIN="localhost:8080"   # OpenSandbox server address
export OPENSANDBOX_API_KEY=""                # API key, if your server requires one
```

`OPENSANDBOX_API_KEY` may be left empty for a local / no-auth server.

## Run the evaluation

```shell
# From examples/harbor-evaluation
harbor run -c config.yaml
```

Harbor creates an OpenSandbox sandbox from the task's prebuilt image
(`ubuntu:24.04`), runs the `oracle` agent (which executes the reference
solution), runs the verifier, records the reward, and tears the sandbox down.

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `OPENSANDBOX_DOMAIN` | `localhost:8080` | OpenSandbox server address |
| `OPENSANDBOX_API_KEY` | _(optional)_ | API key if your server requires authentication |

## Outputs

Results are written under the `jobs_dir` from `config.yaml`
(`jobs/opensandbox/<job-id>/<trial-id>/`), including `agent/` logs, `verifier/`
output (`reward.txt`, `ctrf.json`), collected `artifacts/`, and `results.json`.
A successful run produces `reward = 1.0`.

## Use your own task

The included `hello-opensandbox` task is checked into this repository only to
make the example self-contained and runnable without publishing or downloading
anything first. For real evaluations, prefer tasks from an external Harbor task
registry and override the example task from the command line:

```shell
harbor run -c config.yaml --task <org>/<task>@latest
```

During local task development, you can also point Harbor at a task directory:

```shell
harbor run -c config.yaml --path /path/to/task
```

## Evaluate a real agent

Swap the `oracle` agent for a real agent + model:

```shell
harbor run -c config.yaml -a claude-code -m "anthropic/claude-sonnet-4-5"
```

## OpenSandbox-specific notes

- **Prebuilt images only** — every task must set `[environment].docker_image` in
  `task.toml`; the OpenSandbox backend does not build Dockerfiles. Use
  `image_auth` kwargs for private registries.
- **Working directory** — set `[environment].workdir` (this task uses `/app`) so
  the agent's relative paths resolve where the verifier expects them.
- **Resources** — `cpus` and `memory_mb` map to hard sandbox limits; `storage_mb`
  is ignored; GPUs are supported via `gpus` / `gpu_types`.
- **Artifacts** — files written under `/logs/artifacts` are downloaded into the
  trial's `artifacts/` directory.
- **Multi-container** — `docker-compose.yaml` tasks are not supported; use
  single-container tasks.

## References

- [Source code on GitHub](https://github.com/opensandbox-group/OpenSandbox/tree/main/examples/harbor-evaluation)
- [Harbor](https://github.com/harbor-framework/harbor)
