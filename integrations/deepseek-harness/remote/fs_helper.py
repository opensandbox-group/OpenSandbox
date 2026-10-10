#!/usr/bin/env python3
# Copyright 2026 The OpenSandbox Authors
# SPDX-License-Identifier: Apache-2.0
"""Linux sandbox filesystem helper, protocol v1, Python standard library only.

Invocation: python3 fs_helper.py REQUEST_FILE RESPONSE_FILE
Paths/content are JSON data, never commands. Advisory locks serialize cooperating
helper writers only: ordinary shell writers do not participate in this CAS.
Persistent per-parent lock files must not be unlinked while waiters may exist.
SIGTERM/SIGINT record cancellation intent; it is checked immediately before
publication. Publication followed by lost transport is still an unknown outcome.
"""
import base64
from collections import deque
from contextlib import contextmanager
import errno
import fcntl
import hashlib
import json
import os
import signal
import stat
import sys
import tempfile
import time

PROTOCOL = 1
MAX_DATA_BYTES = 1024 * 1024
MAX_REQUEST_BYTES = 8 * 1024 * 1024
MAX_RESPONSE_BYTES = 16 * 1024 * 1024
MAX_PATH_BYTES = 4096
MAX_LIST_ENTRIES = 4096
MAX_SYMLINK_HOPS = 40
MAX_SAFE_INTEGER = 2 ** 53 - 1
READ_CHUNK_BYTES = 64 * 1024
LOCK_DIRECTORY = ".opensandbox-dsh-locks-v1"
_cancelled = False


class FsFailure(Exception):
    def __init__(self, code, message):
        super().__init__(message)
        self.code = code


def _fail(code, message):
    raise FsFailure(code, message)


def _object(value, required, optional=()):
    if type(value) is not dict or not set(required) <= value.keys() or not value.keys() <= set(required) | set(optional):
        _fail("FS_IO_ERROR", "Invalid protocol object or fields.")
    return value


def _string(value, limit, *, text=False):
    if type(value) is not str:
        _fail("FS_IO_ERROR", "Expected a protocol string.")
    try:
        size = len(value.encode("utf-8", errors="strict"))
    except UnicodeError:
        _fail("FS_IO_ERROR", "Protocol strings must be valid Unicode.")
    if size > limit:
        _fail("FS_TOO_LARGE", "Protocol string exceeds its byte limit.")
    if "\0" in value:
        _fail("FS_NOT_TEXT" if text else "FS_IO_ERROR", "NUL is not allowed in this string.")
    return value


def _path(value, *, absolute=False):
    value = _string(value, MAX_PATH_BYTES)
    if absolute and not os.path.isabs(value):
        _fail("FS_IO_ERROR", "An absolute sandbox path is required.")
    return value


def _integer(value, limit=MAX_SAFE_INTEGER):
    if type(value) is not int or value < 0 or value > MAX_SAFE_INTEGER:
        _fail("FS_IO_ERROR", "Expected a non-negative safe integer.")
    if value > limit:
        _fail("FS_TOO_LARGE", "Requested byte count exceeds the helper limit.")
    return value


def _target(value):
    _object(value, ("path", "displayPath"))
    path = _path(value["path"], absolute=True)
    _path(value["displayPath"], absolute=True)
    if path != os.path.normpath(path):
        _fail("FS_IO_ERROR", "Target path must have a canonical absolute spelling.")
    return value


def _finite_tree(value, depth=0, budget=None):
    if budget is None:
        budget = [10000]
    budget[0] -= 1
    if depth > 16 or budget[0] < 0:
        _fail("FS_IO_ERROR", "Protocol structure exceeds its nesting or item limit.")
    if value is None or type(value) in (bool, int, str):
        return
    # Protocol v1 has integer numbers only. In particular NaN/Infinity are invalid.
    if type(value) is dict:
        for key, child in value.items():
            if type(key) is not str:
                _fail("FS_IO_ERROR", "Protocol object keys must be strings.")
            _finite_tree(child, depth + 1, budget)
        return
    if type(value) is list:
        for child in value:
            _finite_tree(child, depth + 1, budget)
        return
    _fail("FS_IO_ERROR", "Invalid protocol scalar.")


def _validate(request):
    _finite_tree(request)
    _object(request, ("protocol", "operation", "args"))
    if type(request["protocol"]) is not int or request["protocol"] != PROTOCOL:
        _fail("FS_IO_ERROR", "Unsupported filesystem helper protocol.")
    operation = request["operation"]
    if type(operation) is not str:
        _fail("FS_IO_ERROR", "Invalid filesystem operation.")
    args = request["args"]
    if operation in ("resolve", "lstat"):
        _object(args, ("path", "cwd"), ("cancelPath",))
        _path(args["path"])
        _path(args["cwd"], absolute=True)
    elif operation in ("stat", "list", "read-text"):
        _object(args, ("target",), ("cancelPath",))
        _target(args["target"])
    elif operation == "read-bytes":
        _object(args, ("target", "maxBytes"), ("cancelPath",))
        _target(args["target"])
        _integer(args["maxBytes"], MAX_DATA_BYTES)
    elif operation == "read-range":
        _object(args, ("target", "offset", "length"), ("cancelPath",))
        _target(args["target"])
        _integer(args["offset"])
        _integer(args["length"], MAX_DATA_BYTES)
        if args["offset"] + args["length"] > MAX_SAFE_INTEGER:
            _fail("FS_IO_ERROR", "Range end exceeds the safe integer limit.")
    elif operation in ("write-text", "edit-text"):
        required = ("target", "content") if operation == "write-text" else ("target", "edit")
        _object(args, required, ("expected", "cancelPath"))
        _target(args["target"])
        if operation == "write-text":
            _string(args["content"], MAX_DATA_BYTES, text=True)
            if "expected" in args:
                expected = _object(args["expected"], ("kind",), ("version",))
                if expected["kind"] == "createIfAbsent":
                    _object(expected, ("kind",))
                elif expected["kind"] == "replaceIfVersion":
                    _object(expected, ("kind", "version"))
                    _string(expected["version"], 256)
                else:
                    _fail("FS_IO_ERROR", "Invalid write intent.")
        else:
            edit = _object(args["edit"], ("oldString", "newString", "replaceAll"))
            _string(edit["oldString"], MAX_DATA_BYTES, text=True)
            _string(edit["newString"], MAX_DATA_BYTES, text=True)
            if type(edit["replaceAll"]) is not bool:
                _fail("FS_IO_ERROR", "replaceAll must be a boolean.")
            if "expected" in args:
                _object(args["expected"], ("version",))
                _string(args["expected"]["version"], 256)
    else:
        _fail("FS_IO_ERROR", "Unsupported filesystem operation.")
    if "cancelPath" in args:
        _path(args["cancelPath"], absolute=True)
    return operation, args


def _check_cancel(args):
    if _cancelled or ("cancelPath" in args and os.path.lexists(args["cancelPath"])):
        _fail("FS_ABORTED", "Filesystem operation aborted before publication.")


def _display_path(path, cwd):
    if not path.strip():
        _fail("FS_NOT_FOUND", "file_path must be a non-empty string.")
    raw = path if os.path.isabs(path) else cwd + "/" + path
    _path(raw, absolute=True)
    # Do not collapse physical symlink/../ traversal before asking the filesystem.
    return raw if ".." in raw.split("/") else os.path.normpath(raw)


def resolve_target(path, cwd):
    """Physically resolve links, preserving a missing suffix at its real ancestor.

    Dangling links are followed as path data rather than becoming alias-shaped
    targets. This deliberately improves the pinned local provider's ENOENT
    fallback, so later creation preserves the link and saved identity.
    """
    display = _display_path(path, cwd)
    pending = deque(display.split("/"))
    canonical = "/"
    missing = False
    hops = 0
    while pending:
        part = pending.popleft()
        if part in ("", "."):
            continue
        if part == "..":
            if missing:
                _fail("FS_NOT_FOUND", "Parent traversal crosses a missing directory.")
            canonical = os.path.dirname(canonical)
            continue
        candidate = os.path.join(canonical, part)
        _path(candidate, absolute=True)
        if missing:
            canonical = candidate
            continue
        try:
            info = os.lstat(candidate)
        except OSError as error:
            if error.errno == errno.ENOENT:
                canonical, missing = candidate, True
                continue
            if error.errno == errno.ENOTDIR:
                _fail("FS_NOT_FOUND", "A parent path segment is not a directory.")
            raise
        if stat.S_ISLNK(info.st_mode):
            hops += 1
            if hops > MAX_SYMLINK_HOPS:
                _fail("FS_IO_ERROR", "Symlink resolution exceeds its hop limit or has a cycle.")
            linked = _path(os.readlink(candidate))
            if os.path.isabs(linked):
                canonical = "/"
            pending.extendleft(reversed(linked.split("/")))
            continue
        if pending and not stat.S_ISDIR(info.st_mode):
            _fail("FS_NOT_FOUND", "A parent path segment is not a directory.")
        canonical = candidate
    return {"path": canonical, "displayPath": display}


def _version(info):
    return ":".join(str(value) for value in (info.st_dev, info.st_ino, info.st_size, info.st_mtime_ns, info.st_ctime_ns))


def _type(info, nofollow=False):
    if nofollow and stat.S_ISLNK(info.st_mode):
        return "symlink"
    if stat.S_ISREG(info.st_mode):
        return "file"
    if stat.S_ISDIR(info.st_mode):
        return "directory"
    return "other"


def _probe(path, nofollow=False):
    try:
        return os.lstat(path) if nofollow else os.stat(path)
    except OSError as error:
        if error.errno in (errno.ENOENT, errno.ENOTDIR):
            return None
        raise


def _metadata(info, nofollow=False):
    if info is None:
        return None
    kind = _type(info, nofollow)
    result = {"version": _version(info), "type": kind}
    if kind == "file" or nofollow:
        _integer(info.st_size)
        result["size"] = info.st_size
    return result


@contextmanager
def _open_regular(target, args):
    _check_cancel(args)
    info = _probe(target["path"])
    if info is None:
        _fail("FS_NOT_FOUND", "Cannot read absent file.")
    if not stat.S_ISREG(info.st_mode):
        _fail("FS_NOT_REGULAR_FILE", "Cannot read a non-regular file.")
    # Nonblocking avoids hanging if an external writer replaces it with a FIFO.
    try:
        fd = os.open(target["path"], os.O_RDONLY | os.O_NONBLOCK | os.O_NOFOLLOW)
    except OSError as error:
        if error.errno == errno.ELOOP:
            _fail("FS_NOT_REGULAR_FILE", "Canonical file path became a symlink.")
        raise
    try:
        opened = os.fstat(fd)
        if not stat.S_ISREG(opened.st_mode):
            _fail("FS_NOT_REGULAR_FILE", "Opened object is not a regular file.")
        _check_cancel(args)
        yield fd, opened
    finally:
        os.close(fd)


def _read_fd(fd, limit, args):
    chunks = []
    total = 0
    while total <= limit:
        _check_cancel(args)
        chunk = os.read(fd, min(READ_CHUNK_BYTES, limit + 1 - total))
        if not chunk:
            break
        chunks.append(chunk)
        total += len(chunk)
    _check_cancel(args)
    if total > limit:
        _fail("FS_TOO_LARGE", "File content exceeds the byte limit.")
    return b"".join(chunks)


def _stable_read(target, args, *, limit=MAX_DATA_BYTES, offset=None, length=None):
    with _open_regular(target, args) as (fd, opened):
        if offset is None:
            if opened.st_size > limit:
                _fail("FS_TOO_LARGE", "File size exceeds the byte limit.")
            data = _read_fd(fd, limit, args)
        else:
            os.lseek(fd, offset, os.SEEK_SET)
            chunks, total = [], 0
            while total < length:
                _check_cancel(args)
                chunk = os.read(fd, min(READ_CHUNK_BYTES, length - total))
                if not chunk:
                    break
                chunks.append(chunk)
                total += len(chunk)
            data = b"".join(chunks)
        _check_cancel(args)
        if _version(os.fstat(fd)) != _version(opened):
            _fail("FS_STALE_VERSION", "File changed during the read.")
        # Confirm this descriptor still represents the saved canonical path.
        current = _probe(target["path"])
        if current is None or _version(current) != _version(opened):
            _fail("FS_STALE_VERSION", "File was replaced during the read.")
        return data, _version(opened)


def _decode(data):
    if b"\0" in data:
        _fail("FS_NOT_TEXT", "Binary NUL is not valid text.")
    try:
        # TextDecoder in the published local provider ignores an initial UTF-8 BOM.
        return data.decode("utf-8-sig", errors="strict")
    except UnicodeError:
        _fail("FS_NOT_TEXT", "File is not valid UTF-8 text.")


def _list(target, args):
    info = _probe(target["path"])
    if info is None:
        _fail("FS_NOT_FOUND", "Cannot list absent directory.")
    if not stat.S_ISDIR(info.st_mode):
        _fail("FS_NOT_DIRECTORY", "Cannot list a non-directory.")
    names = []
    with os.scandir(target["path"]) as entries:
        for entry in entries:
            _check_cancel(args)
            if len(names) == MAX_LIST_ENTRIES:
                _fail("FS_TOO_LARGE", "Directory listing exceeds its entry limit.")
            _string(entry.name, MAX_PATH_BYTES)
            names.append(entry.name)
    result = []
    for name in sorted(names):
        _check_cancel(args)
        child = resolve_target(name, target["path"])
        child["displayPath"] = _display_path(name, target["displayPath"])
        _path(child["displayPath"], absolute=True)
        info = _probe(child["path"])
        metadata = _metadata(info) or {"type": "other"}
        result.append({"name": name, "target": child, **metadata})
    return result


@contextmanager
def _target_lock(target, args):
    _check_cancel(args)
    parent = os.path.dirname(target["path"])
    os.makedirs(parent, exist_ok=True)
    lock_directory = os.path.join(parent, LOCK_DIRECTORY)
    try:
        os.mkdir(lock_directory, 0o700)
        os.chmod(lock_directory, 0o700)
    except FileExistsError:
        pass
    directory_info = os.lstat(lock_directory)
    if not stat.S_ISDIR(directory_info.st_mode) or directory_info.st_uid != os.geteuid() or stat.S_IMODE(directory_info.st_mode) & 0o077:
        _fail("FS_IO_ERROR", "Cooperative lock directory is not private and owned.")
    directory_fd = os.open(lock_directory, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    fd = None
    try:
        name = hashlib.sha256(target["path"].encode("utf-8")).hexdigest() + ".lock"
        try:
            fd = os.open(name, os.O_RDWR | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600, dir_fd=directory_fd)
            os.fchmod(fd, 0o600)
        except FileExistsError:
            fd = os.open(name, os.O_RDWR | os.O_NOFOLLOW, dir_fd=directory_fd)
        lock_info = os.fstat(fd)
        if not stat.S_ISREG(lock_info.st_mode) or lock_info.st_uid != os.geteuid() or stat.S_IMODE(lock_info.st_mode) & 0o077:
            _fail("FS_IO_ERROR", "Cooperative lock file is not private and owned.")
        while True:
            _check_cancel(args)
            try:
                fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
                break
            except BlockingIOError:
                time.sleep(0.01)
        _check_cancel(args)
        yield
    finally:
        # Cleanup must preserve the body's known publication or primary failure.
        # Attempt each owned close once: an error can mean the fd was released,
        # so retrying risks closing a reused descriptor. Process exit releases
        # any remaining advisory lock; cleanup cannot imply mutation rollback.
        for owned_fd in (fd, directory_fd):
            if owned_fd is not None:
                try:
                    os.close(owned_fd)
                except OSError:
                    pass


def _normalize(content):
    return content.replace("\r\n", "\n")


def _edit(original, edit):
    before = _normalize(original)
    old, new = _normalize(edit["oldString"]), _normalize(edit["newString"])
    if not old:
        _fail("FS_EDIT_NOT_FOUND", "old_string must be a non-empty string.")
    count = before.count(old)
    if count == 0:
        _fail("FS_EDIT_NOT_FOUND", "old_string was not found in the file.")
    if count > 1 and not edit["replaceAll"]:
        _fail("FS_AMBIGUOUS_EDIT", "old_string matched multiple times; use a specific match or replaceAll.")
    # Bound allocation before multiplying a large replacement across many matches.
    size = len(before.encode("utf-8")) + count * (len(new.encode("utf-8")) - len(old.encode("utf-8")))
    if size > MAX_DATA_BYTES:
        _fail("FS_TOO_LARGE", "Edited content exceeds the byte limit.")
    after = before.replace(old, new)
    # Match JavaScript slice(0, 4096), including astral UTF-16 pairs.
    sample = original.encode("utf-16-le")[:8192].decode("utf-16-le", errors="ignore")
    crlf = sample.count("\r\n")
    lf = sample.count("\n") - crlf
    stored = _normalize(after).replace("\n", "\r\n") if crlf > lf else after
    _string(stored, MAX_DATA_BYTES, text=True)
    return before, after, stored


def _diff_basis(target, args, content):
    if len(content.encode("utf-8")) >= MAX_DATA_BYTES:
        return None
    try:
        data, _ = _stable_read(target, args)
        if len(data) >= MAX_DATA_BYTES:
            return None
        return _normalize(_decode(data))
    except FsFailure as error:
        if error.code == "FS_ABORTED":
            raise
        return None
    except OSError:
        return None


def _remove_staging(staging, payload):
    if os.path.lexists(payload):
        os.unlink(payload)
    os.rmdir(staging)


def _publish(target, content, mode, args, create_only):
    _check_cancel(args)
    parent = os.path.dirname(target["path"])
    staging = tempfile.mkdtemp(prefix=".opensandbox-dsh-", suffix=".tmpdir", dir=parent)
    payload = os.path.join(staging, "payload")
    fd = None
    committed = False
    try:
        os.chmod(staging, 0o700)
        fd = os.open(payload, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
        os.fchmod(fd, 0o600)
        data = content.encode("utf-8")
        written = 0
        while written < len(data):
            _check_cancel(args)
            count = os.write(fd, data[written:written + READ_CHUNK_BYTES])
            if count == 0:
                _fail("FS_IO_ERROR", "Staging write made no progress.")
            written += count
        os.fsync(fd)
        if mode is not None:
            os.fchmod(fd, mode)
            os.fsync(fd)
        _check_cancel(args)
        if create_only:
            try:
                os.link(payload, target["path"], follow_symlinks=False)
            except OSError as error:
                collision = _probe(target["path"], nofollow=True)
                if collision is not None and not stat.S_ISREG(collision.st_mode):
                    _fail("FS_NOT_REGULAR_FILE", "Create collided with a non-regular path entry.")
                if collision is not None or error.errno == errno.EEXIST:
                    _fail("FS_NOT_OBSERVED", "Cannot overwrite an existing file without reading it first.")
                raise
        else:
            os.replace(payload, target["path"])
        committed = True
        # Removing the staging hard link changes ctime on createIfAbsent. Do this
        # before taking the result token, and never probe a potentially replaced
        # path for the version of the inode this operation actually published.
        try:
            _remove_staging(staging, payload)
        except OSError:
            pass  # Successful publication survives private cleanup residue.
        try:
            version = _version(os.fstat(fd))
        except OSError:
            # Publication is known, but no freshness guard can be asserted.
            # This sentinel will safely reject a subsequent guarded mutation.
            version = "unavailable:" + hashlib.sha256(target["path"].encode()).hexdigest()
        # Publication already happened. Cancellation/cleanup must not turn it into
        # an alleged rollback. Directory fsync is best effort after this point.
        try:
            parent_fd = os.open(parent, os.O_RDONLY | os.O_DIRECTORY)
            try:
                os.fsync(parent_fd)
            finally:
                os.close(parent_fd)
        except OSError:
            pass
        return version
    finally:
        if fd is not None:
            try:
                os.close(fd)
            except OSError:
                if not committed:
                    raise
        # SIGKILL/process loss skips this block, retaining diagnostics. Known
        # failure before publish has a bounded, operation-owned cleanup scope.
        if not committed:
            try:
                _remove_staging(staging, payload)
            except OSError:
                _fail("FS_IO_ERROR", "Operation failed before publication and staging cleanup failed.")


def mutate(target, request):
    """Read, guard, match and publish under the canonical cross-process lock."""
    operation, args = request["operation"], request["args"]
    with _target_lock(target, args):
        existing = _probe(target["path"])
        if existing is not None and not stat.S_ISREG(existing.st_mode):
            _fail("FS_NOT_REGULAR_FILE", "Cannot mutate a non-regular file.")
        expected = args.get("expected")
        create_only = operation == "write-text" and expected is not None and expected["kind"] == "createIfAbsent"
        if operation == "edit-text":
            if existing is None or (expected is not None and _version(existing) != expected["version"]):
                _fail("FS_STALE_VERSION", "File changed since it was read.")
            data, _ = _stable_read(target, args)
            before, after, content = _edit(_decode(data), args["edit"])
        else:
            if expected is not None:
                if create_only and existing is not None:
                    _fail("FS_NOT_OBSERVED", "Cannot overwrite an existing file without reading it first.")
                if not create_only and (existing is None or _version(existing) != expected["version"]):
                    _fail("FS_STALE_VERSION", "File changed since it was read.")
            content = args["content"]
            before = _diff_basis(target, args, content) if existing is not None else None
            after = _normalize(content)
        mode = stat.S_IMODE(existing.st_mode) & 0o777 if existing is not None else None
        version = _publish(target, content, mode, args, create_only)
        result = {"version": version, "before": before, "after": after}
        if operation == "write-text":
            result["operation"] = "update" if existing is not None else "create"
        return result


def _error_response(error):
    if isinstance(error, FsFailure):
        code, message = error.code, str(error)
    elif isinstance(error, OSError):
        if error.errno in (errno.EACCES, errno.EPERM):
            code, message = "FS_PERMISSION_DENIED", "Filesystem permission denied."
        elif error.errno in (errno.ENAMETOOLONG, errno.EFBIG):
            code, message = "FS_TOO_LARGE", "Filesystem path or content exceeds its limit."
        elif error.errno in (errno.ENOENT, errno.ENOTDIR):
            code, message = "FS_NOT_FOUND", "Filesystem path not found."
        else:
            code, message = "FS_IO_ERROR", "Filesystem operation failed."
    else:
        code, message = "FS_IO_ERROR", "Invalid filesystem protocol data."
    return {"protocol": PROTOCOL, "ok": False, "error": {"code": code, "message": message}}


def _encode(response):
    encoded = json.dumps(response, ensure_ascii=False, allow_nan=False, separators=(",", ":")).encode("utf-8")
    if len(encoded) > MAX_RESPONSE_BYTES:
        _fail("FS_TOO_LARGE", "Filesystem response exceeds its byte limit.")
    return encoded


def dispatch(request):
    """Validate finite data and return one bounded versioned response envelope."""
    try:
        operation, args = _validate(request)
        _check_cancel(args)
        if operation == "resolve":
            result = resolve_target(args["path"], args["cwd"])
        elif operation == "lstat":
            result = _metadata(_probe(_display_path(args["path"], args["cwd"]), nofollow=True), nofollow=True)
        elif operation == "stat":
            result = _metadata(_probe(args["target"]["path"]))
        elif operation == "list":
            result = _list(args["target"], args)
        elif operation in ("read-text", "read-bytes", "read-range"):
            if operation == "read-range":
                data, version = _stable_read(args["target"], args, offset=args["offset"], length=args["length"])
            else:
                data, version = _stable_read(args["target"], args, limit=args.get("maxBytes", MAX_DATA_BYTES))
            result = {"version": version, "text": _decode(data)} if operation == "read-text" else {"version": version, "base64": base64.b64encode(data).decode("ascii")}
        else:
            result = mutate(args["target"], request)
        # Publication success is authoritative even if cancellation arrives later.
        if operation not in ("write-text", "edit-text"):
            _check_cancel(args)
        response = {"protocol": PROTOCOL, "ok": True, "result": result}
        _encode(response)
        return response
    except (FsFailure, OSError, ValueError, TypeError, RecursionError) as error:
        return _error_response(error)


def _pairs(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            _fail("FS_IO_ERROR", "Duplicate protocol object key.")
        result[key] = value
    return result


def _invalid_constant(value):
    _fail("FS_IO_ERROR", "Non-finite JSON number is not allowed.")


def _read_request(path):
    fd = os.open(path, os.O_RDONLY | os.O_NONBLOCK | os.O_NOFOLLOW)
    try:
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode):
            _fail("FS_IO_ERROR", "Request must be a regular file.")
        if info.st_size > MAX_REQUEST_BYTES:
            _fail("FS_TOO_LARGE", "Request exceeds its byte limit.")
        raw = _read_fd(fd, MAX_REQUEST_BYTES, {})
    finally:
        os.close(fd)
    return json.loads(raw.decode("utf-8", errors="strict"), object_pairs_hook=_pairs, parse_constant=_invalid_constant)


def _signal_cancel(_signum, _frame):
    global _cancelled
    _cancelled = True


def main(argv=None):
    """Fixed two-path argv; response is exclusive, owner-only, and never stdout.

    Reserve output before any operation. Until known process completion and a
    complete bounded JSON envelope, an empty/partial response is not an outcome.
    """
    global _cancelled
    argv = sys.argv[1:] if argv is None else argv
    if len(argv) != 2:
        return 2
    _cancelled = False
    signal.signal(signal.SIGTERM, _signal_cancel)
    signal.signal(signal.SIGINT, _signal_cancel)
    try:
        fd = os.open(argv[1], os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    except OSError:
        return 1
    try:
        os.fchmod(fd, 0o600)
        try:
            response = dispatch(_read_request(argv[0]))
        except (FsFailure, OSError, ValueError, TypeError, RecursionError) as error:
            response = _error_response(error)
        data = _encode(response)
        written = 0
        while written < len(data):
            count = os.write(fd, data[written:written + READ_CHUNK_BYTES])
            if count == 0:
                return 1
            written += count
        os.fsync(fd)
        return 0
    except (FsFailure, OSError, ValueError, TypeError):
        # No valid terminal response: retain artifacts and an unknown outcome.
        return 1
    finally:
        os.close(fd)


if __name__ == "__main__":
    sys.exit(main())
