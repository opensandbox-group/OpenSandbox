#!/usr/bin/env python3

#
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
#
"""
OpenAPI client generation script for OpenSandbox Python SDK.

This script generates Python client code from OpenAPI specifications
using openapi-python-client, which generates httpx-based async clients
that support custom httpx.AsyncClient injection.
"""

import shutil
import subprocess
import sys
from pathlib import Path

APACHE_2_LICENSE_HEADER = """#\n# Copyright 2026 Alibaba Group Holding Ltd.\n#\n# Licensed under the Apache License, Version 2.0 (the "License");\n# you may not use this file except in compliance with the License.\n# You may obtain a copy of the License at\n#\n#     http://www.apache.org/licenses/LICENSE-2.0\n#\n# Unless required by applicable law or agreed to in writing, software\n# distributed under the License is distributed on an "AS IS" BASIS,\n# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.\n# See the License for the specific language governing permissions and\n# limitations under the License.\n#\n\n"""


def run_command(cmd: list[str], description: str) -> subprocess.CompletedProcess:
    """Run a command and handle errors."""
    print(f"Running: {description}")
    print(f"Command: {' '.join(cmd)}")

    try:
        result = subprocess.run(cmd, check=True, capture_output=True, text=True)
        print("✅ Success!")
        if result.stdout:
            print(f"Output: {result.stdout}")
        return result
    except subprocess.CalledProcessError as e:
        print(f"❌ Error: {e}")
        if e.stdout:
            print(f"Stdout: {e.stdout}")
        if e.stderr:
            print(f"Stderr: {e.stderr}")
        raise


def generate_execd_api_client() -> None:
    """Generate the execd API client from OpenAPI spec."""
    print("\n🔧 Generating execd API client...")

    spec_path = Path("../../../specs/execd-api.yaml").resolve()
    output_path = Path("src/opensandbox/api/execd")
    config_path = Path("scripts/openapi_execd_config.yaml")
    temp_output = Path("temp_execd_client")

    if not spec_path.exists():
        print(f"❌ OpenAPI spec not found at {spec_path}")
        print("Please ensure the specs directory is available")
        return

    # Remove existing generated code
    if output_path.exists():
        shutil.rmtree(output_path)

    # Remove temp directory if exists
    if temp_output.exists():
        shutil.rmtree(temp_output)

    # Generate using openapi-python-client
    cmd = [
        "openapi-python-client",
        "generate",
        "--path",
        str(spec_path),
        "--output-path",
        str(temp_output),
        "--config",
        str(config_path),
        "--overwrite",
    ]

    try:
        run_command(cmd, "Generating execd API client")
    except subprocess.CalledProcessError:
        print("❌ Failed to generate execd API client")
        return

    # Move generated files to correct location
    # openapi-python-client generates package inside the output directory
    generated_package = temp_output / "opensandbox_api_execd"
    if generated_package.exists():
        output_path.parent.mkdir(parents=True, exist_ok=True)
        shutil.move(str(generated_package), str(output_path))
        shutil.rmtree(temp_output)
        print(f"✅ Moved generated code to {output_path}")
    else:
        # If package name doesn't match, find the generated package
        for item in temp_output.iterdir():
            if item.is_dir() and not item.name.startswith("."):
                output_path.parent.mkdir(parents=True, exist_ok=True)
                shutil.move(str(item), str(output_path))
                shutil.rmtree(temp_output)
                print(f"✅ Moved generated code from {item} to {output_path}")
                break


def generate_egress_api_client() -> None:
    """Generate the egress API client from OpenAPI spec."""
    print("\n🔧 Generating egress API client...")

    spec_path = Path("../../../specs/egress-api.yaml").resolve()
    output_path = Path("src/opensandbox/api/egress")
    config_path = Path("scripts/openapi_egress_config.yaml")
    temp_output = Path("temp_egress_client")

    if not spec_path.exists():
        print(f"❌ OpenAPI spec not found at {spec_path}")
        print("Please ensure the specs directory is available")
        return

    if output_path.exists():
        shutil.rmtree(output_path)

    if temp_output.exists():
        shutil.rmtree(temp_output)

    cmd = [
        "openapi-python-client",
        "generate",
        "--path",
        str(spec_path),
        "--output-path",
        str(temp_output),
        "--config",
        str(config_path),
        "--overwrite",
    ]

    try:
        run_command(cmd, "Generating egress API client")
    except subprocess.CalledProcessError:
        print("❌ Failed to generate egress API client")
        return

    generated_package = temp_output / "opensandbox_api_egress"
    if generated_package.exists():
        output_path.parent.mkdir(parents=True, exist_ok=True)
        shutil.move(str(generated_package), str(output_path))
        shutil.rmtree(temp_output)
        print(f"✅ Moved generated code to {output_path}")
    else:
        for item in temp_output.iterdir():
            if item.is_dir() and not item.name.startswith("."):
                output_path.parent.mkdir(parents=True, exist_ok=True)
                shutil.move(str(item), str(output_path))
                shutil.rmtree(temp_output)
                print(f"✅ Moved generated code from {item} to {output_path}")
                break


def generate_diagnostic_api_client() -> None:
    """Generate the diagnostics API client from OpenAPI spec."""
    print("\n🔧 Generating diagnostics API client...")

    spec_path = Path("../../../specs/diagnostic-api.yml").resolve()
    output_path = Path("src/opensandbox/api/diagnostic")
    config_path = Path("scripts/openapi_diagnostic_config.yaml")
    temp_output = Path("temp_diagnostic_client")

    if not spec_path.exists():
        print(f"❌ OpenAPI spec not found at {spec_path}")
        print("Please ensure the specs directory is available")
        return

    if output_path.exists():
        shutil.rmtree(output_path)

    if temp_output.exists():
        shutil.rmtree(temp_output)

    cmd = [
        "openapi-python-client",
        "generate",
        "--path",
        str(spec_path),
        "--output-path",
        str(temp_output),
        "--config",
        str(config_path),
        "--overwrite",
    ]

    try:
        run_command(cmd, "Generating diagnostics API client")
    except subprocess.CalledProcessError:
        print("❌ Failed to generate diagnostics API client")
        return

    generated_package = temp_output / "opensandbox_api_diagnostic"
    if generated_package.exists():
        output_path.parent.mkdir(parents=True, exist_ok=True)
        shutil.move(str(generated_package), str(output_path))
        shutil.rmtree(temp_output)
        print(f"✅ Moved generated code to {output_path}")
    else:
        for item in temp_output.iterdir():
            if item.is_dir() and not item.name.startswith("."):
                output_path.parent.mkdir(parents=True, exist_ok=True)
                shutil.move(str(item), str(output_path))
                shutil.rmtree(temp_output)
                print(f"✅ Moved generated code from {item} to {output_path}")
                break


def generate_sandbox_lifecycle_api() -> None:
    """Generate the sandbox lifecycle API client."""
    print("\n🔧 Generating sandbox lifecycle API client...")

    spec_path = Path("../../../specs/sandbox-lifecycle.yml").resolve()
    output_path = Path("src/opensandbox/api/lifecycle")
    config_path = Path("scripts/openapi_lifecycle_config.yaml")
    temp_output = Path("temp_lifecycle_client")

    if not spec_path.exists():
        print(f"❌ OpenAPI spec not found at {spec_path}")
        return

    # Remove existing generated code
    if output_path.exists():
        shutil.rmtree(output_path)

    # Remove temp directory if exists
    if temp_output.exists():
        shutil.rmtree(temp_output)

    # Generate using openapi-python-client
    cmd = [
        "openapi-python-client",
        "generate",
        "--path",
        str(spec_path),
        "--output-path",
        str(temp_output),
        "--config",
        str(config_path),
        "--overwrite",
    ]

    try:
        run_command(cmd, "Generating sandbox lifecycle API client")
    except subprocess.CalledProcessError:
        print("❌ Failed to generate lifecycle API client")
        return

    # Move generated files to correct location
    generated_package = temp_output / "opensandbox_api_lifecycle"
    if generated_package.exists():
        output_path.parent.mkdir(parents=True, exist_ok=True)
        shutil.move(str(generated_package), str(output_path))
        shutil.rmtree(temp_output)
        print(f"✅ Moved generated code to {output_path}")
    else:
        # If package name doesn't match, find the generated package
        for item in temp_output.iterdir():
            if item.is_dir() and not item.name.startswith("."):
                output_path.parent.mkdir(parents=True, exist_ok=True)
                shutil.move(str(item), str(output_path))
                shutil.rmtree(temp_output)
                print(f"✅ Moved generated code from {item} to {output_path}")
                break


def add_license_headers(root: Path) -> None:
    """Add Apache-2.0 license header to generated python files (idempotent)."""
    if not root.exists():
        return

    touched = 0
    skipped = 0

    for file_path in root.rglob("*.py"):
        content = file_path.read_text(encoding="utf-8")

        # Avoid double-inserting if generation already includes headers.
        # Keep the check lightweight and tolerant to minor variations.
        head = "\n".join(content.splitlines()[:50])
        if "Licensed under the Apache License, Version 2.0" in head:
            skipped += 1
            continue

        file_path.write_text(APACHE_2_LICENSE_HEADER + content, encoding="utf-8")
        touched += 1

    print(
        f"✅ Added license headers under {root} (updated={touched}, skipped={skipped})"
    )



def post_process_generated_code() -> None:
    """Post-process the generated code to ensure proper package structure."""
    print("\n🔧 Post-processing generated code...")

    # Ensure API directory has __init__.py
    api_dir = Path("src/opensandbox/api")
    if api_dir.exists():
        init_file = api_dir / "__init__.py"
        if not init_file.exists():
            init_file.write_text(
                '"""OpenSandbox API clients generated from OpenAPI specs."""\n'
            )
            print(f"✅ Created {init_file}")

    # Ensure all generated python files have a license header.
    add_license_headers(Path("src/opensandbox/api/execd"))
    add_license_headers(Path("src/opensandbox/api/egress"))
    add_license_headers(Path("src/opensandbox/api/diagnostic"))
    add_license_headers(Path("src/opensandbox/api/lifecycle"))
    add_license_headers(Path("src/opensandbox/api"))


def main() -> None:
    """Main function to generate all API clients."""
    print("🚀 OpenSandbox Python SDK API Generator")
    print("=" * 50)
    print("Using openapi-python-client for httpx-based async clients")
    print("=" * 50)

    # Check if openapi-python-client is available
    try:
        result = subprocess.run(
            ["openapi-python-client", "--version"], check=True, capture_output=True
        )
        version = result.stdout.decode().strip() or result.stderr.decode().strip()
        print(f"openapi-python-client version: {version}")
    except (subprocess.CalledProcessError, FileNotFoundError):
        print("❌ openapi-python-client not found!")
        print("Please install it with: pip install openapi-python-client")
        print("Or: uv add --dev openapi-python-client")
        sys.exit(1)

    # Create API directories
    Path("src/opensandbox/api").mkdir(parents=True, exist_ok=True)

    # Generate API clients
    generate_execd_api_client()
    generate_egress_api_client()
    generate_diagnostic_api_client()
    generate_sandbox_lifecycle_api()

    # Post-process
    post_process_generated_code()

    print("\n✅ API client generation completed!")
    print("Generated clients:")
    print("  - src/opensandbox/api/execd/")
    print("  - src/opensandbox/api/egress/")
    print("  - src/opensandbox/api/diagnostic/")
    print("  - src/opensandbox/api/lifecycle/")
    print("\nThe generated clients support custom httpx.AsyncClient injection:")
    print("  from opensandbox.api.execd import Client, AuthenticatedClient")
    print(
        '  client = AuthenticatedClient(base_url="...", token="...", httpx_client=custom_client)'
    )


if __name__ == "__main__":
    main()
