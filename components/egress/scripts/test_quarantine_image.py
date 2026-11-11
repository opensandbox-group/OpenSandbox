#!/usr/bin/env python3
# Copyright 2026 The OpenSandbox Authors
# SPDX-License-Identifier: Apache-2.0

"""Real image coverage of runtime recovery -> packet quarantine, never a mock pass.

Build the ordinary components/egress/Dockerfile image, then pass --image IMAGE;
without --image this script builds it. No fault build tag, provisioning helper,
private state volume, or restart/replay guarantee is involved. The real image
entrypoint launches supervisor -> Go -> mitmdump. A normal policy update must
leave traffic working; killing the current mitmdump must make the living Go
owner enter recovery and fence its original network namespace.

An independent UID 1000/cap-drop ALL workload supplies real new and established
TCP flows. Management uses Unix sockets/docker exec; trusted nft observers must
prove their actual namespace. The separate kernel matrix covers full IPv4/IPv6
packet paths. Ensure failure/QuarantineUnknown remains an owner unit-test case.
Unavailable Docker/kernel prerequisites FAIL. Hermetic runner tests do not
establish real-image execution. No user computer or external service is used.
"""

import argparse
import json
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import time
import uuid

ROOT = Path(__file__).resolve().parents[3]
EGRESS = "/opt/opensandbox-egress/egress"
SUPERVISOR = "/opt/opensandbox-egress/supervisor"
WORKER_STARTED = "supervisor: worker started ("
CONFIRMED = "packet isolation=QuarantineConfirmed"
REQUIRED = ["TestRuntimeImagePrerequisites", "TestImageFenceVerifierDormantRejection",
            "TestRuntimeMitmFaultOwnerContainment"]
FENCE_TEXT = '''table inet opensandbox_quarantine {
comment "opensandbox quarantine v1"
chain input {
type filter hook input priority -450; policy drop;
drop
}
chain output {
type filter hook output priority -450; policy drop;
drop
}
chain forward {
type filter hook forward priority -450; policy drop;
drop
}
}
'''

SNAPSHOT = r'''
import json, os, pathlib
out = {"processes": [], "netns": os.readlink("/proc/self/ns/net")}
for p in pathlib.Path("/proc").iterdir():
    if not p.name.isdecimal():
        continue
    try:
        args = [v.decode() for v in (p / "cmdline").read_bytes().split(b"\0") if v]
        status = dict(line.split(":", 1) for line in (p / "status").read_text().splitlines())
        if not args or status["State"].strip().startswith("Z"):
            continue
        role = None
        if args == ["/opt/opensandbox-egress/egress"]:
            role = "worker"
        elif args[0] == "/opt/opensandbox-egress/supervisor":
            role = "supervisor"
        elif any(pathlib.Path(a).name == "mitmdump" for a in args[:2]):
            role = "mitmdump"
        if role:
            start = (p / "stat").read_text().rsplit(")", 1)[1].split()[19]
            out["processes"].append({"pid": int(p.name), "ppid": int(status["PPid"]),
                "uid": int(status["Uid"].split()[0]), "role": role, "start": start})
    except (OSError, KeyError, ValueError):
        continue
print(json.dumps(out))
'''


ENTRYPOINT = [SUPERVISOR, "--pre-start=/opt/opensandbox-egress/cleanup.sh",
              "--name=egress", "--grace-period=20s", "--", EGRESS]

WORKLOAD = r'''
import json, os, pathlib, socket, threading, uuid
listener = socket.socket()
listener.bind(("127.0.0.1", 0))
listener.listen()
address = listener.getsockname()
boot = str(uuid.uuid4())
existing = None
def echo(conn):
    with conn:
        while True:
            data = conn.recv(4096)
            if not data:
                return
            conn.sendall(data)
def serve():
    while True:
        conn, _ = listener.accept()
        threading.Thread(target=echo, args=(conn,), daemon=True).start()
threading.Thread(target=serve, daemon=True).start()
def exchange(conn):
    token = uuid.uuid4().hex.encode()
    try:
        conn.sendall(token)
        received = b""
        while len(received) < len(token):
            part = conn.recv(len(token) - len(received))
            if not part:
                return False
            received += part
        return received == token
    except OSError:
        return False
def probe():
    fresh = False
    try:
        with socket.create_connection(address, timeout=0.35) as conn:
            fresh = exchange(conn)
    except OSError:
        pass
    return {"new_tcp": fresh, "established_tcp": None if existing is None else exchange(existing)}
control = socket.socket(socket.AF_UNIX)
control.bind("/tmp/quarantine-workload.sock")
os.chmod("/tmp/quarantine-workload.sock", 0o600)
control.listen()
while True:
    conn, _ = control.accept()
    with conn:
        request = json.loads(conn.makefile("rb").readline())
        status = dict(line.split(":", 1) for line in pathlib.Path("/proc/self/status").read_text().splitlines())
        reply = {"boot": boot, "uid": os.geteuid(), "cap_eff": status["CapEff"].strip(),
                 "netns": os.readlink("/proc/self/ns/net")}
        if request["op"] == "establish":
            try:
                existing = socket.create_connection(address, timeout=0.35)
                reply["established"] = exchange(existing)
            except OSError:
                reply["established"] = False
        elif request["op"] == "probe":
            reply.update(probe())
        conn.sendall(json.dumps(reply).encode() + b"\n")
'''

WORKLOAD_CLIENT = r'''
import json, socket, sys
with socket.socket(socket.AF_UNIX) as conn:
    conn.settimeout(5)
    conn.connect("/tmp/quarantine-workload.sock")
    conn.sendall(json.dumps({"op": sys.argv[1]}).encode() + b"\n")
    print(conn.makefile("r").readline().strip())
'''

FENCE_OBSERVER = r'''
import json, os, subprocess
namespace = os.readlink("/proc/self/ns/net")
result = subprocess.run(["nft", "-j", "list", "ruleset", "inet"],
                        check=True, capture_output=True, text=True)
ruleset = json.loads(result.stdout)
present = any(entry.get("table", {}).get("family") == "inet" and
              entry.get("table", {}).get("name") == "opensandbox_quarantine"
              for entry in ruleset["nftables"])
table_text = None
if present:
    table_text = subprocess.run(["nft", "-n", "-y", "list", "table", "inet", "opensandbox_quarantine"],
                                check=True, capture_output=True, text=True).stdout
print(json.dumps({"netns": namespace, "ruleset": ruleset, "table_text": table_text}))
'''

class Failure(RuntimeError):
    pass

def require(condition, message):
    if not condition:
        raise Failure(message)

def fence_present(data, table_text=None):
    """Require strict JSON plus complete owner/flags text, or true JSON absence."""
    require(isinstance(data, dict) and set(data) == {"nftables"}, "invalid nft JSON envelope")
    require(isinstance(data["nftables"], list), "invalid nft JSON entries")
    owned = []
    for entry in data["nftables"]:
        require(isinstance(entry, dict) and len(entry) == 1, "invalid nft entry")
        kind, obj = next(iter(entry.items()))
        require(isinstance(obj, dict), "invalid nft object")
        if obj.get("family") == "inet" and obj.get("name" if kind == "table" else "table") == "opensandbox_quarantine":
            owned.append((kind, obj))
    if not owned:
        return False
    require(len(owned) == 7, "fence must contain exactly one table, three chains and three rules")
    tables, chains, rules = [], {}, {}
    for kind, obj in owned:
        if kind == "table":
            require(set(obj) <= {"family", "name", "handle", "comment", "flags"}, "unknown fence table property")
            require(("comment" not in obj or obj["comment"] == "opensandbox quarantine v1")
                    and obj.get("flags", []) == [],
                    "fence has wrong owner or nonempty flags")
            tables.append(obj)
        elif kind == "chain":
            require(set(obj) <= {"family", "table", "name", "handle", "type", "hook", "prio", "policy"},
                    "unknown fence chain property")
            name = obj.get("name")
            require(name in ("input", "output", "forward") and name not in chains, "invalid fence chain")
            require((obj.get("type"), obj.get("hook"), obj.get("prio"), obj.get("policy")) ==
                    ("filter", name, -450, "drop"), "fence base hook is not fail-closed")
            chains[name] = obj
        elif kind == "rule":
            require(set(obj) <= {"family", "table", "chain", "handle", "expr"}, "unknown fence rule property")
            name = obj.get("chain")
            require(name in ("input", "output", "forward") and name not in rules, "invalid fence rule")
            require(obj.get("expr") == [{"drop": None}], "fence contains a conditional rule or exception")
            rules[name] = obj
        else:
            raise Failure("unexpected fence object: " + kind)
    require(len(tables) == 1 and set(chains) == set(rules) == {"input", "output", "forward"},
            "fence is missing required hooks")
    # nft 1.0.6 omits table comments from JSON, and its flag rendering is not
    # sufficient proof. Text is mandatory even when JSON includes the comment.
    lines = lambda value: [line.strip() for line in value.splitlines() if line.strip()]
    require(isinstance(table_text, str) and lines(table_text) == lines(FENCE_TEXT),
            "fence text does not match the complete owner/no-flags/drop-hook template")
    return True

def processes(snapshot, role):
    return [p for p in snapshot["processes"] if p["role"] == role]

def assert_process_chain(snapshot, mitm):
    owners, workers, children = (processes(snapshot, role) for role in ("supervisor", "worker", "mitmdump"))
    require(len(owners) == 1 and owners[0]["pid"] == 1, "real supervisor must be PID 1")
    require(len(workers) == 1 and workers[0]["ppid"] == 1, "real Go worker must be the supervisor child")
    if mitm:
        require(len(children) == 1, "exactly one real mitmdump must be alive")
        require(children[0]["uid"] == 10042 and children[0]["ppid"] == workers[0]["pid"],
                "mitmdump must run as its dedicated user and be the Go worker's child")
    else:
        require(not children, "mitmdump launched before the expected bootstrap boundary")

class Docker:
    def __init__(self, artifacts):
        self.artifacts = artifacts

    def run(self, *args, check=True, timeout=30):
        command = ["docker", *map(str, args)]
        try:
            result = subprocess.run(command, capture_output=True, text=True, timeout=timeout)
        except (OSError, subprocess.TimeoutExpired) as exc:
            raise Failure(f"Docker prerequisite/command failed: {command[:3]}: {exc}") from exc
        with (self.artifacts / "commands.jsonl").open("a") as stream:
            stream.write(json.dumps({"command": command, "returncode": result.returncode,
                                     "stdout": result.stdout, "stderr": result.stderr}) + "\n")
        if check and result.returncode:
            raise Failure(f"docker {' '.join(map(str, args[:3]))} failed ({result.returncode}): "
                          f"{result.stdout[-3000:]}{result.stderr[-3000:]}")
        return result

    def json(self, *args):
        return json.loads(self.run(*args).stdout)


def normalize_capabilities(values):
    """Docker inspect may spell Linux capability names with one CAP_ prefix."""
    if values is None:
        return []
    require(isinstance(values, list) and all(isinstance(value, str) for value in values),
            "invalid Docker capability list")
    return [value.removeprefix("CAP_") for value in values]


def validate_observer(data, namespace):
    require(data.get("netns") == namespace,
            f"fence observer ran in wrong namespace: expected {namespace}, observed {data.get('netns')}")
    return fence_present(data["ruleset"], data.get("table_text"))


def validate_fault(before, after, observed, health, logs):
    require(after["netns"] == before["netns"], "runtime fault changed the original namespace")
    require(processes(after, "worker") == processes(before, "worker"), "fault replaced or killed the Go recovery owner")
    require(not processes(after, "mitmdump"), "recovery started another mitmdump")
    require(logs.count(WORKER_STARTED) == 1 and "mitmdump restarted" not in logs,
            "runtime recovery relaunched a child or worker")
    require(CONFIRMED in logs, "real owner did not report confirmed runtime containment")
    require(observed["fenced"], "original workload namespace lacks its exact independent fence")
    require(not observed["probe"]["new_tcp"] and observed["probe"]["established_tcp"] is False,
            "recovery still delivers new or established workload TCP")
    require(health != "200", "recovery became ready again")


class Runtime:
    def __init__(self, docker, image):
        self.docker, self.image = docker, image
        self.name = "egress-runtime-quarantine-" + uuid.uuid4().hex[:12]
        self.workload = self.name + "-workload"
        self.created = []
        self.original = None

    def record(self, name, value):
        (self.docker.artifacts / (name + ".json")).write_text(json.dumps(value, indent=2) + "\n")

    def __enter__(self):
        self.created.append(self.name)
        env = {"OPENSANDBOX_EGRESS_EXPERIMENTAL_REVISION_RUNTIME": "true",
               "OPENSANDBOX_EGRESS_MODE": "dns+nft", "OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT": "true",
               "OPENSANDBOX_EGRESS_RULES": '{"defaultAction":"deny","egress":[]}',
               "OPENSANDBOX_EGRESS_DNS_UPSTREAM": "1.1.1.1:53"}
        args = ["create", "--name", self.name, "--network", "none", "--cap-drop", "NET_RAW",
                "--cap-add", "NET_ADMIN", "--security-opt", "no-new-privileges"]
        for key, value in env.items():
            args += ["--env", key + "=" + value]
        try:
            self.docker.run(*args, self.image)
            info = self.docker.json("inspect", self.name)[0]
            host = info["HostConfig"]
            require(info["Config"]["Entrypoint"] == ENTRYPOINT, "real image entrypoint must be used")
            require(not host["Privileged"] and host["NetworkMode"] == "none" and not host["PidMode"]
                    and normalize_capabilities(host["CapAdd"]) == ["NET_ADMIN"]
                    and normalize_capabilities(host["CapDrop"]) == ["NET_RAW"],
                    "sidecar namespace/capability contract changed")
            require(not info["Mounts"], "runtime test must not depend on private state mounts")
            self.docker.run("start", self.name)
            self.wait("normal bootstrap", lambda: self.http("/healthz") == "200")
            return self
        except BaseException:
            self.__exit__(*sys.exc_info())
            raise

    def __exit__(self, *_):
        evidence = {"captures": [], "cleanup": []}

        def capture(filename, *args, combine_stderr=False):
            entry = {"file": filename}
            evidence["captures"].append(entry)
            try:
                result = self.docker.run(*args, check=False, timeout=10)
                entry.update(returncode=result.returncode,
                             status="collected" if result.returncode == 0 else "command-failed")
                # Keep exact nft stdout, including unsupported JSON properties.
                # This is diagnostic evidence, never accepted containment proof.
                output = result.stdout + result.stderr if combine_stderr else result.stdout
                (self.docker.artifacts / filename).write_text(output)
                (self.docker.artifacts / (filename + ".stderr")).write_text(result.stderr)
            except Exception as exc:
                entry.update(status="capture-failed", error=str(exc))
                print(f"Exit diagnostic {filename} failed: {exc}", file=sys.stderr)

        if self.name in self.created:
            # Read only test-owned namespace/ruleset data. Never read process
            # environments, IPC credentials, or change any rules for diagnostics.
            capture("exit-sidecar-netns.txt", "exec", self.name, "readlink", "/proc/self/ns/net")
            capture("exit-nft-version.txt", "exec", self.name, "nft", "--version")
            capture("exit-nft-ruleset.json", "exec", self.name, "nft", "-j", "list", "ruleset", "inet")
            capture("exit-nft-quarantine.txt", "exec", self.name, "nft", "-n", "-y", "list", "table", "inet", "opensandbox_quarantine")
        if self.original and self.workload in self.created:
            # Attempt this before deleting either container, even when the owner
            # timed out before the ordinary successful-path observer could run.
            capture("exit-workload-observer.json", "run", "--rm", "--network", "container:" + self.workload,
                    "--cap-drop", "ALL", "--cap-add", "NET_ADMIN", "--read-only", "--entrypoint", "python3",
                    self.image, "-c", FENCE_OBSERVER)
        for name in reversed(self.created):
            for suffix, args in (("logs.txt", ("logs", name)), ("inspect.json", ("inspect", name))):
                capture(name + "-" + suffix, *args, combine_stderr=suffix == "logs.txt")
            try:
                result = self.docker.run("rm", "--force", name, check=False, timeout=10)
                evidence["cleanup"].append({"container": name, "returncode": result.returncode})
            except Exception as exc:
                evidence["cleanup"].append({"container": name, "error": str(exc)})
                print(f"Cleanup of {name} failed: {exc}", file=sys.stderr)
        try:
            self.record("exit-diagnostics", evidence)
        except Exception as exc:
            print(f"Exit diagnostic manifest could not be saved: {exc}", file=sys.stderr)
        # Returning None preserves any original test exception; collection or
        # cleanup failures cannot turn a failed runtime assertion into a pass.

    def wait(self, description, predicate, timeout=60):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            result = predicate()
            if result:
                return result
            require(self.docker.json("inspect", self.name)[0]["State"]["Running"], "sidecar exited during " + description)
            time.sleep(0.2)
        raise Failure("timed out waiting for " + description)

    def snapshot(self):
        return self.docker.json("exec", self.name, "python3", "-c", SNAPSHOT)

    def logs(self):
        result = self.docker.run("logs", self.name)
        return result.stdout + result.stderr

    def http(self, path, data=None):
        args = ["exec", self.name, "curl", "--noproxy", "*", "--silent", "--output", "/dev/null",
                "--write-out", "%{http_code}", "--max-time", "10" if data is not None else "1"]
        if data is not None:
            args += ["--request", "POST", "--header", "Content-Type: application/json", "--data", json.dumps(data)]
        result = self.docker.run(*args, "http://127.0.0.1:18080" + path, check=False)
        require(result.returncode in (0, 7, 28), "HTTP probe could not execute: " + result.stderr)
        return result.stdout

    def start_workload(self):
        self.created.append(self.workload)
        self.docker.run("create", "--name", self.workload, "--network", "container:" + self.name,
            "--user", "1000:1000", "--cap-drop", "ALL", "--read-only", "--security-opt", "no-new-privileges",
            "--tmpfs", "/tmp:rw,noexec,nosuid,nodev,mode=1777", "--entrypoint", "python3", self.image, "-u", "-c", WORKLOAD)
        info = self.docker.json("inspect", self.workload)[0]
        host = info["HostConfig"]
        require(info["Config"]["User"] == "1000:1000" and normalize_capabilities(host["CapDrop"]) == ["ALL"]
                and not normalize_capabilities(host["CapAdd"]) and not host["Privileged"] and not host["PidMode"],
                "workload must have no capabilities or shared PID namespace")
        require(not any(m["Type"] == "volume" for m in info["Mounts"]), "workload must not mount private state")
        self.docker.run("start", self.workload)
        self.wait("unprivileged workload Unix control socket", lambda: self.control("status", check=False), timeout=15)
        self.original = self.control("status")
        require(self.original["uid"] == 1000 and int(self.original["cap_eff"], 16) == 0,
                "workload has unexpected effective credentials")
        require(self.original["netns"] == self.snapshot()["netns"], "workload did not join the original namespace")
        require(self.control("establish")["established"], "positive-control TCP connection failed")

    def control(self, operation, check=True):
        result = self.docker.run("exec", self.workload, "python3", "-c", WORKLOAD_CLIENT, operation, check=check)
        if result.returncode:
            return None
        value = json.loads(result.stdout)
        if self.original:
            require(all(value[key] == self.original[key] for key in ("boot", "uid", "cap_eff", "netns")),
                    "original workload process or namespace was replaced")
        return value

    def observe(self, label):
        probe = self.control("probe")
        observer = self.docker.json("run", "--rm", "--network", "container:" + self.workload,
            "--cap-drop", "ALL", "--cap-add", "NET_ADMIN", "--read-only", "--entrypoint", "python3",
            self.image, "-c", FENCE_OBSERVER)
        self.record(label + "-observer", observer)
        result = {"probe": probe, "fenced": validate_observer(observer, self.original["netns"])}
        self.record(label, result)
        return result


def test_image_verifier(docker, image):
    """Isolated verifier fixture, never a substitute for the real owner test."""
    directory = docker.artifacts / "verifier"
    directory.mkdir()
    fixture = Runtime(Docker(directory), image)
    d = fixture.docker
    fixture.created.append(fixture.name)
    try:
        d.run("create", "--name", fixture.name, "--network", "none", "--cap-drop", "ALL",
              "--cap-add", "NET_ADMIN", "--read-only", "--entrypoint", "sleep", image, "120")
        host = d.json("inspect", fixture.name)[0]["HostConfig"]
        require(not host["Privileged"] and host["NetworkMode"] == "none" and not host["PidMode"]
                and normalize_capabilities(host["CapDrop"]) == ["ALL"]
                and normalize_capabilities(host["CapAdd"]) == ["NET_ADMIN"], "unsafe verifier fixture")
        d.run("start", fixture.name)
        namespace = d.run("exec", fixture.name, "readlink", "/proc/self/ns/net").stdout.strip()
        apply = "import subprocess,sys; subprocess.run(['nft','-f','-'],input=sys.argv[1],text=True,check=True)"
        d.run("exec", fixture.name, "python3", "-c", apply, FENCE_TEXT)
        valid = d.json("exec", fixture.name, "python3", "-c", FENCE_OBSERVER)
        fixture.record("valid-observer", valid)
        require(validate_observer(valid, namespace), "real valid verifier fixture was not accepted")
        dormant = "delete table inet opensandbox_quarantine\n" + FENCE_TEXT.replace("{\n", "{\nflags dormant;\n", 1)
        d.run("exec", fixture.name, "python3", "-c", apply, dormant)
        raw = d.run("exec", fixture.name, "nft", "-j", "list", "ruleset", "inet", check=False)
        text = d.run("exec", fixture.name, "nft", "-n", "-y", "list", "table", "inet", "opensandbox_quarantine", check=False)
        actual_namespace = d.run("exec", fixture.name, "readlink", "/proc/self/ns/net").stdout.strip()
        fixture.record("dormant-readback", {"netns": actual_namespace,
            "json": {"returncode": raw.returncode, "stdout": raw.stdout, "stderr": raw.stderr},
            "text": {"returncode": text.returncode, "stdout": text.stdout, "stderr": text.stderr}})
        require(actual_namespace == namespace, "verifier fixture namespace changed")
        require(text.returncode == 0 and any(line.strip().rstrip(";") == "flags dormant" for line in text.stdout.splitlines()),
                "real dormant fixture was not established")
        if raw.returncode:
            print("Verifier rejected dormant table: JSON read failed; raw output and stderr preserved", flush=True)
        else:
            try:
                accepted = fence_present(json.loads(raw.stdout), text.stdout)
            except (Failure, ValueError):
                accepted = False
            require(not accepted, "verifier accepted a real dormant table")
    finally:
        fixture.__exit__(*sys.exc_info())


def test_runtime_fault(docker, image):
    with Runtime(docker, image) as runtime:
        before = runtime.snapshot()
        assert_process_chain(before, mitm=True)
        runtime.start_workload()
        initial = runtime.observe("bootstrap")
        require(not initial["fenced"] and initial["probe"]["new_tcp"] and initial["probe"]["established_tcp"],
                "normal bootstrap unexpectedly fenced or blocked traffic")
        policy = {"defaultAction": "deny", "egress": [{"action": "allow", "target": "203.0.113.7"}]}
        require(runtime.http("/policy", policy) == "200", "normal experimental policy update failed")
        actual = docker.json("exec", runtime.name, "curl", "--noproxy", "*", "--silent", "--fail",
                             "--max-time", "3", "http://127.0.0.1:18080/policy")
        require(actual["policy"] == policy, "normal policy update did not publish the requested policy")
        updated = runtime.observe("normal-policy-update")
        require(not updated["fenced"] and updated["probe"]["new_tcp"] and updated["probe"]["established_tcp"]
                and runtime.http("/healthz") == "200", "normal mutation introduced quarantine or broke traffic")
        current = runtime.snapshot()
        require(current == before, "normal policy update unexpectedly replaced runtime processes")
        child = processes(current, "mitmdump")[0]
        runtime.record("before-mitm-fault", current)
        # This is a real owner caller: current-child OnExit -> revision recovery
        # -> Ensure. This runtime-fault test never installs or removes quarantine itself.
        docker.run("exec", runtime.name, "kill", "-KILL", str(child["pid"]))
        runtime.wait("owner-confirmed packet quarantine", lambda: CONFIRMED in runtime.logs(), timeout=20)
        # Observe three times across the first normal child-retry windows. No
        # permanent/restart guarantee is inferred from this bounded runtime test.
        for index in range(3):
            snapshot = runtime.snapshot()
            observed = runtime.observe("after-mitm-fault-" + str(index))
            runtime.record("after-mitm-fault-processes-" + str(index), snapshot)
            validate_fault(before, snapshot, observed, runtime.http("/healthz"), runtime.logs())
            if index < 2:
                time.sleep(2)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image", help="ordinary egress image; no fault-injection build tag")
    parser.add_argument("--artifacts", type=Path, help="directory for real-runtime evidence")
    args = parser.parse_args(argv)
    artifacts = args.artifacts or Path(tempfile.mkdtemp(prefix="egress-runtime-quarantine-"))
    artifacts.mkdir(parents=True, exist_ok=True)
    docker = Docker(artifacts)
    image = args.image or "opensandbox-egress-runtime-test:" + uuid.uuid4().hex[:12]
    results = []

    def run_case(name, action):
        print("=== RUN   " + name, flush=True)
        started = time.monotonic()
        record = {"name": name, "status": "RUN"}
        results.append(record)
        try:
            action()
        except Exception as exc:
            record.update(status="FAIL", error=str(exc))
            print(f"--- FAIL: {name}: {exc}", flush=True)
            raise
        record.update(status="PASS", seconds=round(time.monotonic() - started, 3))
        print(f"--- PASS: {name} ({record['seconds']:.3f}s)", flush=True)

    def prerequisites():
        require(shutil.which("docker") is not None, "Docker CLI unavailable; real runtime image test did not run")
        info = docker.json("info", "--format", "{{json .}}")
        (artifacts / "docker-runtime.json").write_text(json.dumps(info, indent=2) + "\n")
        require(info["OSType"] == "linux" and not any("rootless" in x for x in info.get("SecurityOptions", [])),
                "runtime image test requires rootful Linux Docker with NET_ADMIN")
        if not args.image:
            docker.run("build", "-f", ROOT / "components/egress/Dockerfile", "-t", image, ROOT, timeout=1800)
        require(docker.json("image", "inspect", image)[0]["Config"]["Entrypoint"] == ENTRYPOINT,
                "image has an unexpected entrypoint")
        docker.run("run", "--rm", "--network", "none", "--cap-drop", "ALL", "--cap-add", "NET_ADMIN",
                   "--entrypoint", "nft", image, "-j", "list", "ruleset", "inet")

    success = False
    try:
        run_case(REQUIRED[0], prerequisites)
        run_case(REQUIRED[1], lambda: test_image_verifier(docker, image))
        run_case(REQUIRED[2], lambda: test_runtime_fault(docker, image))
        require([r["name"] for r in results] == REQUIRED and all(r["status"] == "PASS" for r in results),
                "required runtime image RUN/PASS evidence is incomplete")
        success = True
    except (Failure, ValueError, KeyError) as exc:
        print("Runtime quarantine image validation FAILED: " + str(exc), file=sys.stderr)
    finally:
        (artifacts / "results.json").write_text(json.dumps({"kind": "real-runtime-docker-image", "image": image,
            "passed": success, "required": REQUIRED, "results": results}, indent=2) + "\n")
        print("Runtime image evidence: " + str(artifacts.resolve()), flush=True)
        if not args.image and shutil.which("docker"):
            docker.run("image", "rm", image, check=False)
    return 0 if success else 1


if __name__ == "__main__":
    sys.exit(main())
