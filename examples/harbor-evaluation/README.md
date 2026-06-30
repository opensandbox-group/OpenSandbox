# Harbor Evaluation on OpenSandbox

Run a [Harbor](https://github.com/harbor-framework/harbor) agent evaluation on OpenSandbox, provisioning one sandbox per trial.

The included `tasks/hello-opensandbox` task keeps this example self-contained, so
the config can run immediately after cloning the repo. For real evaluations,
prefer a task from an external Harbor task registry and override the example task
from the command line:

```shell
harbor run -c config.yaml --task <org>/<task>@latest
```

During local task development, you can also point Harbor at a task directory:

```shell
harbor run -c config.yaml --path /path/to/task
```

> **Full documentation**: [docs/examples/harbor-evaluation.md](../../docs/examples/harbor-evaluation.md)
