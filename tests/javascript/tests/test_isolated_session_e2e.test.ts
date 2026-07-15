// Copyright 2026 Alibaba Group Holding Ltd.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

import { describe, it, expect, beforeAll, afterAll } from "vitest";
import { Sandbox } from "@alibaba-group/opensandbox";
import type { OutputMessage } from "@alibaba-group/opensandbox";
import { createConnectionConfig, getSandboxImage } from "./base_e2e.js";

describe("IsolatedSession E2E", () => {
  let sandbox: Sandbox;

  beforeAll(async () => {
    sandbox = await Sandbox.create({
      image: getSandboxImage(),
      connectionConfig: createConnectionConfig(),
      extensions: { "bootstrap.execd.isolation": "enable" },
    });

    const caps = await sandbox.isolation.capabilities();
    console.log(
      `Isolation capabilities: available=${caps.available} isolator=${caps.isolator} version=${caps.version} message=${caps.message}`
    );
    if (!caps.available) {
      throw new Error(`Isolation NOT available: ${caps.message ?? "unknown reason"}`);
    }
  }, 120_000);

  afterAll(async () => {
    if (sandbox) {
      await sandbox.kill();
      await sandbox.close();
    }
  });

  it("test_capabilities", async () => {
    const caps = await sandbox.isolation.capabilities();
    expect(caps.available).toBe(true);
  });

  it("test_session_lifecycle", async () => {
    const session = await sandbox.isolation.create({
      workspace: { path: "/tmp", mode: "rw" },
    });
    expect(session.sessionId).toBeTruthy();

    const state = await session.get();
    expect(state.status).toBe("active");

    await session.delete();
  });

  it("test_list_sessions", async () => {
    const sessionA = await sandbox.isolation.create({
      workspace: { path: "/tmp", mode: "rw" },
    });
    let sessionBDeleted = false;
    const sessionB = await sandbox.isolation.create({
      workspace: { path: "/tmp", mode: "rw" },
    });
    try {
      const sessions = await sandbox.isolation.list();
      const byId = new Map(sessions.map(s => [s.session_id, s]));

      expect(byId.has(sessionA.sessionId)).toBe(true);
      expect(byId.has(sessionB.sessionId)).toBe(true);
      expect(byId.get(sessionA.sessionId)?.status).toBe("active");
      expect(byId.get(sessionA.sessionId)?.created_at).toBeTruthy();

      // After deleting a session it should no longer be listed.
      await sessionB.delete();
      sessionBDeleted = true;
      const remaining = await sandbox.isolation.list();
      const remainingIds = remaining.map(s => s.session_id);
      expect(remainingIds).not.toContain(sessionB.sessionId);
    } finally {
      await sessionA.delete();
      if (!sessionBDeleted) {
        await sessionB.delete();
      }
    }
  });

  it("test_run_echo", async () => {
    const session = await sandbox.isolation.create({
      workspace: { path: "/tmp", mode: "rw" },
    });
    try {
      const exec = await session.run("echo hello-isolation");
      expect(exec.logs.stdout.map(m => m.text).join("")).toContain("hello-isolation");
    } finally {
      await session.delete();
    }
  });

  it("test_pid_isolation", async () => {
    const session = await sandbox.isolation.create({
      workspace: { path: "/tmp", mode: "rw" },
    });
    try {
      const exec = await session.run("echo $$");
      const pid = parseInt(exec.logs.stdout.map(m => m.text).join("").trim(), 10);
      expect(pid).toBeLessThanOrEqual(2);
    } finally {
      await session.delete();
    }
  });

  it("test_run_with_envs", async () => {
    const session = await sandbox.isolation.create({
      workspace: { path: "/tmp", mode: "rw" },
    });
    try {
      const exec = await session.run(
        "echo $MY_VAR",
        { envs: { MY_VAR: "test-value-42" } }
      );
      expect(exec.logs.stdout.map(m => m.text).join("")).toContain("test-value-42");
    } finally {
      await session.delete();
    }
  });

  it("test_session_state_persists", async () => {
    const session = await sandbox.isolation.create({
      workspace: { path: "/tmp", mode: "rw" },
    });
    try {
      await session.run("export PERSIST_VAR=abc123");
      const exec = await session.run("echo $PERSIST_VAR");
      expect(exec.logs.stdout.map(m => m.text).join("")).toContain("abc123");
    } finally {
      await session.delete();
    }
  });

  it("test_tmp_isolation", async () => {
    await sandbox.commands.run("mkdir -p /workspace");

    const sessionA = await sandbox.isolation.create({
      workspace: { path: "/workspace", mode: "rw" },
      profile: "strict",
    });
    const sessionB = await sandbox.isolation.create({
      workspace: { path: "/workspace", mode: "rw" },
      profile: "strict",
    });
    try {
      await sessionA.run("echo secret > /tmp/isolated_test_file.txt");
      const exec = await sessionB.run(
        "cat /tmp/isolated_test_file.txt 2>&1 || echo NOT_FOUND"
      );
      expect(
        exec.logs.stdout.map(m => m.text).join("").includes("NOT_FOUND") || exec.logs.stdout.map(m => m.text).join("").includes("No such file")
      ).toBe(true);
    } finally {
      await sessionA.delete();
      await sessionB.delete();
    }
  });

  it("test_run_with_handlers", async () => {
    const session = await sandbox.isolation.create({
      workspace: { path: "/tmp", mode: "rw" },
    });
    try {
      const collected: string[] = [];
      await session.run(
        "echo handler-test",
        undefined,
        {
          onStdout: (msg: OutputMessage) => {
            collected.push(msg.text);
          },
        }
      );
      expect(collected.join("")).toContain("handler-test");
    } finally {
      await session.delete();
    }
  });

  it("test_files_via_run", async () => {
    const session = await sandbox.isolation.create({
      workspace: { path: "/tmp", mode: "rw" },
    });
    try {
      await session.run("echo hello-from-sdk > /tmp/hello.txt");
      const exec = await session.run("cat /tmp/hello.txt");
      expect(exec.logs.stdout.map(m => m.text).join("")).toContain("hello-from-sdk");
    } finally {
      await session.delete();
    }
  });

  it("test_overlay_mode", async () => {
    const marker = "overlay_marker_file.txt";
    const session = await sandbox.isolation.create({
      workspace: { path: "/tmp", mode: "overlay" },
    });
    try {
      await session.run(`echo overlay-data > /tmp/${marker}`);
      const hostCheck = await sandbox.commands.run(
        `cat /tmp/${marker} 2>&1 || echo NOT_FOUND`
      );
      expect(
        hostCheck.logs.stdout.map(m => m.text).join("").includes("NOT_FOUND") || hostCheck.logs.stdout.map(m => m.text).join("").includes("No such file")
      ).toBe(true);
    } finally {
      await session.delete();
    }
  });

  // ---------------------------------------------------------------------------
  // RW filesystem API tests
  // ---------------------------------------------------------------------------

  it("test_rw_host_visible", async () => {
    const tag = `rw_host_${Date.now()}`;
    const filePath = `/tmp/${tag}.txt`;
    const session = await sandbox.isolation.create({
      workspace: { path: "/tmp", mode: "rw" },
    });
    try {
      await session.run(`echo ${tag} > ${filePath}`);
      const hostCheck = await sandbox.commands.run(`cat ${filePath}`);
      expect(hostCheck.logs.stdout.map(m => m.text).join("")).toContain(tag);
    } finally {
      await sandbox.commands.run(`rm -f ${filePath}`);
      await session.delete();
    }
  });

  it("test_rw_files_upload_download", async () => {
    const tag = `rw_updown_${Date.now()}`;
    const filePath = `/tmp/${tag}.txt`;
    const content = `hello-upload-${tag}`;
    const session = await sandbox.isolation.create({
      workspace: { path: "/tmp", mode: "rw" },
    });
    try {
      await session.files.writeFiles([{ path: filePath, data: content }]);
      const downloaded = await session.files.readFile(filePath, { encoding: "utf-8" });
      expect(downloaded).toContain(content);
    } finally {
      await session.delete();
    }
  });

  it("test_rw_files_info", async () => {
    const tag = `rw_info_${Date.now()}`;
    const filePath = `/tmp/${tag}.txt`;
    const session = await sandbox.isolation.create({
      workspace: { path: "/tmp", mode: "rw" },
    });
    try {
      await session.files.writeFiles([{ path: filePath, data: "info-test" }]);
      const info = await session.files.getFileInfo([filePath]);
      expect(info[filePath]).toBeTruthy();
      expect(info[filePath].path).toBe(filePath);
      expect(info[filePath].type).toBe("file");
    } finally {
      await session.delete();
    }
  });

  it("test_rw_files_search", async () => {
    const tag = `rw_search_${Date.now()}`;
    const filePath = `/tmp/${tag}.txt`;
    const session = await sandbox.isolation.create({
      workspace: { path: "/tmp", mode: "rw" },
    });
    try {
      await session.files.writeFiles([{ path: filePath, data: "search-content" }]);
      const results = await session.files.search({ path: "/tmp", pattern: `${tag}*` });
      const paths = results.map(r => r.path);
      expect(paths.some(p => p.includes(`${tag}.txt`))).toBe(true);
    } finally {
      await session.delete();
    }
  });

  it("test_rw_files_mkdir", async () => {
    const tag = `rw_mkdir_${Date.now()}`;
    const dirPath = `/tmp/${tag}_dir`;
    const session = await sandbox.isolation.create({
      workspace: { path: "/tmp", mode: "rw" },
    });
    try {
      await session.files.createDirectories([{ path: dirPath }]);
      const info = await session.files.getFileInfo([dirPath]);
      expect(info[dirPath]).toBeTruthy();
      expect(info[dirPath].type).toBe("directory");
    } finally {
      await session.delete();
    }
  });

  it("test_rw_files_delete", async () => {
    const tag = `rw_del_${Date.now()}`;
    const filePath = `/tmp/${tag}.txt`;
    const session = await sandbox.isolation.create({
      workspace: { path: "/tmp", mode: "rw" },
    });
    try {
      await session.files.writeFiles([{ path: filePath, data: "delete-me" }]);
      await session.files.deleteFiles([filePath]);
      const exec = await session.run(`cat ${filePath} 2>&1 || echo NOT_FOUND`);
      expect(exec.logs.stdout.map(m => m.text).join("")).toContain("NOT_FOUND");
    } finally {
      await session.delete();
    }
  });

  it("test_rw_files_move", async () => {
    const tag = `rw_move_${Date.now()}`;
    const srcPath = `/tmp/${tag}_src.txt`;
    const destPath = `/tmp/${tag}_dest.txt`;
    const session = await sandbox.isolation.create({
      workspace: { path: "/tmp", mode: "rw" },
    });
    try {
      await session.files.writeFiles([{ path: srcPath, data: "move-content" }]);
      await session.files.moveFiles([{ src: srcPath, dest: destPath }]);
      const downloaded = await session.files.readFile(destPath, { encoding: "utf-8" });
      expect(downloaded).toContain("move-content");
    } finally {
      await session.delete();
    }
  });

  it("test_rw_files_chmod", async () => {
    const tag = `rw_chmod_${Date.now()}`;
    const filePath = `/tmp/${tag}.txt`;
    const session = await sandbox.isolation.create({
      workspace: { path: "/tmp", mode: "rw" },
    });
    try {
      await session.files.writeFiles([{ path: filePath, data: "chmod-test" }]);
      await session.files.setPermissions([{ path: filePath, mode: 755 }]);
      const exec = await session.run(`stat -c '%a' ${filePath}`);
      expect(exec.logs.stdout.map(m => m.text).join("")).toContain("755");
    } finally {
      await session.delete();
    }
  });

  it("test_rw_files_replace", async () => {
    const tag = `rw_replace_${Date.now()}`;
    const filePath = `/tmp/${tag}.txt`;
    const session = await sandbox.isolation.create({
      workspace: { path: "/tmp", mode: "rw" },
    });
    try {
      await session.files.writeFiles([{ path: filePath, data: "old-value-here" }]);
      await session.files.replaceContents([
        { path: filePath, oldContent: "old-value", newContent: "new-value" },
      ]);
      const downloaded = await session.files.readFile(filePath, { encoding: "utf-8" });
      expect(downloaded).toContain("new-value");
      expect(downloaded).not.toContain("old-value");
    } finally {
      await session.delete();
    }
  });

  it("test_rw_files_list_directory", async () => {
    const tag = `rw_listdir_${Date.now()}`;
    const dirPath = `/tmp/${tag}_dir`;
    const filePath = `${dirPath}/child.txt`;
    const session = await sandbox.isolation.create({
      workspace: { path: "/tmp", mode: "rw" },
    });
    try {
      await session.files.createDirectories([{ path: dirPath }]);
      await session.files.writeFiles([{ path: filePath, data: "child-content" }]);
      const entries = await session.files.listDirectory({ path: dirPath, depth: 1 });
      const paths = entries.map(e => e.path);
      expect(paths.some(p => p.includes("child.txt"))).toBe(true);
    } finally {
      await session.delete();
    }
  });

  // ---------------------------------------------------------------------------
  // RO mode tests
  // ---------------------------------------------------------------------------

  it("test_ro_can_read_existing_files", async () => {
    const tag = `ro_read_${Date.now()}`;
    const filePath = `/tmp/${tag}.txt`;
    await sandbox.commands.run(`echo readable-${tag} > ${filePath}`);
    const session = await sandbox.isolation.create({
      workspace: { path: "/tmp", mode: "ro" },
    });
    try {
      const exec = await session.run(`cat ${filePath}`);
      expect(exec.logs.stdout.map(m => m.text).join("")).toContain(`readable-${tag}`);
    } finally {
      await sandbox.commands.run(`rm -f ${filePath}`);
      await session.delete();
    }
  });

  it("test_ro_cannot_write", async () => {
    const tag = `ro_write_${Date.now()}`;
    const filePath = `/tmp/${tag}.txt`;
    const session = await sandbox.isolation.create({
      workspace: { path: "/tmp", mode: "ro" },
    });
    try {
      const exec = await session.run(
        `echo test > ${filePath} 2>&1 || echo WRITE_FAILED`
      );
      const output = exec.logs.stdout.map(m => m.text).join("")
        + exec.logs.stderr.map(m => m.text).join("");
      expect(
        output.includes("WRITE_FAILED") ||
        output.includes("Read-only") ||
        output.includes("read-only") ||
        output.includes("Permission denied")
      ).toBe(true);
    } finally {
      await session.delete();
    }
  });

  it("test_ro_files_api_read", async () => {
    const tag = `ro_api_read_${Date.now()}`;
    const filePath = `/tmp/${tag}.txt`;
    await sandbox.commands.run(`echo api-readable-${tag} > ${filePath}`);
    const session = await sandbox.isolation.create({
      workspace: { path: "/tmp", mode: "ro" },
    });
    try {
      const content = await session.files.readFile(filePath, { encoding: "utf-8" });
      expect(content).toContain(`api-readable-${tag}`);
    } finally {
      await sandbox.commands.run(`rm -f ${filePath}`);
      await session.delete();
    }
  });

  it("test_ro_files_api_search", async () => {
    const tag = `ro_api_search_${Date.now()}`;
    const filePath = `/tmp/${tag}.txt`;
    await sandbox.commands.run(`echo searchable > ${filePath}`);
    const session = await sandbox.isolation.create({
      workspace: { path: "/tmp", mode: "ro" },
    });
    try {
      const results = await session.files.search({ path: "/tmp", pattern: `${tag}*` });
      const paths = results.map(r => r.path);
      expect(paths.some(p => p.includes(`${tag}.txt`))).toBe(true);
    } finally {
      await sandbox.commands.run(`rm -f ${filePath}`);
      await session.delete();
    }
  });

  it("test_ro_files_api_list_directory", async () => {
    const tag = `ro_api_listdir_${Date.now()}`;
    const dirPath = `/tmp/${tag}_dir`;
    await sandbox.commands.run(`mkdir -p ${dirPath} && echo content > ${dirPath}/file.txt`);
    const session = await sandbox.isolation.create({
      workspace: { path: "/tmp", mode: "ro" },
    });
    try {
      const entries = await session.files.listDirectory({ path: dirPath, depth: 1 });
      const paths = entries.map(e => e.path);
      expect(paths.some(p => p.includes("file.txt"))).toBe(true);
    } finally {
      await sandbox.commands.run(`rm -rf ${dirPath}`);
      await session.delete();
    }
  });

  // ---------------------------------------------------------------------------
  // Overlay mode tests (skip if not supported)
  // ---------------------------------------------------------------------------

  it("test_overlay_writes_not_visible_on_host", async () => {
    const caps = await sandbox.isolation.capabilities();
    const overlaySupported = caps.commit_supported || caps.diff_supported;
    if (!overlaySupported) return;

    const tag = `ovl_nohost_${Date.now()}`;
    const filePath = `/tmp/${tag}.txt`;
    const session = await sandbox.isolation.create({
      workspace: { path: "/tmp", mode: "overlay" },
    });
    try {
      await session.run(`echo overlay-secret > ${filePath}`);
      const hostCheck = await sandbox.commands.run(
        `cat ${filePath} 2>&1 || echo NOT_FOUND`
      );
      const out = hostCheck.logs.stdout.map(m => m.text).join("");
      expect(out.includes("NOT_FOUND") || out.includes("No such file")).toBe(true);
    } finally {
      await session.delete();
    }
  });

  it("test_overlay_can_read_host_files", async () => {
    const caps = await sandbox.isolation.capabilities();
    const overlaySupported = caps.commit_supported || caps.diff_supported;
    if (!overlaySupported) return;

    const tag = `ovl_readhost_${Date.now()}`;
    const filePath = `/tmp/${tag}.txt`;
    await sandbox.commands.run(`echo host-content-${tag} > ${filePath}`);
    const session = await sandbox.isolation.create({
      workspace: { path: "/tmp", mode: "overlay" },
    });
    try {
      const exec = await session.run(`cat ${filePath}`);
      expect(exec.logs.stdout.map(m => m.text).join("")).toContain(`host-content-${tag}`);
    } finally {
      await sandbox.commands.run(`rm -f ${filePath}`);
      await session.delete();
    }
  });

  it("test_overlay_cow_does_not_mutate_host", async () => {
    const caps = await sandbox.isolation.capabilities();
    const overlaySupported = caps.commit_supported || caps.diff_supported;
    if (!overlaySupported) return;

    const tag = `ovl_cow_${Date.now()}`;
    const filePath = `/tmp/${tag}.txt`;
    await sandbox.commands.run(`echo original-${tag} > ${filePath}`);
    const session = await sandbox.isolation.create({
      workspace: { path: "/tmp", mode: "overlay" },
    });
    try {
      await session.run(`echo modified-${tag} > ${filePath}`);
      const sessionCheck = await session.run(`cat ${filePath}`);
      expect(sessionCheck.logs.stdout.map(m => m.text).join("")).toContain(`modified-${tag}`);
      const hostCheck = await sandbox.commands.run(`cat ${filePath}`);
      expect(hostCheck.logs.stdout.map(m => m.text).join("")).toContain(`original-${tag}`);
    } finally {
      await sandbox.commands.run(`rm -f ${filePath}`);
      await session.delete();
    }
  });

  it("test_overlay_files_api_upload_download", async () => {
    const caps = await sandbox.isolation.capabilities();
    const overlaySupported = caps.commit_supported || caps.diff_supported;
    if (!overlaySupported) return;

    const tag = `ovl_updown_${Date.now()}`;
    const filePath = `/tmp/${tag}.txt`;
    const content = `overlay-upload-${tag}`;
    const session = await sandbox.isolation.create({
      workspace: { path: "/tmp", mode: "overlay" },
    });
    try {
      await session.files.writeFiles([{ path: filePath, data: content }]);
      const downloaded = await session.files.readFile(filePath, { encoding: "utf-8" });
      expect(downloaded).toContain(content);
      // Verify not visible on host
      const hostCheck = await sandbox.commands.run(
        `cat ${filePath} 2>&1 || echo NOT_FOUND`
      );
      const hostOut = hostCheck.logs.stdout.map(m => m.text).join("");
      expect(hostOut.includes("NOT_FOUND") || hostOut.includes("No such file")).toBe(true);
    } finally {
      await session.delete();
    }
  });

  it("test_overlay_files_api_search", async () => {
    const caps = await sandbox.isolation.capabilities();
    const overlaySupported = caps.commit_supported || caps.diff_supported;
    if (!overlaySupported) return;

    const tag = `ovl_search_${Date.now()}`;
    const filePath = `/tmp/${tag}.txt`;
    const session = await sandbox.isolation.create({
      workspace: { path: "/tmp", mode: "overlay" },
    });
    try {
      await session.files.writeFiles([{ path: filePath, data: "overlay-search-data" }]);
      const results = await session.files.search({ path: "/tmp", pattern: `${tag}*` });
      const paths = results.map(r => r.path);
      expect(paths.some(p => p.includes(`${tag}.txt`))).toBe(true);
    } finally {
      await session.delete();
    }
  });

  it("test_overlay_files_api_delete", async () => {
    const caps = await sandbox.isolation.capabilities();
    const overlaySupported = caps.commit_supported || caps.diff_supported;
    if (!overlaySupported) return;

    const tag = `ovl_del_${Date.now()}`;
    const filePath = `/tmp/${tag}.txt`;
    const session = await sandbox.isolation.create({
      workspace: { path: "/tmp", mode: "overlay" },
    });
    try {
      await session.files.writeFiles([{ path: filePath, data: "to-delete" }]);
      await session.files.deleteFiles([filePath]);
      const exec = await session.run(`cat ${filePath} 2>&1 || echo NOT_FOUND`);
      expect(exec.logs.stdout.map(m => m.text).join("")).toContain("NOT_FOUND");
    } finally {
      await session.delete();
    }
  });

  it("test_overlay_files_api_move", async () => {
    const caps = await sandbox.isolation.capabilities();
    const overlaySupported = caps.commit_supported || caps.diff_supported;
    if (!overlaySupported) return;

    const tag = `ovl_move_${Date.now()}`;
    const srcPath = `/tmp/${tag}_src.txt`;
    const destPath = `/tmp/${tag}_dest.txt`;
    const session = await sandbox.isolation.create({
      workspace: { path: "/tmp", mode: "overlay" },
    });
    try {
      await session.files.writeFiles([{ path: srcPath, data: "overlay-move-data" }]);
      await session.files.moveFiles([{ src: srcPath, dest: destPath }]);
      const downloaded = await session.files.readFile(destPath, { encoding: "utf-8" });
      expect(downloaded).toContain("overlay-move-data");
    } finally {
      await session.delete();
    }
  });

  it("test_overlay_files_api_chmod", async () => {
    const caps = await sandbox.isolation.capabilities();
    const overlaySupported = caps.commit_supported || caps.diff_supported;
    if (!overlaySupported) return;

    const tag = `ovl_chmod_${Date.now()}`;
    const filePath = `/tmp/${tag}.txt`;
    const session = await sandbox.isolation.create({
      workspace: { path: "/tmp", mode: "overlay" },
    });
    try {
      await session.files.writeFiles([{ path: filePath, data: "chmod-overlay" }]);
      await session.files.setPermissions([{ path: filePath, mode: 755 }]);
      const exec = await session.run(`stat -c '%a' ${filePath}`);
      expect(exec.logs.stdout.map(m => m.text).join("")).toContain("755");
    } finally {
      await session.delete();
    }
  });

  it("test_overlay_files_api_replace", async () => {
    const caps = await sandbox.isolation.capabilities();
    const overlaySupported = caps.commit_supported || caps.diff_supported;
    if (!overlaySupported) return;

    const tag = `ovl_replace_${Date.now()}`;
    const filePath = `/tmp/${tag}.txt`;
    const session = await sandbox.isolation.create({
      workspace: { path: "/tmp", mode: "overlay" },
    });
    try {
      await session.files.writeFiles([{ path: filePath, data: "old-overlay-text" }]);
      await session.files.replaceContents([
        { path: filePath, oldContent: "old-overlay", newContent: "new-overlay" },
      ]);
      const downloaded = await session.files.readFile(filePath, { encoding: "utf-8" });
      expect(downloaded).toContain("new-overlay");
      expect(downloaded).not.toContain("old-overlay");
    } finally {
      await session.delete();
    }
  });

  it("test_overlay_files_api_list_directory", async () => {
    const caps = await sandbox.isolation.capabilities();
    const overlaySupported = caps.commit_supported || caps.diff_supported;
    if (!overlaySupported) return;

    const tag = `ovl_listdir_${Date.now()}`;
    const dirPath = `/tmp/${tag}_dir`;
    const filePath = `${dirPath}/overlay_child.txt`;
    const session = await sandbox.isolation.create({
      workspace: { path: "/tmp", mode: "overlay" },
    });
    try {
      await session.files.createDirectories([{ path: dirPath }]);
      await session.files.writeFiles([{ path: filePath, data: "overlay-child" }]);
      const entries = await session.files.listDirectory({ path: dirPath, depth: 1 });
      const paths = entries.map(e => e.path);
      expect(paths.some(p => p.includes("overlay_child.txt"))).toBe(true);
    } finally {
      await session.delete();
    }
  });

  // ── runOnce / withSession convenience API tests ──────────────────

  it("test_runOnce", async () => {
    const result = await sandbox.isolation.runOnce("echo runonce-e2e", "/tmp", {
      workspaceMode: "rw",
    });
    expect(result.logs.stdout.some(m => m.text.includes("runonce-e2e"))).toBe(true);
  });

  it("test_runOnce_with_envs", async () => {
    const result = await sandbox.isolation.runOnce("echo $E2E_RUN_ONCE", "/tmp", {
      workspaceMode: "rw",
      runOpts: { envs: { E2E_RUN_ONCE: "js-value" } },
    });
    expect(result.logs.stdout.some(m => m.text.includes("js-value"))).toBe(true);
  });

  it("test_withSession", async () => {
    const output = await sandbox.isolation.withSession(
      { workspace: { path: "/tmp", mode: "rw" } },
      async (session) => {
        await session.run("export WS_VAR=with-session-js");
        const r = await session.run("echo $WS_VAR");
        return r.logs.stdout.map(m => m.text).join("");
      },
    );
    expect(output).toContain("with-session-js");
  });

  it("test_withSession_multi_run", async () => {
    const output = await sandbox.isolation.withSession(
      { workspace: { path: "/tmp", mode: "rw" } },
      async (session) => {
        await session.run("echo step1 > /tmp/ws_test.txt");
        const r = await session.run("cat /tmp/ws_test.txt");
        return r.logs.stdout.map(m => m.text).join("");
      },
    );
    expect(output).toContain("step1");
  });

  // Bind mount tests (explicit source->dest binds)

  it("test_bind_read_write_host_visible", async () => {
    const ts = Date.now();
    // Source must be within the execd writable allowlist (e.g. /data).
    const srcDir = `/data/bind_rw_${ts}`;
    const dest = "/mnt/bind_rw";
    const fileName = "from_sandbox.txt";
    const content = "bind-rw-visible-on-host";

    // Create the source dir and the destination mount point (bwrap binds onto
    // an existing dir; it cannot create one under the read-only root).
    await sandbox.commands.run(`mkdir -p ${srcDir} ${dest}`);

    const session = await sandbox.isolation.create({
      workspace: { path: "/tmp", mode: "rw" },
      binds: [{ source: srcDir, dest }],
    });
    try {
      const exec = await session.run(
        `echo -n ${content} > ${dest}/${fileName} && cat ${dest}/${fileName}`
      );
      expect(exec.logs.stdout.map(m => m.text).join("")).toContain(content);

      const hostCheck = await sandbox.commands.run(`cat ${srcDir}/${fileName}`);
      expect(hostCheck.logs.stdout.map(m => m.text).join("")).toContain(content);
    } finally {
      await session.delete();
      await sandbox.commands.run(`rm -rf ${srcDir}`);
    }
  });

  it("test_bind_illegal_rejected", async () => {
    await expect(
      sandbox.isolation.create({
        workspace: { path: "/tmp", mode: "rw" },
        // /etc is not in the writable allowlist.
        binds: [{ source: "/etc", dest: "/mnt/etc" }],
      })
    ).rejects.toThrow();
  });

  it("test_bind_read_only_readable", async () => {
    const ts = Date.now();
    const srcDir = `/data/bind_ro_${ts}`;
    const dest = "/mnt/bind_ro";
    const fileName = "host_created.txt";
    const content = "bind-ro-host-content";

    await sandbox.commands.run(
      `mkdir -p ${srcDir} ${dest} && echo -n ${content} > ${srcDir}/${fileName}`
    );

    const session = await sandbox.isolation.create({
      workspace: { path: "/tmp", mode: "rw" },
      binds: [{ source: srcDir, dest, readonly: true }],
    });
    try {
      const exec = await session.run(`cat ${dest}/${fileName}`);
      expect(exec.logs.stdout.map(m => m.text).join("")).toContain(content);

      const write = await session.run(
        `echo x > ${dest}/newfile.txt 2>&1 || echo WRITE_FAILED`
      );
      const output = write.logs.stdout.map(m => m.text).join("")
        + write.logs.stderr.map(m => m.text).join("");
      expect(
        output.includes("WRITE_FAILED") ||
        output.includes("Read-only") ||
        output.includes("read-only") ||
        output.includes("Permission denied")
      ).toBe(true);
    } finally {
      await session.delete();
      await sandbox.commands.run(`rm -rf ${srcDir}`);
    }
  });
});
