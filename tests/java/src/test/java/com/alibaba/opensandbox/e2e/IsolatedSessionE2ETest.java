/*
 * Copyright 2026 Alibaba Group Holding Ltd.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package com.alibaba.opensandbox.e2e;

import static org.junit.jupiter.api.Assertions.*;

import com.alibaba.opensandbox.sandbox.Sandbox;
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxException;
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.Execution;
import com.alibaba.opensandbox.sandbox.domain.models.execd.filesystem.ContentReplaceEntry;
import com.alibaba.opensandbox.sandbox.domain.models.execd.filesystem.EntryInfo;
import com.alibaba.opensandbox.sandbox.domain.models.execd.filesystem.MoveEntry;
import com.alibaba.opensandbox.sandbox.domain.models.execd.filesystem.SearchEntry;
import com.alibaba.opensandbox.sandbox.domain.models.execd.filesystem.SetPermissionEntry;
import com.alibaba.opensandbox.sandbox.domain.models.execd.filesystem.WriteEntry;
import com.alibaba.opensandbox.sandbox.domain.models.execd.isolated.BindMount;
import com.alibaba.opensandbox.sandbox.domain.models.execd.isolated.CreateIsolatedSessionRequest;
import com.alibaba.opensandbox.sandbox.domain.models.execd.isolated.IsolatedCapabilities;
import com.alibaba.opensandbox.sandbox.domain.models.execd.isolated.IsolatedRunRequest;
import com.alibaba.opensandbox.sandbox.domain.models.execd.isolated.IsolatedWorkspaceSpec;
import com.alibaba.opensandbox.sandbox.domain.services.IsolationSession;
import java.time.Duration;
import java.util.List;
import java.util.Map;
import java.util.stream.Collectors;
import org.junit.jupiter.api.*;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

@TestInstance(TestInstance.Lifecycle.PER_CLASS)
@TestMethodOrder(MethodOrderer.OrderAnnotation.class)
public class IsolatedSessionE2ETest extends BaseE2ETest {

    private static final Logger log = LoggerFactory.getLogger(IsolatedSessionE2ETest.class);
    private Sandbox sandbox;

    private static String stdoutText(Execution exec) {
        return exec.getLogs().getStdout().stream()
                .map(m -> m.getText())
                .collect(Collectors.joining());
    }

    @BeforeAll
    void setup() {
        sandbox =
                Sandbox.builder()
                        .connectionConfig(sharedConnectionConfig)
                        .image(getSandboxImage())
                        .readyTimeout(Duration.ofMinutes(2))
                        .extensions(Map.of("bootstrap.execd.isolation", "enable"))
                        .build();

        IsolatedCapabilities caps = sandbox.isolation().capabilities();
        log.info(
                "Isolation capabilities: available={} isolator={} version={} message={}",
                caps.getAvailable(),
                caps.getIsolator(),
                caps.getVersion(),
                caps.getMessage());
        if (!caps.getAvailable()) {
            fail("Isolation NOT available: " + (caps.getMessage() != null ? caps.getMessage() : "unknown reason"));
        }
    }

    @AfterAll
    void tearDown() {
        if (sandbox != null) {
            sandbox.kill();
            sandbox.close();
        }
    }

    @Test
    @Order(1)
    void testCapabilities() {
        IsolatedCapabilities caps = sandbox.isolation().capabilities();
        assertTrue(caps.getAvailable());
    }

    @Test
    @Order(2)
    void testSessionLifecycle() {
        IsolationSession session =
                sandbox.isolation()
                        .create(new CreateIsolatedSessionRequest(
                                new IsolatedWorkspaceSpec("/tmp", "rw"),
                                "balanced", null, null, null, null, null, null, null, null));
        assertNotNull(session.getSessionId());

        var state = session.get();
        assertEquals("active", state.getStatus());

        session.delete();
    }

    @Test
    @Order(3)
    void testRunEcho() {
        IsolationSession session =
                sandbox.isolation()
                        .create(new CreateIsolatedSessionRequest(
                                new IsolatedWorkspaceSpec("/tmp", "rw"),
                                "balanced", null, null, null, null, null, null, null, null));
        try {
            Execution exec = session.run(new IsolatedRunRequest("echo hello-isolation", null, null));
            assertTrue(stdoutText(exec).contains("hello-isolation"));
        } finally {
            session.delete();
        }
    }

    @Test
    @Order(4)
    void testPidIsolation() {
        IsolationSession session =
                sandbox.isolation()
                        .create(new CreateIsolatedSessionRequest(
                                new IsolatedWorkspaceSpec("/tmp", "rw"),
                                "balanced", null, null, null, null, null, null, null, null));
        try {
            Execution exec = session.run(new IsolatedRunRequest("echo $$", null, null));
            int pid = Integer.parseInt(stdoutText(exec).trim());
            assertTrue(pid <= 2, "expected PID 1 or 2, got " + pid);
        } finally {
            session.delete();
        }
    }

    @Test
    @Order(5)
    void testRunWithEnvs() {
        IsolationSession session =
                sandbox.isolation()
                        .create(new CreateIsolatedSessionRequest(
                                new IsolatedWorkspaceSpec("/tmp", "rw"),
                                "balanced", null, null, null, null, null, null, null, null));
        try {
            Execution exec =
                    session.run(new IsolatedRunRequest(
                            "echo $MY_VAR",
                            Map.of("MY_VAR", "test-value-42"),
                            null));
            assertTrue(stdoutText(exec).contains("test-value-42"));
        } finally {
            session.delete();
        }
    }

    @Test
    @Order(6)
    void testSessionStatePersists() {
        IsolationSession session =
                sandbox.isolation()
                        .create(new CreateIsolatedSessionRequest(
                                new IsolatedWorkspaceSpec("/tmp", "rw"),
                                "balanced", null, null, null, null, null, null, null, null));
        try {
            session.run(new IsolatedRunRequest("export PERSIST_VAR=abc123", null, null));
            Execution exec = session.run(new IsolatedRunRequest("echo $PERSIST_VAR", null, null));
            assertTrue(stdoutText(exec).contains("abc123"));
        } finally {
            session.delete();
        }
    }

    @Test
    @Order(7)
    void testTmpIsolation() {
        sandbox.commands().run("mkdir -p /workspace");

        IsolationSession sessionA =
                sandbox.isolation()
                        .create(new CreateIsolatedSessionRequest(
                                new IsolatedWorkspaceSpec("/workspace", "rw"),
                                "strict", null, null, null, null, null, null, null, null));
        IsolationSession sessionB =
                sandbox.isolation()
                        .create(new CreateIsolatedSessionRequest(
                                new IsolatedWorkspaceSpec("/workspace", "rw"),
                                "strict", null, null, null, null, null, null, null, null));
        try {
            sessionA.run(new IsolatedRunRequest(
                    "echo secret > /tmp/isolated_test_file.txt", null, null));
            Execution exec = sessionB.run(new IsolatedRunRequest(
                    "cat /tmp/isolated_test_file.txt 2>&1 || echo NOT_FOUND", null, null));
            String text = stdoutText(exec);
            assertTrue(
                    text.contains("NOT_FOUND") || text.contains("No such file"),
                    "expected /tmp isolation, got: " + text);
        } finally {
            sessionA.delete();
            sessionB.delete();
        }
    }

    // ── RW mode: filesystem API tests ───────────────────────────────

    private IsolationSession createSession(String mode, String path) {
        return sandbox.isolation()
                .create(new CreateIsolatedSessionRequest(
                        new IsolatedWorkspaceSpec(path, mode),
                        "balanced", null, null, null, null, null, null, null, null));
    }

    private IsolationSession createSession(String mode) {
        return createSession(mode, "/tmp");
    }

    @Test
    @Order(8)
    void testRwFilesUploadDownload() {
        IsolationSession session = createSession("rw");
        try {
            String path = "/tmp/upload_rw_" + System.currentTimeMillis() + ".txt";
            session.getFiles().write(List.of(
                    WriteEntry.builder().path(path).data("rw upload").mode(644).build()));
            String content = session.getFiles().readFile(path);
            assertEquals("rw upload", content);
        } finally {
            session.delete();
        }
    }

    @Test
    @Order(9)
    void testRwFilesReadBytes() {
        IsolationSession session = createSession("rw");
        try {
            String path = "/tmp/bytes_rw_" + System.currentTimeMillis() + ".bin";
            byte[] data = new byte[]{0x00, 0x01, 0x02, (byte) 0xff};
            session.getFiles().write(List.of(
                    WriteEntry.builder().path(path).data(data).mode(644).build()));
            byte[] read = session.getFiles().readByteArray(path);
            assertArrayEquals(data, read);
        } finally {
            session.delete();
        }
    }

    @Test
    @Order(10)
    void testRwFilesInfo() {
        IsolationSession session = createSession("rw");
        try {
            String path = "/tmp/info_rw_" + System.currentTimeMillis() + ".txt";
            session.getFiles().write(List.of(
                    WriteEntry.builder().path(path).data("info").mode(644).build()));
            Map<String, EntryInfo> infoMap = session.getFiles().readFileInfo(List.of(path));
            assertTrue(infoMap.containsKey(path));
            assertEquals(4, infoMap.get(path).getSize());
            assertEquals(644, infoMap.get(path).getMode());
        } finally {
            session.delete();
        }
    }

    @Test
    @Order(11)
    void testRwFilesSearch() {
        IsolationSession session = createSession("rw");
        try {
            String prefix = "/tmp/search_rw_" + System.currentTimeMillis();
            session.run(new IsolatedRunRequest("mkdir -p " + prefix, null, null));
            session.getFiles().write(List.of(
                    WriteEntry.builder().path(prefix + "/a.txt").data("a").mode(644).build(),
                    WriteEntry.builder().path(prefix + "/b.txt").data("b").mode(644).build(),
                    WriteEntry.builder().path(prefix + "/c.log").data("c").mode(644).build()));
            List<EntryInfo> results = session.getFiles().search(
                    SearchEntry.builder().path(prefix).pattern("*.txt").build());
            assertEquals(2, results.size());
            List<String> paths = results.stream().map(EntryInfo::getPath).collect(Collectors.toList());
            assertTrue(paths.stream().anyMatch(p -> p.contains("a.txt")));
            assertTrue(paths.stream().anyMatch(p -> p.contains("b.txt")));
        } finally {
            session.delete();
        }
    }

    @Test
    @Order(12)
    void testRwFilesMkdir() {
        IsolationSession session = createSession("rw");
        try {
            String dir = "/tmp/mkdir_rw_" + System.currentTimeMillis();
            session.getFiles().createDirectories(List.of(
                    WriteEntry.builder().path(dir).mode(755).build()));
            Map<String, EntryInfo> infoMap = session.getFiles().readFileInfo(List.of(dir));
            assertTrue(infoMap.containsKey(dir));
        } finally {
            session.delete();
        }
    }

    @Test
    @Order(13)
    void testRwFilesDelete() {
        IsolationSession session = createSession("rw");
        try {
            String path = "/tmp/delete_rw_" + System.currentTimeMillis() + ".txt";
            session.getFiles().write(List.of(
                    WriteEntry.builder().path(path).data("del").mode(644).build()));
            session.getFiles().deleteFiles(List.of(path));
            assertThrows(SandboxException.class,
                    () -> session.getFiles().readFileInfo(List.of(path)));
        } finally {
            session.delete();
        }
    }

    @Test
    @Order(14)
    void testRwFilesMove() {
        IsolationSession session = createSession("rw");
        try {
            String src = "/tmp/mv_rw_src_" + System.currentTimeMillis() + ".txt";
            String dst = "/tmp/mv_rw_dst_" + System.currentTimeMillis() + ".txt";
            session.getFiles().write(List.of(
                    WriteEntry.builder().path(src).data("move").mode(644).build()));
            session.getFiles().moveFiles(List.of(
                    MoveEntry.builder().src(src).dest(dst).build()));
            assertEquals("move", session.getFiles().readFile(dst));
        } finally {
            session.delete();
        }
    }

    @Test
    @Order(15)
    void testRwFilesChmod() {
        IsolationSession session = createSession("rw");
        try {
            String path = "/tmp/chmod_rw_" + System.currentTimeMillis() + ".txt";
            session.getFiles().write(List.of(
                    WriteEntry.builder().path(path).data("ch").mode(644).build()));
            session.getFiles().setPermissions(List.of(
                    SetPermissionEntry.builder().path(path).mode(755).build()));
            Map<String, EntryInfo> infoMap = session.getFiles().readFileInfo(List.of(path));
            assertEquals(755, infoMap.get(path).getMode());
        } finally {
            session.delete();
        }
    }

    @Test
    @Order(16)
    void testRwFilesReplace() {
        IsolationSession session = createSession("rw");
        try {
            String path = "/tmp/replace_rw_" + System.currentTimeMillis() + ".txt";
            session.getFiles().write(List.of(
                    WriteEntry.builder().path(path).data("hello old world").mode(644).build()));
            session.getFiles().replaceContents(List.of(
                    ContentReplaceEntry.builder()
                            .path(path).oldContent("old").newContent("new").build()));
            String content = session.getFiles().readFile(path);
            assertTrue(content.contains("new"));
            assertFalse(content.contains("old"));
        } finally {
            session.delete();
        }
    }

    @Test
    @Order(17)
    void testRwFilesListDirectory() {
        IsolationSession session = createSession("rw");
        try {
            String prefix = "/tmp/listdir_rw_" + System.currentTimeMillis();
            session.run(new IsolatedRunRequest("mkdir -p " + prefix + "/sub", null, null));
            session.getFiles().write(List.of(
                    WriteEntry.builder().path(prefix + "/f1.txt").data("f1").mode(644).build(),
                    WriteEntry.builder().path(prefix + "/sub/f2.txt").data("f2").mode(644).build()));
            List<EntryInfo> entries = session.getFiles().listDirectory(prefix);
            List<String> names = entries.stream().map(EntryInfo::getPath).collect(Collectors.toList());
            assertTrue(names.stream().anyMatch(n -> n.contains("f1.txt")));
            assertTrue(names.stream().anyMatch(n -> n.contains("sub")));
        } finally {
            session.delete();
        }
    }

    // ── RO mode tests ───────────────────────────────────────────────

    @Test
    @Order(18)
    void testRoCanReadExistingFiles() {
        String marker = "ro_read_" + System.currentTimeMillis() + ".txt";
        sandbox.commands().run("echo ro-data > /tmp/" + marker);
        IsolationSession session = createSession("ro");
        try {
            Execution exec = session.run(
                    new IsolatedRunRequest("cat /tmp/" + marker, null, null));
            assertTrue(stdoutText(exec).contains("ro-data"));
        } finally {
            session.delete();
            sandbox.commands().run("rm -f /tmp/" + marker);
        }
    }

    @Test
    @Order(19)
    void testRoCannotWrite() {
        IsolationSession session = createSession("ro");
        try {
            Execution exec = session.run(new IsolatedRunRequest(
                    "echo fail > /tmp/ro_write_test.txt 2>&1; echo EXIT=$?", null, null));
            String text = stdoutText(exec);
            assertTrue(
                    text.contains("EXIT=1") || text.contains("Read-only") || text.contains("Permission denied"),
                    "expected write failure in RO mode, got: " + text);
        } finally {
            session.delete();
        }
    }

    @Test
    @Order(20)
    void testRoFilesApiRead() {
        String marker = "ro_api_" + System.currentTimeMillis() + ".txt";
        sandbox.commands().run("echo ro-api-data > /tmp/" + marker);
        IsolationSession session = createSession("ro");
        try {
            String content = session.getFiles().readFile("/tmp/" + marker);
            assertTrue(content.contains("ro-api-data"));
        } finally {
            session.delete();
            sandbox.commands().run("rm -f /tmp/" + marker);
        }
    }

    @Test
    @Order(21)
    void testRoFilesApiSearch() {
        String prefix = "/tmp/ro_search_" + System.currentTimeMillis();
        sandbox.commands().run("mkdir -p " + prefix + " && echo x > " + prefix + "/file.txt");
        IsolationSession session = createSession("ro");
        try {
            List<EntryInfo> results = session.getFiles().search(
                    SearchEntry.builder().path(prefix).pattern("*.txt").build());
            assertTrue(results.size() >= 1);
        } finally {
            session.delete();
            sandbox.commands().run("rm -rf " + prefix);
        }
    }

    @Test
    @Order(22)
    void testRoFilesApiListDirectory() {
        String prefix = "/tmp/ro_listdir_" + System.currentTimeMillis();
        sandbox.commands().run("mkdir -p " + prefix + " && echo x > " + prefix + "/f.txt");
        IsolationSession session = createSession("ro");
        try {
            List<EntryInfo> entries = session.getFiles().listDirectory(prefix);
            assertTrue(entries.size() >= 1);
        } finally {
            session.delete();
            sandbox.commands().run("rm -rf " + prefix);
        }
    }

    // ── Overlay mode tests ──────────────────────────────────────────

    private void requireOverlay() {
        IsolatedCapabilities caps = sandbox.isolation().capabilities();
        Assumptions.assumeTrue(
                caps.getCommitSupported() || caps.getDiffSupported(),
                "overlay mode not available in this environment");
    }

    @Test
    @Order(23)
    void testOverlayWritesNotVisibleOnHost() {
        requireOverlay();
        String marker = "overlay_invis_" + System.currentTimeMillis() + ".txt";
        IsolationSession session = createSession("overlay");
        try {
            session.run(new IsolatedRunRequest(
                    "echo overlay-data > /tmp/" + marker, null, null));
            Execution hostCheck = sandbox.commands().run(
                    "cat /tmp/" + marker + " 2>&1 || echo NOT_FOUND");
            String text = stdoutText(hostCheck);
            assertTrue(
                    text.contains("NOT_FOUND") || text.contains("No such file"),
                    "overlay write should not be visible on host, got: " + text);
        } finally {
            session.delete();
        }
    }

    @Test
    @Order(24)
    void testOverlayCanReadHostFiles() {
        requireOverlay();
        String marker = "overlay_lower_" + System.currentTimeMillis() + ".txt";
        sandbox.commands().run("echo lower-data > /tmp/" + marker);
        IsolationSession session = createSession("overlay");
        try {
            Execution exec = session.run(new IsolatedRunRequest(
                    "cat /tmp/" + marker, null, null));
            assertTrue(stdoutText(exec).contains("lower-data"));
        } finally {
            session.delete();
            sandbox.commands().run("rm -f /tmp/" + marker);
        }
    }

    @Test
    @Order(25)
    void testOverlayCowDoesNotMutateHost() {
        requireOverlay();
        String marker = "overlay_cow_" + System.currentTimeMillis() + ".txt";
        sandbox.commands().run("echo original > /tmp/" + marker);
        IsolationSession session = createSession("overlay");
        try {
            session.run(new IsolatedRunRequest(
                    "echo modified > /tmp/" + marker, null, null));
            Execution inSession = session.run(new IsolatedRunRequest(
                    "cat /tmp/" + marker, null, null));
            assertTrue(stdoutText(inSession).contains("modified"));
            Execution hostCheck = sandbox.commands().run("cat /tmp/" + marker);
            assertTrue(stdoutText(hostCheck).contains("original"));
        } finally {
            session.delete();
            sandbox.commands().run("rm -f /tmp/" + marker);
        }
    }

    @Test
    @Order(26)
    void testOverlayFilesApiUploadDownload() {
        requireOverlay();
        IsolationSession session = createSession("overlay");
        try {
            String path = "/tmp/ov_upload_" + System.currentTimeMillis() + ".txt";
            session.getFiles().write(List.of(
                    WriteEntry.builder().path(path).data("overlay file").mode(644).build()));
            String content = session.getFiles().readFile(path);
            assertEquals("overlay file", content);
            // Host should NOT see it
            Execution hostCheck = sandbox.commands().run(
                    "cat " + path + " 2>&1 || echo NOT_FOUND");
            String hostText = stdoutText(hostCheck);
            assertTrue(
                    hostText.contains("NOT_FOUND") || hostText.contains("No such file"),
                    "overlay file should not be visible on host, got: " + hostText);
        } finally {
            session.delete();
        }
    }

    @Test
    @Order(27)
    void testOverlayFilesApiSearch() {
        requireOverlay();
        String prefix = "/tmp/ov_search_" + System.currentTimeMillis();
        sandbox.commands().run("mkdir -p " + prefix + " && echo lower > " + prefix + "/lower.txt");
        IsolationSession session = createSession("overlay");
        try {
            session.getFiles().write(List.of(
                    WriteEntry.builder().path(prefix + "/upper.txt").data("upper").mode(644).build()));
            List<EntryInfo> results = session.getFiles().search(
                    SearchEntry.builder().path(prefix).pattern("*.txt").build());
            List<String> paths = results.stream().map(EntryInfo::getPath).collect(Collectors.toList());
            assertTrue(paths.stream().anyMatch(p -> p.contains("lower.txt")));
            assertTrue(paths.stream().anyMatch(p -> p.contains("upper.txt")));
        } finally {
            session.delete();
            sandbox.commands().run("rm -rf " + prefix);
        }
    }

    @Test
    @Order(28)
    void testOverlayFilesApiDelete() {
        requireOverlay();
        String prefix = "/tmp/ov_del_" + System.currentTimeMillis();
        sandbox.commands().run("mkdir -p " + prefix + " && echo x > " + prefix + "/target.txt");
        IsolationSession session = createSession("overlay");
        try {
            session.getFiles().deleteFiles(List.of(prefix + "/target.txt"));
            assertThrows(SandboxException.class,
                    () -> session.getFiles().readFileInfo(List.of(prefix + "/target.txt")));
            // Host file should be untouched
            Execution hostCheck = sandbox.commands().run("cat " + prefix + "/target.txt");
            assertTrue(stdoutText(hostCheck).contains("x"));
        } finally {
            session.delete();
            sandbox.commands().run("rm -rf " + prefix);
        }
    }

    @Test
    @Order(29)
    void testOverlayFilesApiMove() {
        requireOverlay();
        IsolationSession session = createSession("overlay");
        try {
            String src = "/tmp/ov_mv_src_" + System.currentTimeMillis() + ".txt";
            String dst = "/tmp/ov_mv_dst_" + System.currentTimeMillis() + ".txt";
            session.getFiles().write(List.of(
                    WriteEntry.builder().path(src).data("moveme").mode(644).build()));
            session.getFiles().moveFiles(List.of(
                    MoveEntry.builder().src(src).dest(dst).build()));
            assertEquals("moveme", session.getFiles().readFile(dst));
        } finally {
            session.delete();
        }
    }

    @Test
    @Order(30)
    void testOverlayFilesApiChmod() {
        requireOverlay();
        String marker = "ov_chmod_" + System.currentTimeMillis() + ".txt";
        sandbox.commands().run("echo ch > /tmp/" + marker + " && chmod 644 /tmp/" + marker);
        IsolationSession session = createSession("overlay");
        try {
            session.getFiles().setPermissions(List.of(
                    SetPermissionEntry.builder().path("/tmp/" + marker).mode(755).build()));
            Map<String, EntryInfo> infoMap =
                    session.getFiles().readFileInfo(List.of("/tmp/" + marker));
            assertEquals(755, infoMap.get("/tmp/" + marker).getMode());
            // Host should still be 644
            Execution hostCheck = sandbox.commands().run("stat -c %a /tmp/" + marker);
            assertTrue(stdoutText(hostCheck).contains("644"));
        } finally {
            session.delete();
            sandbox.commands().run("rm -f /tmp/" + marker);
        }
    }

    @Test
    @Order(31)
    void testOverlayFilesApiReplace() {
        requireOverlay();
        String marker = "ov_repl_" + System.currentTimeMillis() + ".txt";
        sandbox.commands().run("printf 'hello old world' > /tmp/" + marker);
        IsolationSession session = createSession("overlay");
        try {
            session.getFiles().replaceContents(List.of(
                    ContentReplaceEntry.builder()
                            .path("/tmp/" + marker)
                            .oldContent("old")
                            .newContent("new")
                            .build()));
            String content = session.getFiles().readFile("/tmp/" + marker);
            assertTrue(content.contains("new"));
            assertFalse(content.contains("old"));
            // Host unchanged
            Execution hostCheck = sandbox.commands().run("cat /tmp/" + marker);
            assertTrue(stdoutText(hostCheck).contains("old"));
        } finally {
            session.delete();
            sandbox.commands().run("rm -f /tmp/" + marker);
        }
    }

    @Test
    @Order(32)
    void testOverlayFilesApiListDirectory() {
        requireOverlay();
        String prefix = "/tmp/ov_ls_" + System.currentTimeMillis();
        sandbox.commands().run("mkdir -p " + prefix + " && echo l > " + prefix + "/lower.txt");
        IsolationSession session = createSession("overlay");
        try {
            session.getFiles().write(List.of(
                    WriteEntry.builder().path(prefix + "/upper.txt").data("u").mode(644).build()));
            List<EntryInfo> entries = session.getFiles().listDirectory(prefix);
            List<String> names = entries.stream().map(EntryInfo::getPath).collect(Collectors.toList());
            assertTrue(names.stream().anyMatch(n -> n.contains("lower.txt")));
            assertTrue(names.stream().anyMatch(n -> n.contains("upper.txt")));
        } finally {
            session.delete();
            sandbox.commands().run("rm -rf " + prefix);
        }
    }

    // ── run_once / withSession convenience API tests ─────────────────

    @Test
    @Order(33)
    void testRunOnce() {
        Execution exec = sandbox.isolation()
                .runOnce("echo runonce-e2e", "/tmp", "rw", null, null, null, null, null);
        assertTrue(stdoutText(exec).contains("runonce-e2e"));
    }

    @Test
    @Order(34)
    void testRunOnceWithEnvs() {
        Execution exec = sandbox.isolation()
                .runOnce(
                        "echo $E2E_RUN_ONCE",
                        "/tmp",
                        "rw",
                        Map.of("E2E_RUN_ONCE", "kt-value"),
                        null,
                        null,
                        null,
                        null);
        assertTrue(stdoutText(exec).contains("kt-value"));
    }

    @Test
    @Order(35)
    void testWithSession() {
        String output = sandbox.isolation().withSession(
                new CreateIsolatedSessionRequest(
                        new IsolatedWorkspaceSpec("/tmp", "rw"),
                        "balanced", null, null, null, null, null, null, null, null),
                session -> {
                    session.run(new IsolatedRunRequest("export WS_VAR=with-session-kt", null, null));
                    Execution exec = session.run(new IsolatedRunRequest("echo $WS_VAR", null, null));
                    return stdoutText(exec);
                });
        assertTrue(output.contains("with-session-kt"));
    }

    @Test
    @Order(36)
    void testWithSessionMultiRun() {
        String output = sandbox.isolation().withSession(
                new CreateIsolatedSessionRequest(
                        new IsolatedWorkspaceSpec("/tmp", "rw"),
                        "balanced", null, null, null, null, null, null, null, null),
                session -> {
                    session.run(new IsolatedRunRequest("echo step1 > /tmp/ws_test_kt.txt", null, null));
                    Execution exec = session.run(new IsolatedRunRequest("cat /tmp/ws_test_kt.txt", null, null));
                    return stdoutText(exec);
                });
        assertTrue(output.contains("step1"));
    }

    // ── Bind mount tests (explicit source->dest binds) ─────────────────

    @Test
    @Order(37)
    void testBindReadWriteHostVisible() {
        long ts = System.currentTimeMillis();
        // Source must be within the execd writable allowlist (e.g. /data).
        String srcDir = "/data/bind_rw_" + ts;
        String dest = "/mnt/bind_rw";
        String fileName = "from_sandbox.txt";
        String content = "bind-rw-visible-on-host";

        // Create the source dir and the destination mount point (bwrap binds
        // onto an existing dir; it cannot create one under the read-only root).
        sandbox.commands().run("mkdir -p " + srcDir + " " + dest);

        IsolationSession session =
                sandbox.isolation()
                        .create(new CreateIsolatedSessionRequest(
                                new IsolatedWorkspaceSpec("/tmp", "rw"),
                                "balanced",
                                null,
                                List.of(new BindMount(srcDir, dest, null)),
                                null, null, null, null, null, null));
        try {
            Execution exec = session.run(new IsolatedRunRequest(
                    "echo -n " + content + " > " + dest + "/" + fileName
                            + " && cat " + dest + "/" + fileName,
                    null, null));
            assertTrue(stdoutText(exec).contains(content));

            Execution hostCheck = sandbox.commands().run("cat " + srcDir + "/" + fileName);
            assertTrue(stdoutText(hostCheck).contains(content));
        } finally {
            session.delete();
            sandbox.commands().run("rm -rf " + srcDir);
        }
    }

    @Test
    @Order(38)
    void testBindIllegalRejected() {
        assertThrows(
                SandboxException.class,
                () -> sandbox.isolation()
                        .create(new CreateIsolatedSessionRequest(
                                new IsolatedWorkspaceSpec("/tmp", "rw"),
                                "balanced",
                                null,
                                // /etc is not in the writable allowlist.
                                List.of(new BindMount("/etc", "/mnt/etc", null)),
                                null, null, null, null, null, null)));
    }

    @Test
    @Order(39)
    void testBindReadOnlyReadable() {
        long ts = System.currentTimeMillis();
        String srcDir = "/data/bind_ro_" + ts;
        String dest = "/mnt/bind_ro";
        String fileName = "host_created.txt";
        String content = "bind-ro-host-content";

        sandbox.commands().run(
                "mkdir -p " + srcDir + " " + dest + " && echo -n " + content + " > " + srcDir + "/" + fileName);

        IsolationSession session =
                sandbox.isolation()
                        .create(new CreateIsolatedSessionRequest(
                                new IsolatedWorkspaceSpec("/tmp", "rw"),
                                "balanced",
                                null,
                                List.of(new BindMount(srcDir, dest, true)),
                                null, null, null, null, null, null));
        try {
            Execution exec = session.run(new IsolatedRunRequest("cat " + dest + "/" + fileName, null, null));
            assertTrue(stdoutText(exec).contains(content));

            Execution write = session.run(new IsolatedRunRequest(
                    "echo x > " + dest + "/newfile.txt 2>&1; echo EXIT=$?", null, null));
            String text = stdoutText(write);
            assertTrue(
                    text.contains("EXIT=1") || text.contains("Read-only")
                            || text.contains("read-only") || text.contains("Permission denied"),
                    "expected write to fail through read-only bind, got: " + text);
        } finally {
            session.delete();
            sandbox.commands().run("rm -rf " + srcDir);
        }
    }
}
