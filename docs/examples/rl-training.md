---
title: RL Training
description: Run a basic reinforcement learning training loop (CartPole + DQN) inside an isolated OpenSandbox container.
---

# Reinforcement Learning Sandbox Example

Demonstrates running a basic RL training loop (CartPole + DQN) inside an isolated OpenSandbox container. The example installs RL dependencies in the sandbox, trains a policy, saves a checkpoint, and returns a training summary.

## Start OpenSandbox server [local]

Start the local OpenSandbox server:

```shell
uv pip install opensandbox-server
opensandbox-server init-config ~/.sandbox.toml --example docker
opensandbox-server
```

## Run the Example

```shell
# Install OpenSandbox package
uv pip install opensandbox

# Run the example
uv run python examples/rl-training/main.py
```

The script provisions a sandbox, installs RL dependencies, trains a DQN agent on CartPole, saves a checkpoint, and prints the JSON training summary.

![RL training screenshot](../public/images/rl-training-screenshot.jpg)

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `SANDBOX_DOMAIN` | `localhost:8080` | Sandbox service address |
| `SANDBOX_API_KEY` | _(optional)_ | API key if your server requires authentication |
| `SANDBOX_IMAGE` | `sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/code-interpreter:v1.1.0` | Docker image to use |
| `RL_TIMESTEPS` | `5000` | Training timesteps to run |

## TensorBoard

The training script logs to `runs/`. To visualize metrics, open a shell in the sandbox and run:

```shell
tensorboard --logdir runs --host 0.0.0.0 --port 6006
```

## References

- [Source code on GitHub](https://github.com/opensandbox-group/OpenSandbox/tree/main/examples/rl-training)
