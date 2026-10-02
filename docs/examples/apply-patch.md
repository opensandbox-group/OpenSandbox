---
title: Apply Model Patches
description: Apply Codex-style file patches in an OpenSandbox sandbox with a custom image.
---

# Apply Model Patches

OpenSandbox exposes file operations and command execution, but the default
`execd` image does not include a patch interpreter. The `/files/replace` API
performs literal string replacement; it does not understand contextual hunks,
file creation, deletion, or moves.

If your coding agent emits the patch format used by OpenAI Codex, install the
standalone `apply_patch` executable in a custom sandbox image and run it inside
the sandbox. This keeps the patch dialect in the agent image instead of adding
vendor-specific behavior to the OpenSandbox file API.

## Build a custom image

The following example builds the executable from a pinned Codex revision and
copies it into the OpenSandbox `execd` image. Pin `CODEX_REF` to a reviewed
commit in production so image builds remain reproducible.

```dockerfile
FROM rust:alpine AS apply-patch-builder

ARG CODEX_REF=<reviewed-commit>
RUN apk add --no-cache build-base git musl-dev
WORKDIR /src
RUN git clone --depth 1 https://github.com/openai/codex.git . \
    && git fetch --depth 1 origin ${CODEX_REF} \
    && git checkout ${CODEX_REF}
RUN cargo build --release --locked -p codex-apply-patch

FROM opensandbox/execd:v1.1.0
COPY --from=apply-patch-builder /src/target/release/apply_patch /usr/local/bin/apply_patch
RUN chmod 0755 /usr/local/bin/apply_patch
```

Build and publish the image, then use its name when creating the sandbox:

```shell
docker build --build-arg CODEX_REF=<reviewed-commit> \
  -t my-registry/opensandbox-execd-with-patch:1 .
docker push my-registry/opensandbox-execd-with-patch:1
```

The builder and runtime stages must produce compatible binaries. If your base
image is not Alpine, use a builder with the matching C library and toolchain.

## Apply a patch from the Python SDK

Write the model output to a temporary file, then invoke `apply_patch` through
the existing command API. The executable accepts either one patch argument or
the patch on standard input; using a file avoids shell quoting and argument
length problems.

```python
from opensandbox import Sandbox

patch = """*** Begin Patch
*** Update File: /workspace/hello.py
@@
-print("hello")
+print("hello from OpenSandbox")
*** End Patch
"""

async with Sandbox.create(image="my-registry/opensandbox-execd-with-patch:1") as sandbox:
    await sandbox.files.write_file("/tmp/change.patch", patch)
    result = await sandbox.commands.run(
        "cd /workspace && apply_patch < /tmp/change.patch"
    )
    stdout = result.logs.stdout[0].text if result.logs.stdout else ""
    print(stdout)
```

Patch paths are resolved relative to the command working directory unless the
patch uses an absolute path. Keep the working directory and patch paths inside
the sandbox workspace, and treat patch content as untrusted input from the
model.

## Operational notes

- `apply_patch` can add, update, delete, and move files and matches update
  hunks using surrounding context.
- A failed patch can leave earlier file changes committed. Check the command
  exit status and inspect the affected files before retrying.
- The default OpenSandbox images remain unchanged. Use the custom image only
  for agents that need this patch format.
- For agents that already manage their own editing tools, continue using the
  regular file and command APIs instead of installing this executable.

See the [execd command API](/api/) and the
[Codex CLI example](/examples/codex-cli) for the surrounding sandbox setup.
