#!/usr/bin/env python3
"""P5 service rows GS-01..GS-06 (docs/ISOLATED_REGRESSION_MATRIX.md).

Runs a game-save binary against disposable data directories with random
tokens, never a NAS or a live save, and prints a JSON evidence log. Tokens are
never printed.

    go build -o /tmp/gs-bin . && python3 -I scripts/p5_service_rows.py /tmp/gs-bin "$(mktemp -d)"
"""
import hashlib
import http.server
import json
import os
import secrets
import shutil
import socket
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.request
from base64 import b64encode
from datetime import datetime, timezone

BIN, WORK = sys.argv[1], sys.argv[2]
TOKEN = secrets.token_hex(32)
ADMIN = secrets.token_hex(32)
evidence = []


def free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


class Service:
    def __init__(self, data, extra=None):
        self.data, self.port, self.extra = data, free_port(), dict(extra or {})
        self.proc = None

    def start(self):
        env = dict(os.environ, GAME_SAVE_TOKEN=TOKEN, GAME_SAVE_ADMIN_TOKEN=ADMIN, **self.extra)
        log = open(os.path.join(WORK, f"service-{self.port}.log"), "ab")
        self.proc = subprocess.Popen([BIN, "--data-dir", self.data, "--listen", f"127.0.0.1:{self.port}"],
                                     env=env, stdout=log, stderr=log)
        for _ in range(100):
            try:
                if self.req("GET", "/healthz", auth=None)[0] == 200:
                    return self
            except OSError:
                pass
            time.sleep(0.1)
        raise RuntimeError("service did not start")

    def stop(self):
        self.proc.terminate()
        self.proc.wait(10)

    def req(self, method, path, body=None, headers=None, auth="token"):
        h = dict(headers or {})
        if auth == "token":
            h["Authorization"] = f"Bearer {TOKEN}"
        elif auth == "admin":
            h["Authorization"] = "Basic " + b64encode(f"admin:{ADMIN}".encode()).decode()
            h["X-Game-Save-Admin"] = "1"
        r = urllib.request.Request(f"http://127.0.0.1:{self.port}{path}", data=body, method=method, headers=h)
        try:
            with urllib.request.urlopen(r, timeout=10) as resp:
                return resp.status, resp.read()
        except urllib.error.HTTPError as e:
            return e.code, e.read()


def jbody(raw):
    try:
        return json.loads(raw)
    except ValueError:
        return raw.decode(errors="replace")[:200]


def record(row, passed, **details):
    evidence.append({"row": row, "result": "pass" if passed else "fail", **details})
    print(f"{row}: {'pass' if passed else 'FAIL'}", file=sys.stderr)


def upload(svc, kind, game, snap, content):
    name = f"{game}__main__{snap}.zip"
    return name, svc.req("PUT", f"/v1/opensave/files/{kind}/{name}", content)[0]


def candidate(snap, archive, parent, content):
    return json.dumps({
        "id": snap, "archive": archive, "parent": parent,
        "contentDigest": hashlib.sha256(content).hexdigest(),
        "createdAt": datetime.now(timezone.utc).isoformat().replace("+00:00", "Z"),
    }).encode()


def publish(svc, kind, game, snap, archive, parent, content, if_match):
    return svc.req("PUT", f"/v1/opensave/scopes/{kind}/{game}/snapshots/{snap}",
                   candidate(snap, archive, parent, content),
                   {"If-Match": f'"{if_match}"', "Content-Type": "application/json"})[0]


def scope(svc, kind, game):
    status, raw = svc.req("GET", f"/v1/opensave/scopes/{kind}/{game}")
    body = jbody(raw)
    head = (body.get("head") or {}) if isinstance(body, dict) else {}
    conflicts = body.get("conflicts") or [] if isinstance(body, dict) else []
    return status, body, head.get("id"), [c.get("id") for c in conflicts]


data = os.path.join(WORK, "data")
os.makedirs(data)

# GS-01: empty service, the two shared-fixture reads, restart; no device recorded.
svc = Service(data).start()
h1 = svc.req("GET", "/healthz", auth=None)[0]
sw = scope(svc, "switch", "010000000000BEEF")
pc = scope(svc, "pc", "regression-pc")
svc.stop()
svc.start()
h2 = svc.req("GET", "/healthz", auth=None)[0]
sw2 = scope(svc, "switch", "010000000000BEEF")
overview = jbody(svc.req("GET", "/admin/api/overview", auth="admin")[1])
devices = overview.get("devices") if isinstance(overview, dict) else overview
record("GS-01", h1 == h2 == 200 and sw[0] == pc[0] == 200 and sw[2] is None and pc[2] is None and sw2[2] is None and not devices,
       health=[h1, h2], heads=[sw[2], pc[2], sw2[2]], devices=devices)

# GS-02: publish A with If-Match "", restart, the head is still A with the same digest.
A = b"PC-REV-A"
nameA, upA = upload(svc, "pc", "regression-pc", "snap-a", A)
pubA = publish(svc, "pc", "regression-pc", "snap-a", nameA, "", A, "")
svc.stop()
svc.start()
_, body, head, _ = scope(svc, "pc", "regression-pc")
digest = (body.get("head") or {}).get("contentDigest")
record("GS-02", upA == 201 and pubA == 201 and head == "snap-a" and digest == hashlib.sha256(A).hexdigest(),
       upload=upA, publish=pubA, headAfterRestart=head)

# GS-03: B child of A; stale C (parent A) refused; C retained as a conflict and kept by a prune.
B, C = b"PC-REV-B", b"PC-REV-C"
nameB, _ = upload(svc, "pc", "regression-pc", "snap-b", B)
pubB = publish(svc, "pc", "regression-pc", "snap-b", nameB, "snap-a", B, "snap-a")
nameC, _ = upload(svc, "pc", "regression-pc", "snap-c", C)
staleC = publish(svc, "pc", "regression-pc", "snap-c", nameC, "snap-a", C, "snap-a")
conflictC = svc.req("PUT", "/v1/opensave/scopes/pc/regression-pc/conflicts/snap-c",
                    candidate("snap-c", nameC, "snap-a", C), {"Content-Type": "application/json"})[0]
prune = svc.req("POST", "/admin/api/maintenance/prune", b"", auth="admin")[0]
_, _, head, conflicts = scope(svc, "pc", "regression-pc")
record("GS-03", pubB == 201 and staleC in (409, 412) and conflictC == 201 and head == "snap-b" and "snap-c" in conflicts,
       publishB=pubB, staleC=staleC, conflictC=conflictC, prune=prune, head=head, conflicts=conflicts)
svc.stop()


# GS-04: Bark upload notifications to an endpoint that answers 502 must not block a publish.
class Failing(http.server.BaseHTTPRequestHandler):
    hits = 0

    def do_GET(self):
        Failing.hits += 1
        self.send_response(502)
        self.end_headers()

    do_POST = do_GET

    def log_message(self, *args):
        pass


bark_port = free_port()
bark = http.server.HTTPServer(("127.0.0.1", bark_port), Failing)
threading.Thread(target=bark.serve_forever, daemon=True).start()
data4 = os.path.join(WORK, "data-bark")
os.makedirs(data4)
svc4 = Service(data4, {"GAME_SAVE_BARK_URL": f"http://127.0.0.1:{bark_port}/disposable-device",
                       "GAME_SAVE_BARK_EVENTS": "upload"}).start()
P = b"PSP-REV-A"
nameP, _ = upload(svc4, "emulator-ppsspp", "regression-ppsspp", "snap-a", P)
pubP = publish(svc4, "emulator-ppsspp", "regression-ppsspp", "snap-a", nameP, "", P, "")
time.sleep(1.5)
_, _, headP, _ = scope(svc4, "emulator-ppsspp", "regression-ppsspp")
svc4.stop()
bark.shutdown()
record("GS-04", pubP == 201 and headP == "snap-a" and Failing.hits > 0, publish=pubP, head=headP, barkAttempts=Failing.hits)

# GS-05: copy the complete data directory and start a new service on the copy.
copy = os.path.join(WORK, "data-copy")
shutil.copytree(data, copy)
svc5 = Service(copy).start()
_, _, head5, conflicts5 = scope(svc5, "pc", "regression-pc")
overview5 = svc5.req("GET", "/admin/api/overview", auth="admin")[0]
svc5.stop()
record("GS-05", head5 == "snap-b" and "snap-c" in conflicts5 and overview5 == 200,
       head=head5, conflicts=conflicts5, overview=overview5)

# GS-06: corrupt one SaveVault archive in another copy; verify must name it; metrics are visible.
copy6 = os.path.join(WORK, "data-corrupt")
shutil.copytree(data, copy6)
target = os.path.join(copy6, "opensave-native", "pc", "regression-pc__main__snap-a.zip")
with open(target, "wb") as f:
    f.write(b"tampered")
svc6 = Service(copy6).start()
verify = svc6.req("POST", "/admin/api/maintenance/verify", b"", auth="admin")
report = jbody(verify[1])
metrics_status, metrics = svc6.req("GET", "/metrics", auth="admin")
svc6.stop()
problems = report.get("problems", []) if isinstance(report, dict) else []
named = [p for p in problems if p.get("archive") == "regression-pc__main__snap-a.zip" and p.get("problem") == "corrupt"]
record("GS-06", verify[0] == 200 and len(problems) == 1 and len(named) == 1 and metrics_status == 200,
       verify=verify[0], problems=problems, objects=report.get("objects") if isinstance(report, dict) else None,
       metrics=[l for l in metrics.decode().splitlines() if l.startswith(("game_save_opensave", "game_save_disk"))])

json.dump({"rows": evidence}, sys.stdout, ensure_ascii=False, indent=1)
