# Copyright 2025 Alibaba Group Holding Ltd.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

import asyncio
import os
import textwrap
from datetime import timedelta
from pathlib import Path

from opensandbox import Sandbox
from opensandbox.config import ConnectionConfig


def _load_requirements() -> str:
    requirements_path = Path(__file__).with_name("requirements.txt")
    return requirements_path.read_text(encoding="utf-8")


def _training_script() -> str:
    return textwrap.dedent(
        """
        import json
        import os

        import gymnasium as gym
        from stable_baselines3 import DQN
        from stable_baselines3.common.evaluation import evaluate_policy

        timesteps = int(os.getenv("RL_TIMESTEPS", "5000"))
        tensorboard_log = os.getenv("RL_TENSORBOARD_LOG", "runs")

        env = gym.make("CartPole-v1")
        model = DQN(
            "MlpPolicy",
            env,
            verbose=1,
            tensorboard_log=tensorboard_log,
            learning_rate=1e-3,
            buffer_size=10000,
            learning_starts=1000,
            batch_size=32,
            train_freq=4,
            gradient_steps=1,
        )

        model.learn(total_timesteps=timesteps)

        os.makedirs("checkpoints", exist_ok=True)
        checkpoint_path = "checkpoints/cartpole_dqn"
        model.save(checkpoint_path)

        mean_reward, std_reward = evaluate_policy(model, env, n_eval_episodes=5)
        summary = {
            "timesteps": timesteps,
            "mean_reward": float(mean_reward),
            "std_reward": float(std_reward),
            "checkpoint_path": f"{checkpoint_path}.zip",
        }
        with open("training_summary.json", "w", encoding="utf-8") as handle:
            json.dump(summary, handle, indent=2)

        print("Training summary:", summary)
        env.close()
        """
    ).lstrip()


async def _print_execution_logs(execution) -> None:
    for msg in execution.logs.stdout:
        print(f"[stdout] {msg.text}")
    for msg in execution.logs.stderr:
        print(f"[stderr] {msg.text}")
    if execution.error:
        print(f"[error] {execution.error.name}: {execution.error.value}")


def _execution_failed(execution) -> bool:
    return execution.error is not None


async def _run_command(sandbox: Sandbox, command: str) -> bool:
    execution = await sandbox.commands.run(command)
    await _print_execution_logs(execution)
    return not _execution_failed(execution)


def _with_python_env(command: str) -> str:
    return (
        "bash -lc '"
        "source /opt/opensandbox/code-interpreter-env.sh "
        "python ${PYTHON_VERSION:-3.14} >/dev/null "
        "&& "
        f"{command}"
        "'"
    )


async def _ensure_pip(sandbox: Sandbox) -> bool:
    bootstrap_commands = [
        _with_python_env("python3 -m pip --version"),
        _with_python_env("python3 -m ensurepip --upgrade"),
        "apt-get update && apt-get install -y python3-pip",
        "apk add --no-cache py3-pip",
    ]
    for command in bootstrap_commands:
        if await _run_command(sandbox, command):
            return True
    return False


async def _install_requirements(sandbox: Sandbox) -> bool:
    install_commands = [
        _with_python_env(
            "python3 -m pip install --no-cache-dir --break-system-packages -r requirements.txt"
        ),
        "pip3 install --no-cache-dir -r requirements.txt",
        "pip install --no-cache-dir -r requirements.txt",
    ]
    for command in install_commands:
        if await _run_command(sandbox, command):
            return True
    return False


async def main() -> None:
    domain = os.getenv("SANDBOX_DOMAIN", "localhost:8080")
    api_key = os.getenv("SANDBOX_API_KEY")
    image = os.getenv("SANDBOX_IMAGE", "sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/code-interpreter:v1.0.2")
    timesteps = os.getenv("RL_TIMESTEPS", "5000")

    config = ConnectionConfig(
        domain=domain,
        api_key=api_key,
        request_timeout=timedelta(minutes=10),
    )

    sandbox = await Sandbox.create(
        image,
        connection_config=config,
        env={"RL_TIMESTEPS": timesteps},
    )

    async with sandbox:
        try:
            await sandbox.files.write_file("requirements.txt", _load_requirements())
            if not await _ensure_pip(sandbox):
                print("Failed to bootstrap pip inside the sandbox.")
                return

            if not await _install_requirements(sandbox):
                print("Failed to install RL dependencies inside the sandbox.")
                return

            await sandbox.files.write_file("train.py", _training_script())
            train_exec = await sandbox.commands.run(_with_python_env("python3 train.py"))
            await _print_execution_logs(train_exec)
            if _execution_failed(train_exec):
                print("Training failed inside the sandbox.")
                return

            try:
                summary = await sandbox.files.read_file("training_summary.json")
            except Exception as exc:
                print(f"\nFailed to read training summary: {exc}")
            else:
                print("\n=== Training summary ===")
                print(summary)
        finally:
            await sandbox.kill()


if __name__ == "__main__":
    asyncio.run(main())
