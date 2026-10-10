// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

import { randomUUID } from 'node:crypto';
import type { Context } from '@deepseek-ai/cordis';
import { ShellExecutor } from '@deepseek-ai/dsh-shell';
import type { ShellExecRequest, ShellExecSpec, ShellExecution, ShellRunResult } from '@deepseek-ai/dsh-shell';
import type { CommandStatus, RunCommandOpts, ServerStreamEvent } from '@alibaba-group/opensandbox';
import { BindingClosedError } from './errors.js';
import { OutputBuffer } from './output.js';
import { SdkTransport } from './sdk-transport.js';
import type { BoundSandbox } from './types.js';

const MAX_TIMER_MS = 2_147_483_647;
const MAX_CAPTURE_BYTES = 64 * 1024 * 1024;
/** Fixed wrapper: command/path/managed-key metadata are argv data; values stay in SDK envs. */
const WRAPPER = [
  'import os,sys,json',
  'os.chdir(sys.argv[4])',
  'managed=set(json.loads(sys.argv[3]))',
  'env={k:v for k,v in os.environ.items() if not k.startswith("DSH_") or k in managed}',
  'fd=os.open(sys.argv[1] or "/dev/null",os.O_RDONLY)',
  'os.dup2(fd,0)',
  'if fd != 0: os.close(fd)',
  'os.execvpe("bash",["bash","-c",sys.argv[2]],env)',
].join('\n');

export interface RemoteShellOptions {
  timeoutMs?: number;
  maxTimeoutMs?: number;
  maxOutputBytes?: number;
  maxStdinBytes?: number;
  /** Bounded local stop/status observation, separate from remote execution and sandbox TTL. */
  settlementTimeoutMs?: number;
}
export interface RemoteShellConfig extends RemoteShellOptions { binding: BoundSandbox; }
export type ShellErrorCode = 'invalid_request' | 'unsupported_policy' | 'session_mismatch' | 'aborted' | 'disposed' |
  'preparation_unknown' | 'outcome_unknown' | 'output_incomplete' | 'termination_unknown' | 'runner_failed';
type RemoteState = 'unknown' | 'running' | 'stopped';
type StopCause = 'timeout' | 'abort' | 'kill' | 'dispose';

/** Safe evidence only; never retain command/env/stdin, SDK diagnostic causes or credentials. */
export class RemoteShellError extends Error {
  constructor(
    readonly code: ShellErrorCode,
    message: string,
    readonly sandboxId?: string,
    readonly commandId?: string,
    readonly remoteState: RemoteState = 'unknown',
    readonly timedOut = false,
    readonly aborted = false,
  ) { super(message); this.name = 'RemoteShellError'; }
}
function positive(value: number, field: string, ceiling = MAX_TIMER_MS): number {
  if (!Number.isSafeInteger(value) || value <= 0 || value > ceiling) {
    throw new RemoteShellError('invalid_request', `${field} must be a positive safe integer no greater than ${ceiling}.`);
  }
  return value;
}
function envFor(spec: Pick<ShellExecSpec, 'env' | 'dshEnv'>): Record<string, string> {
  const env: Record<string, string> = Object.create(null) as Record<string, string>;
  for (const [key, value] of Object.entries(spec.env ?? {})) {
    if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(key) || typeof value !== 'string' || value.includes('\0')) {
      throw new RemoteShellError('invalid_request', 'Invalid ordinary environment entry.');
    }
    if (!key.startsWith('DSH_')) env[key] = value;
  }
  for (const [key, value] of Object.entries(spec.dshEnv ?? {})) {
    if (!/^DSH_[A-Za-z0-9_]+$/.test(key) || typeof value !== 'string' || value.includes('\0')) {
      throw new RemoteShellError('invalid_request', 'Invalid managed environment entry.');
    }
    env[key] = value;
  }
  return env;
}
interface OwnedRun { handle: ShellExecution; dispose(): Promise<void>; }

/** Remote-only bash service; its route is the immutable binding supplied at composition. */
export class RemoteShell extends ShellExecutor {
  readonly #binding: BoundSandbox;
  readonly #transport: SdkTransport;
  readonly #options: Required<RemoteShellOptions>;
  readonly #runs = new Set<OwnedRun>();
  #disposed = false;
  #disposing: Promise<void> | undefined;

  constructor(ctx: Context, config: RemoteShellConfig) {
    super(ctx);
    this.#binding = config.binding;
    this.#transport = new SdkTransport(config.binding.sandbox);
    this.#options = Object.freeze({
      timeoutMs: positive(config.timeoutMs ?? 120_000, 'timeoutMs'),
      maxTimeoutMs: positive(config.maxTimeoutMs ?? 600_000, 'maxTimeoutMs'),
      maxOutputBytes: positive(config.maxOutputBytes ?? 64_000, 'maxOutputBytes', MAX_CAPTURE_BYTES),
      maxStdinBytes: positive(config.maxStdinBytes ?? 1024 * 1024, 'maxStdinBytes', MAX_CAPTURE_BYTES),
      settlementTimeoutMs: positive(config.settlementTimeoutMs ?? 3_000, 'settlementTimeoutMs'),
    });
    ctx.effect(() => () => this.dispose(), 'remote shell execution ownership');
  }

  // Cordis invokes services through proxies; lexical receivers preserve private routing fields.
  resolve = (request: ShellExecRequest): ShellExecSpec => {
    const workdir = request.workdir ?? this.#binding.descriptor.remoteCwd;
    if (typeof request.command !== 'string' || !request.command.trim() || request.command.includes('\0') ||
      !workdir.startsWith('/') || /[\x00-\x1f\x7f]/.test(workdir)) {
      throw new RemoteShellError('invalid_request', 'A command without NUL and an absolute Linux working directory are required.');
    }
    const policy = request.sandboxPolicy;
    if (policy && policy.mode !== 'danger-full-access') {
      throw new RemoteShellError('unsupported_policy', 'OpenSandbox does not enforce this narrower file-effect policy.');
    }
    if (policy?.sessionId !== undefined && String(policy.sessionId) !== this.#binding.descriptor.sessionId) {
      throw new RemoteShellError('session_mismatch', 'The execution policy session does not match the bound session.');
    }
    const onExpiry = request.onExpiry ?? 'kill';
    if (onExpiry !== 'none' && onExpiry !== 'kill') throw new RemoteShellError('invalid_request', 'Invalid expiry policy.');
    if (request.stdin !== undefined && (typeof request.stdin !== 'string' || Buffer.byteLength(request.stdin, 'utf8') > this.#options.maxStdinBytes)) {
      throw new RemoteShellError('invalid_request', 'Finite stdin exceeds the configured byte budget.');
    }
    envFor(request);
    return {
      ...request, workdir, onExpiry,
      timeoutMs: Math.min(positive(request.timeoutMs ?? this.#options.timeoutMs, 'timeoutMs'), this.#options.maxTimeoutMs),
      stdoutMaxBytes: positive(request.stdoutMaxBytes ?? this.#options.maxOutputBytes, 'stdoutMaxBytes', MAX_CAPTURE_BYTES),
      sandboxPolicy: policy ? { ...policy } : undefined,
      ...(request.env ? { env: { ...request.env } } : {}),
      ...(request.dshEnv ? { dshEnv: { ...request.dshEnv } } : {}),
    };
  };

  #assertOpen(): void {
    if (this.#disposed) throw new RemoteShellError('disposed', 'Remote shell composition is disposed.');
    if (this.#binding.state !== 'open') throw new BindingClosedError(this.#binding.descriptor.sandboxId);
  }

  execute = async (input: ShellExecSpec): Promise<ShellExecution> => {
    this.#assertOpen();
    const spec = this.resolve(input);
    if (spec.signal?.aborted) throw new RemoteShellError('aborted', 'Remote command cancelled before launch.');
    const stdout = new OutputBuffer(spec.stdoutMaxBytes);
    const stderr = new OutputBuffer(this.#options.maxOutputBytes);
    const streamAbort = new AbortController();
    const startedAt = Date.now();
    let resolveDone!: () => void;
    const done = new Promise<void>(resolve => { resolveDone = resolve; });
    let resolvePrepared!: () => void;
    const prepared = new Promise<void>(resolve => { resolvePrepared = resolve; });
    let resultPromise: Promise<ShellRunResult> | undefined;
    let failure: RemoteShellError | undefined;
    let cause: StopCause | undefined;
    let settled = false;
    let launched = false;
    let commandId: string | undefined;
    let remoteState: RemoteState = 'unknown';
    let stagingDirectory: string | undefined;
    let stagingSafe = false;
    let preparationFinished = false;
    let cleanupStarted = false;
    let interruptRequested = false;
    let stopObserving = false;
    let deadline: ReturnType<typeof setTimeout> | undefined;
    let stopDeadline: ReturnType<typeof setTimeout> | undefined;
    let statusTimer: ReturnType<typeof setTimeout> | undefined;
    const classify = () => ({ timedOut: cause === 'timeout', aborted: cause === 'abort' });
    const safeError = (code: ShellErrorCode, message: string) => new RemoteShellError(code, message,
      this.#binding.descriptor.sandboxId, commandId, remoteState, classify().timedOut, classify().aborted);
    const clearLocalTimers = () => {
      clearTimeout(deadline); clearTimeout(stopDeadline); clearTimeout(statusTimer);
      stopObserving = false;
      // A failed local observation does not relinquish explicit remote stop ownership.
      if (cause || !launched || remoteState === 'stopped') spec.signal?.removeEventListener('abort', onAbort);
    };
    const cleanup = () => {
      if (this.#binding.state === 'killed') { clearLocalTimers(); spec.signal?.removeEventListener('abort', onAbort); this.#runs.delete(owned); return; }
      if ((!launched && preparationFinished && (stagingSafe || !stagingDirectory)) || remoteState === 'stopped') {
        spec.signal?.removeEventListener('abort', onAbort);
        this.#runs.delete(owned);
        if (stagingDirectory && !cleanupStarted && this.#binding.state === 'open') {
          cleanupStarted = true;
          // Known stopped/no-launch is necessary before deleting any input artifact.
          void this.#transport.deleteDirectories([stagingDirectory]).catch(() => undefined);
        }
      }
    };
    const settle = (exitCode: number | null = null, error?: RemoteShellError) => {
      if (settled) { clearLocalTimers(); cleanup(); return; }
      settled = true;
      failure = error;
      handle.status = cause || error ? 'killed' : 'completed';
      handle.exitCode = exitCode;
      if (error) stderr.append(`${stderr.collected().text.endsWith('\n') ? '' : '\n'}remote shell: ${error.message}`);
      clearLocalTimers(); resolveDone(); resolvePrepared(); cleanup();
    };
    const checkStatus = async () => {
      if (!commandId || this.#binding.state !== 'open') return;
      let status: CommandStatus;
      try { status = await this.#transport.getCommandStatus(commandId); }
      catch { return; }
      if (status.id !== undefined && status.id !== commandId) return;
      if (status.running === false) {
        remoteState = 'stopped';
        if (cause) {
          stdout.markIncomplete(); stderr.markIncomplete();
          settle(typeof status.exitCode === 'number' ? status.exitCode : null);
        }
        cleanup();
      } else if (status.running === true) {
        remoteState = 'running';
        if (cause && stopObserving) statusTimer = setTimeout(() => { void checkStatus(); }, Math.min(100, this.#options.settlementTimeoutMs));
      }
    };
    const interrupt = () => {
      if (!commandId || interruptRequested || remoteState === 'stopped' || this.#binding.state !== 'open') return;
      interruptRequested = true;
      // Status is independent: an unresponsive interrupt request cannot block observation.
      void this.#transport.interrupt(commandId).catch(() => undefined);
      void checkStatus();
    };
    const stop = (nextCause: StopCause): boolean => {
      if (cause || (settled && (!launched || remoteState === 'stopped'))) return false;
      cause = nextCause;
      stopObserving = true;
      spec.signal?.removeEventListener('abort', onAbort);
      clearTimeout(deadline); clearTimeout(stopDeadline); clearTimeout(statusTimer);
      streamAbort.abort();
      if (!launched) { settle(); return true; }
      interrupt();
      stopDeadline = setTimeout(() => {
        stdout.markIncomplete(); stderr.markIncomplete();
        settle(null, commandId
          ? safeError('termination_unknown', 'Remote command termination is unconfirmed.')
          : safeError('outcome_unknown', 'Remote command execution outcome is unknown. No retry was attempted.'));
      }, this.#options.settlementTimeoutMs);
      return true;
    };
    const onAbort = () => { stop('abort'); };
    const handle: ShellExecution = {
      status: 'running', exitCode: null, signal: null, done,
      observed: { stdout: { readFrom: offset => stdout.read(offset) }, stderr: { readFrom: offset => stderr.read(offset) } },
      readOutput: () => {
        const out = stdout.readDelta(); const err = stderr.readDelta();
        const separator = out.text && !out.text.endsWith('\n') ? '\n' : '';
        return { delta: out.text + (err.text ? `${separator}[stderr]\n${err.text}` : ''), lossy: out.lossy || err.lossy };
      },
      kill: () => stop('kill'),
      result: () => {
        resultPromise ??= done.then(() => {
          if (failure) throw failure;
          return { exitCode: handle.exitCode, signal: handle.signal, ...classify(), timeoutMs: spec.timeoutMs,
            stdout: stdout.collected(), stderr: stderr.collected() };
        });
        return resultPromise;
      },
    };
    const owned: OwnedRun = { handle, dispose: async () => {
      if (!stop('dispose') && remoteState !== 'stopped' && launched) { cause ??= 'dispose'; interrupt(); }
      await done;
      if (this.#binding.state === 'killed') { cleanup(); return; }
      if (!launched) {
        if (!preparationFinished || (stagingDirectory && !stagingSafe)) {
          throw safeError('preparation_unknown', 'Remote input preparation remains unconfirmed; artifacts were retained.');
        }
        return;
      }
      if (remoteState !== 'stopped') {
        let timer: ReturnType<typeof setTimeout> | undefined;
        try {
          await Promise.race([checkStatus(), new Promise<void>(resolve => {
            timer = setTimeout(resolve, this.#options.settlementTimeoutMs);
          })]);
        } finally { clearTimeout(timer); }
      }
      if (remoteState !== 'stopped') throw safeError('termination_unknown', 'Remote command termination is unconfirmed after composition disposal.');
    } };
    this.#runs.add(owned);
    spec.signal?.addEventListener('abort', onAbort, { once: true });
    if (spec.signal?.aborted) onAbort();
    if (spec.onExpiry === 'kill' && !cause) deadline = setTimeout(() => { stop('timeout'); }, spec.timeoutMs);

    const observe = async (argv: string[], options: RunCommandOpts) => {
      const iterator = this.#transport.runStream(argv, options, streamAbort.signal)[Symbol.asyncIterator]();
      try {
        for (;;) {
          const item = await iterator.next();
          if (item.done) break;
          const event: ServerStreamEvent = item.value;
          if (event.type === 'init' && event.text && !commandId) {
            commandId = event.text;
            if (cause) interrupt();
          } else if (!settled && event.type === 'stdout') stdout.append(event.text ?? '');
          else if (!settled && event.type === 'stderr') stderr.append(event.text ?? '');
          else if (event.type === 'error') {
            // Pinned execd drains both streams and uses error as the terminal foreground event.
            const value = event.error?.evalue ?? event.error?.value;
            const name = event.error?.ename ?? event.error?.name;
            remoteState = 'stopped';
            if (name === 'CommandExecError' && typeof value === 'string' && /^-?\d+$/.test(value.trim()) && Number.isSafeInteger(Number(value))) {
              settle(Number(value));
            } else {
              settle(null, safeError('runner_failed', 'Remote command runner failed to report an exit outcome.'));
            }
            void iterator.return?.().catch(() => undefined);
            return;
          } else if (event.type === 'execution_complete') {
            remoteState = 'stopped';
            settle(0);
            void iterator.return?.().catch(() => undefined);
            return;
          }
        }
      } catch { /* SDK failures cannot expose raw diagnostic data. */ }
      if (settled) { cleanup(); return; }
      if (cause) { interrupt(); return; }
      stdout.markIncomplete(); stderr.markIncomplete();
      // Bound this independent wait too; getCommandStatus can hang after headers.
      if (commandId) {
        stopDeadline = setTimeout(() => { settle(null, safeError('output_incomplete', 'Remote command output observation is incomplete.')); }, this.#options.settlementTimeoutMs);
        await checkStatus();
        if (!settled) settle(null, safeError('output_incomplete', 'Remote command output observation is incomplete.'));
      } else settle(null, safeError('outcome_unknown', 'Remote command execution outcome is unknown. No retry was attempted.'));
    };
    const prepare = async () => {
      try {
        if (spec.stdin !== undefined && !cause) {
          stagingDirectory = `/tmp/opensandbox-dsh-stdin-${randomUUID()}`;
          // Public SDK modes are octal-digit numbers; execd parses their decimal text in base 8.
          await this.#transport.createDirectories([{ path: stagingDirectory, mode: 700 }]);
          stagingSafe = true;
          if (!cause) {
            this.#assertOpen(); stagingSafe = false;
            await this.#transport.writeBytes(`${stagingDirectory}/stdin`, Buffer.from(spec.stdin, 'utf8'), { mode: 600 });
            stagingSafe = true;
          }
        }
        preparationFinished = true;
        if (cause) { cleanup(); return; }
        this.#assertOpen();
        const envs = envFor(spec);
        const argv = ['python3', '-c', WRAPPER, stagingDirectory ? `${stagingDirectory}/stdin` : '', spec.command, JSON.stringify(Object.keys(spec.dshEnv ?? {})), spec.workdir];
        const options: RunCommandOpts = { background: false, workingDirectory: '/', envs,
          ...(spec.onExpiry === 'kill' ? { timeoutSeconds: Math.max(0.001, (spec.timeoutMs - (Date.now() - startedAt)) / 1000) } : {}) };
        launched = true;
        void observe(argv, options).catch(() => {
          stdout.markIncomplete(); stderr.markIncomplete(); settle(null, safeError('outcome_unknown', 'Remote command execution outcome is unknown.'));
        });
        resolvePrepared();
      } catch {
        preparationFinished = true;
        if (!settled) settle(null, safeError('preparation_unknown', 'Remote stdin preparation outcome is unknown. Input artifacts were retained.'));
        cleanup();
      }
    };
    void prepare();
    await prepared;
    if (!launched && cause === 'abort') throw safeError('aborted', 'Remote command cancelled before publication.');
    if (!launched && failure) throw failure;
    return handle;
  };

  /** Composition-owned stop and bounded join; never deletes the bound sandbox. */
  dispose = (): Promise<void> => {
    if (this.#disposing) return this.#disposing;
    this.#disposed = true;
    const runs = [...this.#runs];
    this.#disposing = Promise.all(runs.map(run => run.dispose())).then(() => undefined);
    return this.#disposing;
  };
}
export default RemoteShell;
