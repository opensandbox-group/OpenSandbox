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
from datetime import timedelta

from code_interpreter import CodeInterpreter, SupportedLanguage
from opensandbox import Sandbox
from opensandbox.config import ConnectionConfig


async def main() -> None:
    domain = os.getenv("SANDBOX_DOMAIN", "localhost:8080")
    api_key = os.getenv("SANDBOX_API_KEY")
    image = os.getenv(
        "SANDBOX_IMAGE",
        "sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/code-interpreter:v1.0.2",
    )

    config = ConnectionConfig(
        domain=domain,
        api_key=api_key,
        request_timeout=timedelta(seconds=60),
    )

    sandbox = await Sandbox.create(
        image,
        connection_config=config,
        extensions={"poolRef":"pool-sample"},
        entrypoint=["/opt/opensandbox/code-interpreter.sh"],
        env={
            "TEST_ENV": "test",
        },
    )

    async with sandbox:
        interpreter = await CodeInterpreter.create(sandbox=sandbox)

        # Verify environment variable is set
        print("\n=== Verify Environment Variable ===")
        env_check = await interpreter.codes.run(
            "import os\n"
            "test_env = os.getenv('TEST_ENV', 'NOT_SET')\n"
            "print(f'TEST_ENV value: {test_env}')\n"
            "test_env",
            language=SupportedLanguage.PYTHON,
        )
        for msg in env_check.logs.stdout:
            print(f"[ENV Check] {msg.text}")
        if env_check.result:
            for res in env_check.result:
                print(f"[ENV Result] {res.text}")

        # Java example: print to stdout and return the final result line.
        java_exec = await interpreter.codes.run(
            "System.out.println(\"Hello from Java!\");\n"
            "int result = 2 + 3;\n"
            "System.out.println(\"2 + 3 = \" + result);\n"
            "result",
            language=SupportedLanguage.JAVA,
        )
        print("\n=== Java example ===")
        for msg in java_exec.logs.stdout:
            print(f"[Java stdout] {msg.text}")
        if java_exec.result:
            for res in java_exec.result:
                print(f"[Java result] {res.text}")
        if java_exec.error:
            print(f"[Java error] {java_exec.error.name}: {java_exec.error.value}")

        # Go example: print logs and demonstrate a main function structure.
        go_exec = await interpreter.codes.run(
            "package main\n"
            "import \"fmt\"\n"
            "func main() {\n"
            "    fmt.Println(\"Hello from Go!\")\n"
            "    sum := 3 + 4\n"
            "    fmt.Println(\"3 + 4 =\", sum)\n"
            "}",
            language=SupportedLanguage.GO,
        )
        print("\n=== Go example ===")
        for msg in go_exec.logs.stdout:
            print(f"[Go stdout] {msg.text}")
        if go_exec.error:
            print(f"[Go error] {go_exec.error.name}: {go_exec.error.value}")

        # TypeScript example: use typing and sum an array.
        ts_exec = await interpreter.codes.run(
            "console.log('Hello from TypeScript!');\n"
            "const nums: number[] = [1, 2, 3];\n"
            "console.log('sum =', nums.reduce((a, b) => a + b, 0));",
            language=SupportedLanguage.TYPESCRIPT,
        )
        print("\n=== TypeScript example ===")
        for msg in ts_exec.logs.stdout:
            print(f"[TypeScript stdout] {msg.text}")
        if ts_exec.error:
            print(f"[TypeScript error] {ts_exec.error.name}: {ts_exec.error.value}")

        await sandbox.kill()


if __name__ == "__main__":
    asyncio.run(main())
