# Copyright 2026 The OpenSandbox Authors
# SPDX-License-Identifier: Apache-2.0
"""Local temporary-filesystem tests only; never a remote deployment check."""
import base64
import importlib.util
import json
import os
from pathlib import Path
import signal
import stat
import subprocess
import sys
import tempfile
import time
import unittest
from unittest import mock

PACKAGE = Path(__file__).resolve().parents[1]
HELPER = PACKAGE / "remote" / "fs_helper.py"
DRIVER = PACKAGE / "tests" / "fixtures" / "helper_process.py"


def load_helper():
    spec = importlib.util.spec_from_file_location("fs_helper", HELPER)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class HelperTest(unittest.TestCase):
    def setUp(self):
        self.assertTrue(HELPER.is_file(), "remote/fs_helper.py must implement the approved protocol")
        self.helper = load_helper()
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)

    def call(self, operation, **args):
        return self.helper.dispatch({"protocol": 1, "operation": operation, "args": args})

    def result(self, operation, **args):
        response = self.call(operation, **args)
        self.assertEqual(response.get("protocol"), 1)
        self.assertTrue(response.get("ok"), response)
        return response["result"]

    def error(self, code, operation, **args):
        response = self.call(operation, **args)
        self.assertFalse(response["ok"], response)
        self.assertEqual(response["error"]["code"], code, response)
        return response

    def target(self, path):
        return self.result("resolve", path=str(path), cwd=str(self.root))

    def request_files(self, operation, **args):
        work = self.root / ("request-" + str(time.time_ns()))
        work.mkdir(mode=0o700)
        request, response = work / "request.json", work / "response.json"
        request.write_text(json.dumps({"protocol": 1, "operation": operation, "args": args}), encoding="utf-8")
        request.chmod(0o600)
        return request, response

    def start_paused(self, boundary, operation, **args):
        request, response = self.request_files(operation, **args)
        ready, gate = request.parent / "ready.json", request.parent / "gate"
        process = subprocess.Popen([sys.executable, str(DRIVER), str(HELPER), str(request), str(response), boundary, str(ready), str(gate)], stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        self.addCleanup(self.stop_process, process)
        deadline = time.monotonic() + 10
        while not ready.exists():
            if process.poll() is not None:
                out, err = process.communicate()
                self.fail(f"helper exited before boundary: {out!r} {err!r}")
            if time.monotonic() > deadline:
                self.fail("helper did not reach publication boundary")
            time.sleep(0.01)
        return process, response, gate, json.loads(ready.read_text())

    @staticmethod
    def stop_process(process):
        if process.poll() is None:
            process.kill()
        process.communicate(timeout=5)

    def finish(self, process, response):
        out, err = process.communicate(timeout=10)
        self.assertEqual(process.returncode, 0, (out, err))
        self.assertEqual(out, b"", "protocol data belongs in response file, not stdout")
        self.assertEqual(err, b"")
        return json.loads(response.read_text(encoding="utf-8"))

    def test_resolve_symlink_aliases_and_missing_parents_share_identity(self):
        (self.root / "real").mkdir()
        (self.root / "alias").symlink_to("real", target_is_directory=True)
        a = self.target("alias/new/deep/file.txt")
        b = self.target("real/new/deep/file.txt")
        self.assertEqual(a["path"], b["path"])
        self.assertEqual(a["displayPath"], str(self.root / "alias/new/deep/file.txt"))
        self.result("write-text", target=a, content="created")
        self.assertEqual(self.target("alias/new/deep/file.txt")["path"], a["path"])

    def test_saved_canonical_target_survives_alias_retarget(self):
        for name in ["one", "two"]:
            (self.root / name).mkdir()
            (self.root / name / "file").write_text(name)
        alias = self.root / "alias"
        alias.symlink_to("one", target_is_directory=True)
        saved = self.target("alias/file")
        alias.unlink()
        alias.symlink_to("two", target_is_directory=True)
        self.result("write-text", target=saved, content="saved")
        self.assertEqual((self.root / "one/file").read_text(), "saved")
        self.assertEqual((self.root / "two/file").read_text(), "two")

    def test_physical_parent_traversal_and_missing_traversal(self):
        (self.root / "real/child").mkdir(parents=True)
        (self.root / "alias").symlink_to("real/child", target_is_directory=True)
        self.assertEqual(self.target("alias/../file")["path"], str(self.root / "real/file"))
        self.error("FS_NOT_FOUND", "resolve", path="missing/../file", cwd=str(self.root))
        (self.root / "regular").write_text("x")
        self.error("FS_NOT_FOUND", "resolve", path="regular/file", cwd=str(self.root))
        self.error("FS_NOT_FOUND", "resolve", path="   ", cwd=str(self.root))

    def test_dangling_relative_absolute_and_parent_symlinks_keep_identity(self):
        for alias, destination in [("relative", "new/deep/file"), ("absolute", str(self.root / "other/deep/file")), ("parent", "third")]:
            link = self.root / alias
            link.symlink_to(destination, target_is_directory=alias == "parent")
            requested = alias + "/file" if alias == "parent" else alias
            target = self.target(requested)
            expected = str(self.root / "third/file") if alias == "parent" else str(self.root / destination)
            self.assertEqual(target["path"], expected)
            self.result("write-text", target=target, content="created", expected={"kind": "createIfAbsent"})
            self.assertTrue(link.is_symlink())
            self.assertEqual(Path(expected).read_bytes(), b"created")
            self.assertEqual(self.target(requested)["path"], target["path"])
            # Retargeting an alias must not change the already-resolved absent target.
            link.unlink()
            link.symlink_to("new-target", target_is_directory=alias == "parent")
            self.result("write-text", target=target, content="saved")
            self.assertEqual(Path(expected).read_bytes(), b"saved")

    def test_symlink_cycles_hop_limit_and_missing_link_parent_traversal(self):
        (self.root / "a").symlink_to("b")
        (self.root / "b").symlink_to("a")
        self.error("FS_IO_ERROR", "resolve", path="a", cwd=str(self.root))
        for number in range(42):
            (self.root / f"hop-{number}").symlink_to(f"hop-{number + 1}")
        self.error("FS_IO_ERROR", "resolve", path="hop-0", cwd=str(self.root))
        (self.root / "cross-missing").symlink_to("missing/../file")
        self.error("FS_NOT_FOUND", "resolve", path="cross-missing", cwd=str(self.root))

    def test_stat_lstat_and_absence(self):
        missing = self.target("missing")
        self.assertIsNone(self.result("stat", target=missing))
        self.assertIsNone(self.result("lstat", path="missing", cwd=str(self.root)))
        (self.root / "file").write_bytes(b"a")
        (self.root / "link").symlink_to("file")
        self.assertEqual(self.result("lstat", path="link", cwd=str(self.root))["type"], "symlink")
        self.assertEqual(self.result("stat", target=self.target("link"))["type"], "file")
        self.assertEqual(self.result("stat", target=self.target("file"))["size"], 1)

    def test_nonregular_and_missing_read_write_edit_errors(self):
        target = self.target("absent")
        for operation, options in [("read-text", {}), ("read-bytes", {"maxBytes": 10}), ("read-range", {"offset": 0, "length": 10}), ("list", {})]:
            self.error("FS_NOT_FOUND", operation, target=target, **options)
        self.error("FS_STALE_VERSION", "edit-text", target=target, edit={"oldString": "x", "newString": "y", "replaceAll": False})
        directory = self.target(".")
        self.error("FS_NOT_DIRECTORY", "list", target=self.target_file("file", b"a"))
        for operation, options in [("read-text", {}), ("read-bytes", {"maxBytes": 10}), ("read-range", {"offset": 0, "length": 10}), ("write-text", {"content": "x"}), ("edit-text", {"edit": {"oldString": "x", "newString": "y", "replaceAll": False}})]:
            self.error("FS_NOT_REGULAR_FILE", operation, target=directory, **options)
        os.mkfifo(self.root / "fifo")
        self.error("FS_NOT_REGULAR_FILE", "read-text", target=self.target("fifo"))

    def target_file(self, name, data):
        (self.root / name).write_bytes(data)
        return self.target(name)

    def test_strict_utf8_nul_and_binary_bytes(self):
        for name, data in [("invalid", b"\xff"), ("nul", b"a\0b"), ("late-nul", b"a" * 9000 + b"\0"), ("split-invalid", b"a" * 65535 + b"\xe2\x82")]:
            target = self.target_file(name, data)
            self.error("FS_NOT_TEXT", "read-text", target=target)
            self.error("FS_NOT_TEXT", "edit-text", target=target, edit={"oldString": "a", "newString": "b", "replaceAll": True})
            raw = self.result("read-bytes", target=target, maxBytes=len(data))
            self.assertEqual(base64.b64decode(raw["base64"], validate=True), data)
        data = ("a" * 65535 + "€\r\n你好").encode()
        text = self.result("read-text", target=self.target_file("valid", data))
        self.assertEqual(text["text"].encode(), data)
        self.assertEqual(text["version"], self.result("stat", target=self.target("valid"))["version"])

    def test_bounded_bytes_and_ranges(self):
        target = self.target_file("bytes", b"0123456789")
        self.error("FS_TOO_LARGE", "read-bytes", target=target, maxBytes=9)
        self.assertEqual(base64.b64decode(self.result("read-bytes", target=target, maxBytes=10)["base64"]), b"0123456789")
        for offset, length, expected in [(3, 4, b"3456"), (8, 8, b"89"), (99, 5, b""), (0, 0, b"")]:
            result = self.result("read-range", target=target, offset=offset, length=length)
            self.assertEqual(base64.b64decode(result["base64"]), expected)
            self.assertEqual(result["version"], self.result("stat", target=target)["version"])
        self.error("FS_TOO_LARGE", "read-range", target=target, offset=0, length=self.helper.MAX_DATA_BYTES + 1)
        for offset, length in [(-1, 1), (0, -1), (0.5, 1), (True, 1), (0, float("inf"))]:
            self.error("FS_IO_ERROR", "read-range", target=target, offset=offset, length=length)

    def test_large_text_and_oversized_edit_reject_without_truncation(self):
        target = self.target_file("large", b"x" * (self.helper.MAX_DATA_BYTES + 1))
        self.error("FS_TOO_LARGE", "read-text", target=target)
        self.error("FS_TOO_LARGE", "edit-text", target=target, edit={"oldString": "x", "newString": "y", "replaceAll": True})
        small = self.target_file("small", b"xxx")
        self.error("FS_TOO_LARGE", "edit-text", target=small, edit={"oldString": "x", "newString": "y" * self.helper.MAX_DATA_BYTES, "replaceAll": True})
        self.assertEqual((self.root / "small").read_bytes(), b"xxx")

    def test_read_version_is_from_opened_descriptor_and_detects_change(self):
        target = self.target_file("file", b"old")
        original_open = self.helper.os.open
        def replace_before_open(path, flags, *args, **kwargs):
            if path == target["path"]:
                replacement = self.root / "replacement"
                replacement.write_bytes(b"new")
                replacement.replace(self.root / "file")
            return original_open(path, flags, *args, **kwargs)
        with mock.patch.object(self.helper.os, "open", side_effect=replace_before_open):
            value = self.result("read-text", target=target)
        self.assertEqual(value["text"], "new")
        self.assertEqual(value["version"], self.result("stat", target=target)["version"])
        original_read = self.helper.os.read
        changed = False
        def grow_during_read(fd, size):
            nonlocal changed
            data = original_read(fd, size)
            if not changed:
                changed = True
                with open(self.root / "file", "ab") as writer:
                    writer.write(b"x")
            return data
        with mock.patch.object(self.helper.os, "read", side_effect=grow_during_read):
            self.error("FS_STALE_VERSION", "read-range", target=target, offset=0, length=2)

    def test_listing_is_sorted_metadata_only_and_bounded(self):
        self.target_file("z", b"\xff")
        self.target_file("a", b"a")
        (self.root / "sub").mkdir()
        (self.root / "link").symlink_to("a")
        listing = self.result("list", target=self.target("."))
        self.assertEqual([entry["name"] for entry in listing], ["a", "link", "sub", "z"])
        self.assertEqual(listing[1]["target"]["path"], str(self.root / "a"))
        self.assertNotIn("text", listing[-1])
        with mock.patch.object(self.helper, "MAX_LIST_ENTRIES", 2):
            self.error("FS_TOO_LARGE", "list", target=self.target("."))

    def test_write_mode_crlf_diff_basis_and_invalid_prior_basis(self):
        target = self.target_file("file", b"a\r\nb\r\n")
        (self.root / "file").chmod(0o640)
        result = self.result("write-text", target=target, content="c\r\nd\r\n")
        self.assertEqual(result["operation"], "update")
        self.assertEqual(result["before"], "a\nb\n")
        self.assertEqual(result["after"], "c\nd\n")
        self.assertEqual((self.root / "file").read_bytes(), b"c\r\nd\r\n")
        self.assertEqual(stat.S_IMODE((self.root / "file").stat().st_mode), 0o640)
        binary = self.target_file("binary", b"\xff\0")
        self.assertIsNone(self.result("write-text", target=binary, content="good")["before"])
        created = self.result("write-text", target=self.target("new"), content="new")
        self.assertEqual(created["operation"], "create")
        self.assertIsNone(created["before"])
        self.assertEqual(stat.S_IMODE((self.root / "new").stat().st_mode), 0o600)

    def test_literal_edit_ambiguity_empty_not_found_and_crlf(self):
        target = self.target_file("file", b"a\r\na\r\n")
        for old, code in [("a", "FS_AMBIGUOUS_EDIT"), ("missing", "FS_EDIT_NOT_FOUND"), ("", "FS_EDIT_NOT_FOUND")]:
            self.error(code, "edit-text", target=target, edit={"oldString": old, "newString": "b", "replaceAll": False})
        edited = self.result("edit-text", target=target, edit={"oldString": "a\r\n", "newString": "b\r\n", "replaceAll": True})
        self.assertEqual(edited["before"], "a\na\n")
        self.assertEqual(edited["after"], "b\nb\n")
        self.assertEqual((self.root / "file").read_bytes(), b"b\r\nb\r\n")

    def test_crlf_restoration_normalizes_new_pair_at_replacement_boundary(self):
        target = self.target_file("file", b"a\r\nb\r\n")
        result = self.result("edit-text", target=target, edit={"oldString": "a", "newString": "x\r", "replaceAll": False})
        self.assertEqual(result["before"], "a\nb\n")
        self.assertEqual(result["after"], "x\r\nb\n", "preserve dsh result-time edit basis")
        self.assertEqual((self.root / "file").read_bytes(), b"x\r\nb\r\n")

    def test_lock_close_fault_preserves_publication_and_closes_both_descriptors(self):
        target = self.target_file("file", b"before")
        original_close = self.helper.os.close
        attempted = []
        def close(fd):
            path = os.readlink(f"/proc/self/fd/{fd}")
            original_close(fd)
            if self.helper.LOCK_DIRECTORY in path:
                attempted.append(path)
                raise OSError(5, "simulated lock cleanup I/O failure after actual close")
        with mock.patch.object(self.helper.os, "close", side_effect=close):
            response = self.call("write-text", target=target, content="after")
        self.assertEqual(len(attempted), 2, "both lock and directory need a cleanup attempt")
        self.assertTrue(response["ok"], response)
        self.assertEqual((self.root / "file").read_bytes(), b"after")
        self.assertEqual(response["result"]["version"], self.result("stat", target=target)["version"])

    def test_lock_close_fault_preserves_primary_prepublication_error(self):
        target = self.target_file("file", b"before")
        original_close = self.helper.os.close
        attempted = []
        def close(fd):
            path = os.readlink(f"/proc/self/fd/{fd}")
            original_close(fd)
            if self.helper.LOCK_DIRECTORY in path:
                attempted.append(path)
                raise OSError(5, "simulated lock cleanup I/O failure after actual close")
        with mock.patch.object(self.helper.os, "close", side_effect=close):
            response = self.call("write-text", target=target, content="after", expected={"kind": "replaceIfVersion", "version": "stale"})
        self.assertEqual(len(attempted), 2)
        self.assertEqual(response["error"]["code"], "FS_STALE_VERSION", "cleanup must not override the primary guard failure")
        self.assertEqual((self.root / "file").read_bytes(), b"before")

    def test_create_if_absent_returns_post_cleanup_version(self):
        target = self.target("new")
        result = self.result("write-text", target=target, content="created", expected={"kind": "createIfAbsent"})
        self.assertEqual(result["version"], self.result("stat", target=target)["version"])
        self.result("edit-text", target=target, expected={"version": result["version"]}, edit={"oldString": "created", "newString": "edited", "replaceAll": False})

    def test_post_publication_probe_failure_cannot_report_rollback(self):
        target = self.target_file("file", b"before")
        original_replace = self.helper.os.replace
        original_probe = self.helper._probe
        published = False
        def replace(source, destination):
            nonlocal published
            original_replace(source, destination)
            published = True
        def probe(path, nofollow=False):
            if published and path == target["path"]:
                raise PermissionError(13, "simulated post-publication observation loss")
            return original_probe(path, nofollow)
        with mock.patch.object(self.helper.os, "replace", side_effect=replace), mock.patch.object(self.helper, "_probe", side_effect=probe):
            result = self.result("write-text", target=target, content="after")
        self.assertEqual(result["version"], self.result("stat", target=target)["version"])
        self.assertEqual((self.root / "file").read_bytes(), b"after")

    def test_response_collision_prevents_mutation(self):
        target = self.target_file("file", b"before")
        request, response = self.request_files("write-text", target=target, content="after")
        response.write_text("prior diagnostic")
        process = subprocess.run([sys.executable, str(HELPER), str(request), str(response)], capture_output=True, timeout=10)
        self.assertNotEqual(process.returncode, 0)
        self.assertEqual((self.root / "file").read_bytes(), b"before")
        self.assertEqual(response.read_text(), "prior diagnostic")

    def test_cancel_after_publication_does_not_change_success(self):
        target = self.target_file("file", b"before")
        original_replace = self.helper.os.replace
        def replace(source, destination):
            original_replace(source, destination)
            self.helper._cancelled = True
        with mock.patch.object(self.helper.os, "replace", side_effect=replace):
            result = self.result("write-text", target=target, content="after")
        self.helper._cancelled = False
        self.assertEqual(result["version"], self.result("stat", target=target)["version"])
        self.assertEqual((self.root / "file").read_bytes(), b"after")

    def test_newline_style_sample_matches_dsh_utf16_window(self):
        # Published dsh samples 4096 JavaScript UTF-16 code units, not codepoints.
        original = "😀" * 2048 + "\r\nA\r\nB\r\n"
        target = self.target_file("file", original.encode("utf-8"))
        self.result("edit-text", target=target, edit={"oldString": "A", "newString": "X", "replaceAll": False})
        self.assertEqual((self.root / "file").read_bytes(), original.replace("\r\n", "\n").replace("A", "X").encode("utf-8"))

    def test_stale_guard_precedes_literal_matching(self):
        target = self.target_file("file", b"a")
        version = self.result("stat", target=target)["version"]
        self.result("write-text", target=target, content="b")
        self.error("FS_STALE_VERSION", "edit-text", target=target, expected={"version": version}, edit={"oldString": "absent", "newString": "c", "replaceAll": False})
        self.error("FS_STALE_VERSION", "write-text", target=target, expected={"kind": "replaceIfVersion", "version": version}, content="c")
        self.error("FS_NOT_OBSERVED", "write-text", target=target, expected={"kind": "createIfAbsent"}, content="c")

    def test_protocol_is_strict_finite_bounded_and_data_only(self):
        requests = [None, [], {}, {"protocol": True, "operation": "stat", "args": {}}, {"protocol": 2, "operation": "resolve", "args": {}}, {"protocol": 1, "operation": "eval", "args": {"source": "1+1"}}, {"protocol": 1, "operation": "resolve", "args": {"path": "x", "cwd": str(self.root), "extra": 1}}, {"protocol": 1, "operation": "resolve", "args": {"path": "x", "cwd": "."}}, {"protocol": 1, "operation": "stat", "args": {"target": {"path": "relative", "displayPath": "x"}}}, {"protocol": 1, "operation": "resolve", "args": {"path": "\ud800", "cwd": str(self.root)}}]
        for request in requests:
            with self.subTest(request=request):
                response = self.helper.dispatch(request)
                self.assertFalse(response["ok"], response)
                self.assertEqual(response["error"]["code"], "FS_IO_ERROR")
        self.error("FS_TOO_LARGE", "write-text", target=self.target("file"), content="a" * (self.helper.MAX_DATA_BYTES + 1))
        self.error("FS_NOT_TEXT", "write-text", target=self.target("file"), content="a\0b")
        tricky = "$(touch pwned);'\n\"`x`"
        target = self.target(tricky)
        self.result("write-text", target=target, content=tricky)
        self.assertEqual((self.root / tricky).read_text(), tricky)
        self.assertFalse((self.root / "pwned").exists())

    def test_main_has_fixed_argv_private_bounded_response_and_rejects_json(self):
        request, response = self.request_files("resolve", path="x", cwd=str(self.root))
        proc = subprocess.run([sys.executable, str(HELPER), str(request), str(response)], capture_output=True, timeout=10)
        self.assertEqual(proc.returncode, 0, proc.stderr)
        self.assertEqual(proc.stdout, b"")
        self.assertTrue(json.loads(response.read_text())["ok"])
        self.assertEqual(stat.S_IMODE(response.stat().st_mode), 0o600)
        for raw, code in [(b'{"protocol":1,"protocol":2,"operation":"resolve","args":{}}', "FS_IO_ERROR"), (b'{"protocol":1,"operation":"read-range","args":{"length":NaN}}', "FS_IO_ERROR"), (b"\xff", "FS_IO_ERROR"), (b"{" * 6000, "FS_IO_ERROR"), (b" " * (self.helper.MAX_REQUEST_BYTES + 1), "FS_TOO_LARGE")]:
            request.write_bytes(raw)
            response.unlink()
            proc = subprocess.run([sys.executable, str(HELPER), str(request), str(response)], capture_output=True, timeout=10)
            self.assertEqual(proc.returncode, 0, proc.stderr)
            parsed = json.loads(response.read_text())
            self.assertFalse(parsed["ok"])
            self.assertEqual(parsed["error"]["code"], code)
            self.assertLessEqual(response.stat().st_size, self.helper.MAX_RESPONSE_BYTES)
        bad_argv = subprocess.run([sys.executable, str(HELPER), "only-one-path"], capture_output=True, timeout=10)
        self.assertEqual(bad_argv.returncode, 2)

    def test_response_destination_is_not_followed_or_overwritten(self):
        request, response = self.request_files("resolve", path="x", cwd=str(self.root))
        sentinel = self.root / "sentinel"
        sentinel.write_text("keep")
        response.symlink_to(sentinel)
        proc = subprocess.run([sys.executable, str(HELPER), str(request), str(response)], capture_output=True, timeout=10)
        self.assertNotEqual(proc.returncode, 0)
        self.assertEqual(sentinel.read_text(), "keep")
        self.assertTrue(response.is_symlink())

    def test_cross_process_edits_share_canonical_lock_and_one_is_stale(self):
        (self.root / "actual").mkdir()
        target = self.target_file("actual/file", b"one")
        (self.root / "alias").symlink_to("actual", target_is_directory=True)
        alias = self.target("alias/file")
        version = self.result("stat", target=target)["version"]
        first, response1, gate, _ = self.start_paused("stage-synced", "edit-text", target=alias, expected={"version": version}, edit={"oldString": "one", "newString": "first", "replaceAll": False})
        request2, response2 = self.request_files("edit-text", target=target, expected={"version": version}, edit={"oldString": "one", "newString": "second", "replaceAll": False})
        second = subprocess.Popen([sys.executable, str(HELPER), str(request2), str(response2)], stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        self.addCleanup(self.stop_process, second)
        time.sleep(0.15)
        self.assertIsNone(second.poll(), "second helper must wait on the same canonical advisory lock")
        self.assertEqual(response2.read_bytes(), b"", "a reserved output is not a terminal response")
        gate.touch()
        self.assertTrue(self.finish(first, response1)["ok"])
        result2 = self.finish(second, response2)
        self.assertEqual(result2["error"]["code"], "FS_STALE_VERSION")
        self.assertEqual((self.root / "actual/file").read_bytes(), b"first")

    def test_cross_process_missing_parent_create_has_one_winner(self):
        (self.root / "actual").mkdir()
        (self.root / "alias").symlink_to("actual", target_is_directory=True)
        target = self.target("actual/new/deep/file")
        alias = self.target("alias/new/deep/file")
        first, response1, gate, _ = self.start_paused("stage-synced", "write-text", target=alias, content="first", expected={"kind": "createIfAbsent"})
        request2, response2 = self.request_files("write-text", target=target, content="second", expected={"kind": "createIfAbsent"})
        second = subprocess.Popen([sys.executable, str(HELPER), str(request2), str(response2)], stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        self.addCleanup(self.stop_process, second)
        time.sleep(0.15)
        self.assertIsNone(second.poll())
        gate.touch()
        self.assertTrue(self.finish(first, response1)["ok"])
        self.assertEqual(self.finish(second, response2)["error"]["code"], "FS_NOT_OBSERVED")
        self.assertEqual(Path(target["path"]).read_bytes(), b"first")

    def test_guarded_create_does_not_replace_external_racing_creator(self):
        target = self.target("file")
        process, response, gate, _ = self.start_paused("stage-synced", "write-text", target=target, content="helper", expected={"kind": "createIfAbsent"})
        (self.root / "file").write_bytes(b"external")
        gate.touch()
        self.assertEqual(self.finish(process, response)["error"]["code"], "FS_NOT_OBSERVED")
        self.assertEqual((self.root / "file").read_bytes(), b"external")

    def test_private_staging_fsync_mode_and_cancel_before_publication(self):
        target = self.target_file("file", b"before")
        (self.root / "file").chmod(0o644)
        process, response, gate, staging = self.start_paused("stage-synced", "write-text", target=target, content="after")
        directory, payload = Path(staging["stagingDir"]), Path(staging["tempPath"])
        self.assertEqual(directory.parent, self.root)
        self.assertEqual(stat.S_IMODE(directory.stat().st_mode), 0o700)
        self.assertEqual(stat.S_IMODE(payload.stat().st_mode), 0o600)
        self.assertEqual(payload.read_bytes(), b"after")
        self.assertEqual((self.root / "file").read_bytes(), b"before")
        process.send_signal(signal.SIGTERM)
        result = self.finish(process, response)
        self.assertEqual(result["error"]["code"], "FS_ABORTED")
        self.assertEqual((self.root / "file").read_bytes(), b"before")
        self.assertFalse(directory.exists())

    def test_lock_wait_is_cancellable_in_a_separate_process(self):
        target = self.target_file("file", b"before")
        first, response1, gate, _ = self.start_paused("stage-synced", "write-text", target=target, content="first")
        second, response2, _, state = self.start_paused("lock-wait", "write-text", target=target, content="second")
        self.assertTrue(state["waitingForLock"], "second process has attempted the real advisory lock")
        second.send_signal(signal.SIGTERM)
        self.assertEqual(self.finish(second, response2)["error"]["code"], "FS_ABORTED")
        self.assertEqual((self.root / "file").read_bytes(), b"before")
        gate.touch()
        self.assertTrue(self.finish(first, response1)["ok"])

    def test_lock_artifacts_are_private_even_with_restrictive_umask(self):
        target = self.target("file")
        previous = os.umask(0o777)
        try:
            self.result("write-text", target=target, content="one")
        finally:
            os.umask(previous)
        directory = self.root / self.helper.LOCK_DIRECTORY
        self.assertEqual(stat.S_IMODE(directory.stat().st_mode), 0o700)
        locks = list(directory.iterdir())
        self.assertEqual(len(locks), 1)
        self.assertEqual(stat.S_IMODE(locks[0].stat().st_mode), 0o600)
        self.result("write-text", target=target, content="two")

    def test_combined_path_cap_and_errno_translation(self):
        long = "x" * self.helper.MAX_PATH_BYTES
        self.error("FS_TOO_LARGE", "lstat", path=long, cwd=str(self.root))
        self.error("FS_TOO_LARGE", "resolve", path=long, cwd=str(self.root))
        target = self.target("file")
        with mock.patch.object(self.helper.os, "stat", side_effect=PermissionError(13, "denied")):
            self.error("FS_PERMISSION_DENIED", "stat", target=target)
        with mock.patch.object(self.helper.os, "stat", side_effect=OSError(5, "I/O")):
            self.error("FS_IO_ERROR", "stat", target=target)

    def test_opened_file_growth_and_path_replacement_reject_stale_reads(self):
        target = self.target_file("file", b"old")
        original_read = self.helper.os.read
        replaced = False
        def replace_during_read(fd, size):
            nonlocal replaced
            data = original_read(fd, size)
            if not replaced:
                replaced = True
                new = self.root / "new"
                new.write_bytes(b"new")
                new.replace(self.root / "file")
            return data
        with mock.patch.object(self.helper.os, "read", side_effect=replace_during_read):
            self.error("FS_STALE_VERSION", "read-text", target=target)
        original_fstat = self.helper.os.fstat
        grown = False
        def grow_after_fstat(fd):
            nonlocal grown
            info = original_fstat(fd)
            if not grown:
                grown = True
                with open(self.root / "file", "ab") as writer:
                    writer.write(b"12345")
            return info
        with mock.patch.object(self.helper.os, "fstat", side_effect=grow_after_fstat):
            self.error("FS_TOO_LARGE", "read-bytes", target=target, maxBytes=3)

    def test_cancel_marker_and_known_completion_cleanup(self):
        marker = self.root / "cancel"
        marker.touch()
        target = self.target("file")
        self.error("FS_ABORTED", "write-text", target=target, content="after", cancelPath=str(marker))
        self.assertFalse((self.root / "file").exists())
        marker.unlink()
        self.result("write-text", target=target, content="after", cancelPath=str(marker))
        self.assertEqual(list(self.root.glob("*.tmpdir")), [])
        self.assertEqual(list(self.root.glob(".*.tmpdir")), [])

    def test_unknown_killed_helper_retains_request_and_staging_diagnostics(self):
        target = self.target_file("file", b"before")
        process, response, gate, staging = self.start_paused("stage-synced", "write-text", target=target, content="after")
        process.kill()
        process.communicate(timeout=10)
        self.assertEqual(response.read_bytes(), b"", "an empty reserved response is no confirmed helper outcome")
        self.assertTrue(Path(staging["stagingDir"]).exists())
        self.assertTrue((response.parent / "request.json").exists())
        self.assertEqual((self.root / "file").read_bytes(), b"before")

    def test_unrelated_writer_is_outside_guarded_replacement_guarantee(self):
        target = self.target_file("file", b"original")
        version = self.result("stat", target=target)["version"]
        process, response, gate, _ = self.start_paused("stage-synced", "write-text", target=target, content="helper", expected={"kind": "replaceIfVersion", "version": version})
        # Ordinary shell/filesystem writers deliberately do not acquire helper locks.
        (self.root / "file").write_bytes(b"external-after-guard")
        gate.touch()
        self.assertTrue(self.finish(process, response)["ok"])
        self.assertEqual((self.root / "file").read_bytes(), b"helper")


if __name__ == "__main__":
    unittest.main()
