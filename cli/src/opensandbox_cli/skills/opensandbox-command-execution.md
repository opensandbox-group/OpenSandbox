---
name: command-execution
description: Use OpenSandbox command execution commands to run foreground or background processes, inspect tracked execution status and logs, interrupt work, and manage persistent shell sessions inside a sandbox. Trigger when users want to execute commands in a sandbox and need exact OpenSandbox CLI flows.
---

# OpenSandbox Command Execution

Run commands with `osb command`. Use foreground streaming by default, and add `--background` when you need tracked execution.

## When To Use

- the user wants to run a one-off command in a sandbox
- the user needs a tracked background command with later status/log inspection
- the user wants to stop a running command
- the user wants a persistent shell session that keeps working directory or environment state across runs

## Config Gate

Run this first:

```bash
osb config show -o json
```

If `domain` is missing, stop and set it before command execution:

```bash
osb config set connection.domain <host:port> -o json
osb config show -o json
```

If auth is required and `api_key` is missing, stop and set it before command execution:

```bash
osb config set connection.api_key <api-key> -o json
osb config show -o json
```

## Separator Rule

Use `--` before the sandbox command payload. This is required when the payload contains flags like `-l`, `-c`, or `-m`.

```bash
osb command run <sandbox-id> -o raw -- sh -lc 'echo ready'
osb command run <sandbox-id> -o raw -- python -m http.server
osb command session run <sandbox-id> <session-id> -o raw -- sh -lc 'pwd'
```

## Execution Modes

Treat these as three distinct execution paths:

- `osb command run` without `--background`
  Use for foreground one-shot commands when the result should stream directly to the terminal
- `osb command run --background`
  Use when the user needs an execution ID, later status checks, or log retrieval
- `osb command session ...`
  Use when shell state must persist across commands, such as exported variables or a working directory

## Golden Paths

Foreground one-shot command:

```bash
osb command run <sandbox-id> -o raw -- python -c "print(1 + 1)"
```

Tracked background command:

```bash
osb command run <sandbox-id> --background -o json -- sh -c "sleep 10; echo done"
osb command status <sandbox-id> <execution-id> -o json
osb command logs <sandbox-id> <execution-id> -o json
```

Persistent session:

```bash
osb command session create <sandbox-id> --workdir /workspace -o json
osb command session run <sandbox-id> <session-id> -o raw -- pwd
osb command session run <sandbox-id> <session-id> -o raw -- export FOO=bar
osb command session run <sandbox-id> <session-id> -o raw -- sh -c 'echo $FOO'
osb command session delete <sandbox-id> <session-id> -o json
```

## Foreground Commands

For simple one-off execution, use:

```bash
osb command run <sandbox-id> -o raw -- <command>
osb command run <sandbox-id> --workdir /workspace -o raw -- <command>
osb command run <sandbox-id> --timeout 30s -o raw -- <command>
```

Use foreground mode when the user wants immediate output and does not need a tracked execution ID.

## Background Commands

Use background mode when the user will need follow-up inspection:

```bash
osb command run <sandbox-id> --background -o json -- <command>
osb command run <sandbox-id> --background --workdir /workspace -o json -- <command>
osb command run <sandbox-id> --background --timeout 5m -o json -- <command>
```

Then inspect the tracked execution:

```bash
osb command status <sandbox-id> <execution-id> -o json
osb command logs <sandbox-id> <execution-id> -o json
osb command logs <sandbox-id> <execution-id> --cursor 0 -o json
```

Use `status` for lifecycle state and exit information. Use `logs` for tracked background output. Do not suggest `command logs` for foreground commands that already streamed to the terminal.

## Persistent Sessions

Use sessions when commands must share shell state:

```bash
osb command session create <sandbox-id> --workdir /workspace -o json
osb command session run <sandbox-id> <session-id> -o raw -- pwd
osb command session run <sandbox-id> <session-id> -o raw -- export FOO=bar
osb command session run <sandbox-id> <session-id> -o raw -- sh -c 'echo $FOO'
osb command session run <sandbox-id> <session-id> --workdir /var -o raw -- pwd
osb command session delete <sandbox-id> <session-id> -o json
```

Rules:

- `session create --workdir` sets the initial working directory for the session
- `session run --workdir` overrides the working directory for that single run only
- exported variables and shell state persist across `session run` calls in the same session
- delete the session when the user is done; do not leave idle sessions around

## Interrupting Work

Interrupt only tracked executions:

```bash
osb command interrupt <sandbox-id> <execution-id> -o json
```

Only suggest interruption when the user explicitly wants to stop work or the process is clearly stuck.

## Failure Semantics

- foreground `osb command run` streams output directly and requires `-o raw`
- background `osb command run --background` returns tracked execution metadata and supports structured output
- `session run` also exits non-zero on execution error
- tracked background commands should be checked with `status` and `logs`
- if the command failure is caused by an unhealthy sandbox rather than the command itself, switch to `sandbox-troubleshooting`

## Response Pattern

Structure the answer as:

1. exact command to run
2. which execution mode it uses
3. the next inspection or cleanup command if the workflow continues

Keep command examples concrete and ready to paste.

## Minimal Closed Loops

Foreground command with timeout:

```bash
osb command run <sandbox-id> --timeout 30s -o raw -- python -c "print(1 + 1)"
```

Tracked background execution:

```bash
osb command run <sandbox-id> --background -o json -- sh -c "sleep 10; echo done"
osb command status <sandbox-id> <execution-id> -o json
osb command logs <sandbox-id> <execution-id> -o json
```

Background execution with interrupt:

```bash
osb command run <sandbox-id> --background -o json -- sh -c "sleep 300"
osb command interrupt <sandbox-id> <execution-id> -o json
osb command status <sandbox-id> <execution-id> -o json
```

Persistent session with shared shell state:

```bash
osb command session create <sandbox-id> --workdir /workspace -o json
osb command session run <sandbox-id> <session-id> -o raw -- export FOO=bar
osb command session run <sandbox-id> <session-id> -o raw -- sh -c 'echo $FOO'
osb command session delete <sandbox-id> <session-id> -o json
```

Per-run working directory override inside a session:

```bash
osb command session create <sandbox-id> --workdir /tmp -o json
osb command session run <sandbox-id> <session-id> -o raw -- pwd
osb command session run <sandbox-id> <session-id> --workdir /var -o raw -- pwd
osb command session delete <sandbox-id> <session-id> -o json
```

## Best Practices

- use `osb command run -o raw -- ...` for quick foreground commands
- use `--background` when the user will need execution tracking
- use sessions only when state persistence is actually needed
- use `--workdir` explicitly when directory context matters
- use `--timeout` when the command should not run indefinitely
- prefer `status` before guessing whether a background command is still running or already failed
