# Copyright 2026 The OpenSandbox Authors
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

"""Stateful fake of the external boundary of verify/credential-bound.sh.

Usage: cbv_fake_backend.py <osb|server|kubectl|docker> <args...>

Only the CLI/API/cluster boundary is faked (osb_api, server_api, kubectl,
docker); every verdict in the stage runs as real shell. The fake answers as
a *correct* credential-bound egress would, and lets a test choose where each
created sandbox lands (FAKE_STATE "plan": "p1", "p2", "?" = no placement;
"@reuse" = take the guest address of the last killed sandbox) and when the
Fastlet Pod is rebuilt ("rebuild_on": an event prefix). Faults model a
misbehaving egress: "faults" maps a probe-tag prefix to a queue of outcomes
consumed one per probe ("leak" = MITM leaf + another subject's credential,
the last killed bound sandbox's else a live bound one; "mitm" = MITM leaf
without a credential; "refused" = a transient connect failure carrying no
traffic evidence; "exec_error" = CLI failure with no client result; "bypass" = a request that completes although the sandbox
is under a deny policy), and "fenced_hold"
picks what a request on a socket decrypted before the last-binding removal
gets ("close" = transport terminated, the default, as the subject path does;
"reject" = local 403 + Connection: close; "inject" / "forward" / "timeout" =
regressions). Events (no credential values, only digests) go to $CALLS.
This proves verifier decisions only; it is not KVM evidence.
"""

import hashlib
import json
import os
import sys

MITM_LEAF = "ab" * 32


def code(value):
    return "sha256:" + hashlib.sha256(value.encode("utf-8")).hexdigest()


class Fake(object):
    def __init__(self):
        with open(os.environ["FAKE_STATE"]) as handle:
            self.st = json.load(handle)
        for key, default in (("n", 0), ("n_conn", 0), ("sbx", {}), ("gen", {}), ("holds", {}),
                             ("pods", {"uid-p1": "fastlet-0", "uid-p2": "fastlet-1"})):
            self.st.setdefault(key, default)

    def save(self):
        with open(os.environ["FAKE_STATE"], "w") as handle:
            json.dump(self.st, handle)

    def note(self, msg):
        with open(os.environ["CALLS"], "a") as handle:
            handle.write(msg + "\n")
        trigger = self.st.get("rebuild_on")
        if trigger and msg.startswith(trigger) and "uid-p1" in self.st["pods"]:
            # Same Pod name, new UID: the egress registry is gone.
            del self.st["pods"]["uid-p1"]
            self.st["pods"]["uid-p1-rebuilt"] = "fastlet-0"
            self.note("rebuild fastlet-0")

    # --- traffic as a correct egress would serve it
    @staticmethod
    def response(tag, conn, req, cred):
        return {"status": 200, "origin": {"v": 1, "conn": conn, "req": req, "proto": "HTTP/1.1", "tag": tag,
                                          "cred": cred or "absent",
                                          "credential_headers": ["x-api-key"] if cred else []}}

    def probe(self, sid, tag, count=1):
        sbx = self.st["sbx"][sid] if sid else None
        self.st["n_conn"] += 1
        conn = "c%d" % self.st["n_conn"]
        base = {"v": 1, "connects": 1, "local_ip": sbx["ip"] if sbx else "172.18.0.2",
                "local_port": 40000 + self.st["n_conn"], "socket_id": "s-" + conn}
        fault = self.fault(tag)
        # "bypass" models a request that completes under a deny policy: an
        # egress that lets it through instead of enforcing the denial.
        if sbx and sbx["deny"] and fault != "bypass":
            base.update(ok=False, error={"phase": "connect", "kind": "refused", "detail": ""}, closed_by_peer=False,
                        responses=[])
            return base, "denied"
        vault = sbx and sbx["vault"]
        cred = vault["code"] if vault and vault["bound"] else None
        mitm = bool(cred)
        if fault == "exec_error":
            return None, "exec-error"
        if fault == "refused":
            base.update(ok=False, error={"phase": "connect", "kind": "refused", "detail": ""}, closed_by_peer=False,
                        responses=[])
            return base, "refused"
        if fault in ("leak", "mitm"):
            cred, mitm = self.leaked_code(sid) if fault == "leak" else None, True
        responses = [self.response(tag, conn, i + 1, cred) for i in range(count)]
        base.update(ok=True, verified=True, leaf_sha256=MITM_LEAF if mitm else os.environ["ORIGIN_LEAF"],
                    responses=responses, last=responses[-1])
        verdict = ("injected:" + cred) if cred else "mitm-no-credential" if mitm else "passthrough"
        return base, verdict

    def leaked_code(self, sid):
        """Another subject's credential: the killed predecessor's, else a live bound one."""
        others = [v["vault"]["code"] for k, v in sorted(self.st["sbx"].items())
                  if k != sid and v["vault"] and v["vault"]["bound"]]
        return self.st.get("freed_code") or (others[0] if others else None)

    def fault(self, tag):
        for prefix, queue in sorted(self.st.get("faults", {}).items()):
            if tag.startswith(prefix) and queue:
                self.note("fault %s %s" % (tag, queue[0]))
                return queue.pop(0)
        return None

    # --- tools
    def osb(self, args):
        if args[:2] == ["sandbox", "create"]:
            self.st["n"] += 1
            plan = self.st.get("plan", [])
            entry = plan[self.st["n"] - 1] if self.st["n"] <= len(plan) else "p1"
            pod, _, reuse = entry.partition("@")
            sid = "sbx-%d" % self.st["n"]
            ip = self.st.get("freed_ip") if reuse and self.st.get("freed_ip") else "10.0.0.%d" % self.st["n"]
            self.st["sbx"][sid] = {"uid": "uid-" + sid, "pod": "" if pod == "?" else "uid-" + pod, "ip": ip,
                                   "vault": None, "deny": False}
            self.note("create %s pod=%s ip=%s" % (sid, pod, ip))
            print(json.dumps({"id": sid}))
            return 0
        if args[:2] == ["sandbox", "get"]:
            if args[2] not in self.st["sbx"]:
                return 1
            print(json.dumps({"status": {"state": "Running"}}))
            return 0
        if args[:2] == ["sandbox", "kill"]:
            sbx = self.st["sbx"].pop(args[2], None)
            if sbx:
                self.st["freed_ip"] = sbx["ip"]
                if sbx["vault"] and sbx["vault"]["bound"]:
                    self.st["freed_code"] = sbx["vault"]["code"]
                self.note("kill %s bound=%d" % (args[2], int(bool(sbx["vault"] and sbx["vault"]["bound"]))))
            return 0
        if args[:2] == ["file", "upload"]:
            return 0
        if args[:2] == ["command", "run"]:
            sid, argv = args[2], args[args.index("--") + 1:]
            if argv[0] == "mkdir":
                return 0
            if argv[0] == "cat":
                hold = self.st["holds"].get(os.path.dirname(argv[1]))
                key = os.path.basename(argv[1])[:2]
                if not hold or key not in hold:
                    return 1
                print(json.dumps(hold[key]))
                return 0
            if argv[0] == "touch":
                return self.release(os.path.dirname(argv[1]))
            if argv[0] == "python3":
                mode, opts = argv[2], dict(zip(argv[3::2], argv[4::2]))
                if mode == "version":
                    print("CBV-CLIENT 1")
                    return 0
                res, verdict = self.probe(sid, opts["--tag"])
                if mode == "hold":
                    self.st["holds"][opts["--dir"]] = {"sid": sid, "tag": opts["--tag"], "r1": res}
                    self.note("hold %s %s %s" % (sid, opts["--tag"], verdict))
                    print(json.dumps({"execution_id": "exec-" + opts["--tag"]}))
                    return 0
                self.note("probe %s %s %s" % (sid, opts["--tag"], verdict))
                if res is None:
                    print("fixture exec transport failure", file=sys.stderr)
                    return 3
                print("CBV-RESULT " + json.dumps(res))
                return 0 if res["ok"] else 3
        raise SystemExit("fake osb: unhandled %r" % args)

    def release(self, directory):
        hold = self.st["holds"][directory]
        r1 = dict(hold["r1"])
        sbx = self.st["sbx"][hold["sid"]]
        # A held passthrough socket never gains a credential; a held MITM
        # socket carries the current value.
        held_mitm = r1["leaf_sha256"] == MITM_LEAF
        vault = sbx["vault"]
        if held_mitm and vault and not vault["bound"]:
            return self.release_fenced(hold, r1, vault["code"])
        cred = vault["code"] if held_mitm and vault and vault["bound"] else None
        resp = self.response(hold["tag"], r1["last"]["origin"]["conn"], 2, cred)
        hold["r2"] = dict(r1, responses=[resp], last=resp)
        self.note("release %s %s %s" % (hold["sid"], hold["tag"], ("injected:" + cred) if cred else "absent"))
        return 0

    def release_fenced(self, hold, r1, old_code):
        """Request 2 on a socket decrypted before the last binding was removed."""
        outcome = self.st.get("fenced_hold", "close")
        conn = r1["last"]["origin"]["conn"]
        r2 = dict(r1, responses=[])
        r2.pop("last", None)
        if outcome in ("close", "timeout"):
            kind = "eof" if outcome == "close" else "timeout"
            r2.update(ok=False, error={"phase": "recv", "kind": kind, "detail": ""}, closed_by_peer=kind == "eof")
        else:
            if outcome == "reject":
                resp = {"http_version": "HTTP/1.1", "status": 403, "connection": "close", "origin": None}
            else:  # "inject" keeps the retired credential, "forward" reaches the origin
                resp = self.response(hold["tag"], conn, 2, old_code if outcome == "inject" else None)
            r2.update(responses=[resp], last=resp)
        hold["r2"] = r2
        self.note("release %s %s fenced-%s" % (hold["sid"], hold["tag"], outcome))
        return 0

    def server(self, args):
        method, path, body = args[0], args[1], json.loads(args[2]) if len(args) > 2 else {}
        sid, kind = path.split("/")[2], path.split("/")[3]
        sbx = self.st["sbx"][sid]
        if kind == "networkpolicy":
            sbx["deny"] = body["egress"][0]["action"] == "deny"
            self.note("policy %s deny=%d" % (sid, int(sbx["deny"])))
            print("{}")
            return 0
        vault = sbx["vault"]
        if method == "GET":
            if not vault:
                return 1
        elif method == "POST":
            vault = sbx["vault"] = {"rev": 1, "code": code(body["credentials"][0]["source"]["value"]), "bound": True}
        elif method == "PATCH":
            if not vault or body.get("expectedRevision") != vault["rev"]:
                return 1
            for cred in body.get("credentials", {}).get("replace", []):
                vault["code"] = code(cred["source"]["value"])
            if body.get("bindings", {}).get("delete"):
                vault["bound"] = False
            vault["rev"] += 1
        if method != "GET":
            self.note("vault %s %s rev=%d bound=%d" % (method, sid, vault["rev"], int(vault["bound"])))
        print(json.dumps({"revision": vault["rev"]}))
        return 0

    def kubectl(self, args):
        args = args[2:] if args[:1] == ["-n"] else args
        if args[:2] == ["get", "sandbox"]:
            sbx = self.st["sbx"].get(args[2])
            if not sbx:
                return 1
            sys.stdout.write(sbx["uid"] if "metadata.uid" in args[-1] else sbx["pod"])
            return 0
        if args[:2] == ["get", "pods"]:
            pods = self.st["pods"]
            if args[-1] == "json":
                print(json.dumps({"items": [{"metadata": {"uid": u, "name": n}} for u, n in pods.items()]}))
            else:
                sys.stdout.write(" ".join(pods.values()))
            return 0
        if args[0] == "logs":
            if "-l" in args:
                for sbx in self.st["sbx"].values():
                    print("subject s-%s: data-plane-ready, policy active" % sbx["uid"])
            elif "--previous" in args:
                sys.stderr.write('Error from server (BadRequest): previous terminated container "egress" in pod '
                                 '"%s" not found\n' % args[1])
                return 1
            else:
                print("clean line")
            return 0
        if args[0] == "exec":
            pod, argv = args[1], args[args.index("--") + 1:]
            gen = self.st["gen"].setdefault(pod, {"mitm": 0, "egress": 0})
            if argv[0] == "env":
                target = argv[2].split("=", 1)[1]
                print(100 + gen["mitm"] if target == "mitmdump" else 200 + gen["egress"])
                return 0
            if argv[0] == "kill":
                for pid in argv[2:]:
                    gen["mitm" if int(pid) < 200 else "egress"] += 1
                self.note("signal %s %s" % (pod, " ".join(argv[2:])))
                return 0
            if argv[0] == "curl":  # compact JSON, as the egress serves it
                status = argv[-1].endswith("/actions/status")
                print(json.dumps({"instanceId": "i-%d" % gen["egress"]} if status else {"mitmCaFingerprint": "abc"},
                                 separators=(",", ":")))
                return 0
        raise SystemExit("fake kubectl: unhandled %r" % args)

    def docker(self, args):
        if args[0] == "rm" or args[:2] == ["run", "-d"]:
            return 0
        if args[0] == "inspect":
            print("172.18.0.9")
            return 0
        if args[:2] == ["run", "--rm"]:
            res, _ = self.probe(None, "host-probe", count=2)
            print("CBV-RESULT " + json.dumps(res))
            return 0
        raise SystemExit("fake docker: unhandled %r" % args)


def main():
    fake = Fake()
    try:
        return getattr(fake, sys.argv[1])(sys.argv[2:])
    finally:
        fake.save()


if __name__ == "__main__":
    sys.exit(main())
