---
name: file-operations
description: Use OpenSandbox file commands to read, write, upload, download, search, replace, move, delete, and inspect files or directories inside a sandbox. Trigger when users want exact sandbox file manipulation commands instead of generic shell guidance.
---

# OpenSandbox File Operations

Manipulate sandbox files with `osb file` commands. Choose the operation mode first, then use the matching verification step. Do not mix sandbox-internal edits with host-to-sandbox transfer commands casually.

## When To Use

- the user wants to read or write a file inside a sandbox
- the user needs to upload a local file into a sandbox or download a sandbox file back to the host
- the user wants to search, replace, move, chmod, or inspect paths
- the user needs directory creation or cleanup

## Config Gate

Run this first:

```bash
osb config show -o json
```

If `domain` is missing, stop and set it before file commands:

```bash
osb config set connection.domain <host:port> -o json
osb config show -o json
```

If auth is required and `api_key` is missing, stop and set it before file commands:

```bash
osb config set connection.api_key <api-key> -o json
osb config show -o json
```

## Operation Modes

Treat these as distinct categories:

- sandbox-only content operations
  `cat`, `write`, `replace`, `mv`, `mkdir`, `rm`, `rmdir`, `info`, `chmod`
- host-to-sandbox transfer
  `upload`
- sandbox-to-host transfer
  `download`
- discovery before modification
  `search`, then `info`

If the path is uncertain, search first. If the file boundary crosses between host and sandbox, use `upload` or `download` instead of `write` or `cat`.

## Golden Paths

Write and verify inside the sandbox:

```bash
osb file write <sandbox-id> /workspace/app.txt -c "hello" -o json
osb file cat <sandbox-id> /workspace/app.txt -o raw
```

Upload from host and verify in the sandbox:

```bash
osb file upload <sandbox-id> ./local.txt /workspace/local.txt -o json
osb file cat <sandbox-id> /workspace/local.txt -o raw
```

Search before editing:

```bash
osb file search <sandbox-id> /workspace --pattern "*.py" -o json
osb file info <sandbox-id> /workspace/main.py -o json
```

## Sandbox-Only File Edits

Read and write:

```bash
osb file cat <sandbox-id> /path/to/file -o raw
osb file write <sandbox-id> /path/to/file -c "hello" -o json
```

Use `write` with `-c/--content` when the new content is known directly. If the content should come from stdin, omit `-c` and pipe or paste the content into the command.

Edit existing content:

```bash
osb file replace <sandbox-id> /path/to/file --old old --new new -o json
osb file mv <sandbox-id> /old/path /new/path -o json
```

Prefer `replace` for small text substitutions and `mv` for rename/path changes. Do not rewrite a full file when a targeted replace is enough.

Create directories:

```bash
osb file mkdir <sandbox-id> /workspace/output -o json
osb file mkdir <sandbox-id> /workspace/a /workspace/b --mode 755 -o json
```

## Host <-> Sandbox Transfer

Host to sandbox:

```bash
osb file upload <sandbox-id> ./local.txt /remote/path/local.txt -o json
```

Sandbox to host:

```bash
osb file download <sandbox-id> /remote/path/result.json ./result.json -o json
```

Rules:

- use `upload` when the source file is on the host
- use `download` when the destination should be written to the host filesystem
- use `write` and `cat` only when the operation stays entirely inside the sandbox

## Metadata and Permissions

Inspect metadata:

```bash
osb file info <sandbox-id> /path/to/file -o json
osb file info <sandbox-id> /path/one /path/two -o json
```

Search by pattern:

```bash
osb file search <sandbox-id> /workspace --pattern "*.py" -o json
```

Set permissions:

```bash
osb file chmod <sandbox-id> /path/to/script --mode 755 -o json
osb file chmod <sandbox-id> /path/to/file --mode 644 --owner root --group root -o json
```

Use `info` after `chmod` when the user needs to confirm mode, ownership, or timestamps changed as expected.

## Destructive Operations

Delete files or directories only after verifying the target path:

```bash
osb file info <sandbox-id> /workspace/tmp.txt -o json
osb file rm <sandbox-id> /workspace/tmp.txt -o json
```

```bash
osb file search <sandbox-id> /workspace --pattern "old-*" -o json
osb file rmdir <sandbox-id> /workspace/old-dir -o json
```

Rules:

- prefer `info` when the exact path is known
- prefer `search` when the path is uncertain
- do not suggest `rm` or `rmdir` until the target has been verified
- after `mv`, `rm`, or `rmdir`, verify the new state with `info` or `search`

## Failure Semantics

- `upload` and `download` have host filesystem side effects; treat them as cross-boundary operations
- `download` writes to the local path immediately, so be explicit about the destination
- permission or ownership failures are usually path/runtime permission issues, not a reason to switch away from `osb file`
- if multiple file commands fail unexpectedly, check sandbox health before assuming a file-command bug

## Response Pattern

Structure the answer as:

1. exact `osb file` command
2. which operation mode it uses
3. the next verification command if the workflow continues

Keep command examples concrete and ready to paste.

## Minimal Closed Loops

Write and verify:

```bash
osb file write <sandbox-id> /workspace/app.txt -c "hello" -o json
osb file cat <sandbox-id> /workspace/app.txt -o raw
```

Upload and verify:

```bash
osb file upload <sandbox-id> ./local.txt /workspace/local.txt -o json
osb file cat <sandbox-id> /workspace/local.txt -o raw
```

Replace and verify:

```bash
osb file replace <sandbox-id> /workspace/app.txt --old hello --new world -o json
osb file cat <sandbox-id> /workspace/app.txt -o raw
```

Change permissions and inspect:

```bash
osb file chmod <sandbox-id> /workspace/script.sh --mode 755 -o json
osb file info <sandbox-id> /workspace/script.sh -o json
```

Create a directory and inspect it:

```bash
osb file mkdir <sandbox-id> /workspace/output -o json
osb file info <sandbox-id> /workspace/output -o json
```

Move a file and verify the new path:

```bash
osb file mv <sandbox-id> /workspace/app.txt /workspace/archive/app.txt -o json
osb file info <sandbox-id> /workspace/archive/app.txt -o json
```

Delete and verify removal:

```bash
osb file info <sandbox-id> /workspace/tmp.txt -o json
osb file rm <sandbox-id> /workspace/tmp.txt -o json
osb file search <sandbox-id> /workspace --pattern "tmp.txt" -o json
```

## Best Practices

- prefer `search` before modification when the path is not certain
- prefer `info` before destructive actions
- prefer `replace` over full rewrites for small text changes
- prefer `upload` and `download` only for host boundary crossings
- prefer explicit verification after `mv`, `chmod`, `rm`, and `rmdir`
