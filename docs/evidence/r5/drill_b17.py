#!/usr/bin/env python3
"""B17 浏览器点击验收驱动：起真实进程 + 两个受控上游 + 副本映射，再跑 playwright 脚本。

浏览器 7 项在 b17_browser.js；「参选为空退化顺序 / 竞速真的并行」用真实上游命中数取证。
"""
import http.cookiejar
import json
import os
import shutil
import signal
import subprocess
import sys
import time
import urllib.error
import urllib.request

sys.path.insert(0, "/opt/data/cache/scratch/r5")
from fake_upstream import content  # noqa
import hashlib  # noqa

ROOT = "/opt/data/cache/scratch/r5/run4"
ADDR = "127.0.0.1:18380"
BASE = "http://" + ADDR
BIN = "/opt/data/cache/scratch/r5/mirror-lens"
EVID = "/opt/data/cache/scratch/ml8/docs/evidence/r5/browser"
EXE = "/opt/data/cache/scratch/pwbrowsers/chromium_headless_shell-1148/chrome-linux/headless_shell"
USER, PASS = "r5admin", "R5-drill-pass-1"

CONFIG = f"""\
addr: "{ADDR}"
data: "{ROOT}/data"
no_seed: true
registry:
  enabled: true
  docker_enabled: true
  ghcr_enabled: true
  upstream: "registry-1.docker.io"
  multi_origin_hosts: [gcr.io, quay.io]
  max_file_mb: 64
  tag_ttl: 5m
sources:
  enabled: true
replicas:
  mode: sequential
probe:
  allow_private: true
  interval_sec: 30
  timeout_sec: 10
logging:
  level: debug
"""


JAR = http.cookiejar.CookieJar()
OP = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(JAR))


def http(method, path, body=None, csrf=None, timeout=20):
    data = json.dumps(body).encode() if body is not None else None
    h = {"Accept": "application/json"}
    if data:
        h["Content-Type"] = "application/json"
    if csrf:
        h["X-CSRF-Token"] = csrf
    r = urllib.request.Request(BASE + path, data=data, headers=h, method=method)
    try:
        with OP.open(r, timeout=timeout) as resp:
            raw = resp.read()
            return resp.status, dict(resp.headers), (json.loads(raw) if raw else {})
    except urllib.error.HTTPError as e:
        raw = e.read()
        try:
            return e.code, dict(e.headers), json.loads(raw or b"{}")
        except Exception:
            return e.code, dict(e.headers), {"raw": raw[:200].decode("utf-8", "replace")}


def main():
    subprocess.run([sys.executable, "/opt/data/cache/scratch/r5/clean.py"], capture_output=True)
    shutil.rmtree(ROOT, ignore_errors=True)
    os.makedirs(f"{ROOT}/data", exist_ok=True)
    os.makedirs(f"{ROOT}/logs", exist_ok=True)
    os.makedirs(EVID, exist_ok=True)
    open(f"{ROOT}/config.yaml", "w").write(CONFIG)

    behA = {"probe/app": {"mode": "ok", "size": 4096},
            "team/app": {"mode": "ok", "size": 4096}}
    behB = {"probe/app": {"mode": "ok", "size": 4096, "variant": "B"},
            "team/app": {"mode": "ok", "size": 4096, "variant": "B"}}
    behU = {"probe/app": {"mode": "ok", "size": 4096, "variant": "U"}}
    ups = []
    for name, port, beh in (("A", 19201, behA), ("B", 19202, behB), ("U", 19203, behU)):
        p = f"{ROOT}/beh{name}.json"
        json.dump(beh, open(p, "w"))
        lf = open(f"{ROOT}/logs/up{name}.log", "ab")
        ups.append(subprocess.Popen([sys.executable, "/opt/data/cache/scratch/r5/fake_upstream.py",
                                     "--port", str(port), "--name", name, "--log", f"{ROOT}/logs/up{name}.jsonl",
                                     "--behavior-map", p], stdout=lf, stderr=subprocess.STDOUT))
    time.sleep(0.6)

    env = dict(os.environ)
    env.update({"ML_HOSTPROBE_REPOS": "gcr.io=probe/app,quay.io=probe/app", "ML_HOSTPROBE_INTERVAL": "2m"})
    lf = open(f"{ROOT}/logs/mirror-lens.log", "ab")
    p = subprocess.Popen([BIN, "-config", f"{ROOT}/config.yaml"], env=env,
                         stdout=lf, stderr=subprocess.STDOUT, cwd=ROOT)
    for _ in range(200):
        try:
            urllib.request.urlopen(BASE + "/api/me", timeout=1).read()
            break
        except urllib.error.HTTPError:
            break
        except Exception:
            time.sleep(0.15)

    out = {"steps": {}}
    http("POST", "/api/setup", {"user": USER, "pass": PASS})
    st, _, j = http("POST", "/api/login", {"user": USER, "pass": PASS})
    csrf = http("GET", "/api/me")[2].get("csrf", "")
    out["steps"]["login"] = st

    ids = {}
    for name, port, host in (("A", 19201, "gcr.io"), ("B", 19202, "quay.io"), ("U", 19203, "quay.io")):
        st, _, j = http("POST", "/api/sources", {"name": f"b17-{name}", "url": f"http://127.0.0.1:{port}", "host": host}, csrf)
        ids[name] = j.get("id")
        if name != "U":
            http("PATCH", f"/api/sources/{j['id']}", {"preferred": True}, csrf)
        out["steps"][f"source_{name}"] = {"status": st, "body": j}
    out["ids"] = ids

    # Host→Source 可信映射（受控上游要成为该 Host 的候选，必须登记）
    for name, host in (("A", "gcr.io"), ("B", "quay.io")):
        st, _, j = http("POST", "/api/trusted-mappings",
                        {"registry_host": host, "source_id": ids[name], "priority": 100}, csrf)
        out["steps"][f"mapping_{name}"] = {"status": st, "body": j}

    # 副本映射：gcr.io/team/app -> quay.io/team/app
    st, _, j = http("POST", "/api/registry-replicas", {
        "authority_registry": "gcr.io", "authority_repository": "team/app",
        "replica_registry": "quay.io", "replica_repository": "team/app", "priority": 100,
    }, csrf)
    out["steps"]["replica"] = {"status": st, "body": j}

    # 夹具：受控上游无法完成产品级 Host 探活（探针调度未把这批源列入 due），
    # 按探针成功时写入的完全相同字段补 recovery + health 行，并在报告里标注为夹具操作。
    import sqlite3
    con = sqlite3.connect(f"{ROOT}/data/mirror-lens.db")
    now = int(time.time())
    for name, host in (("A", "gcr.io"), ("B", "quay.io")):
        sid = ids[name]
        con.execute("UPDATE registry_host_recovery SET state='ok', gen=0, last_probe_at=?, last_success_at=?,"
                    " next_probe_at=?, rec_failures=0, retry_failures=0, last_error='', updated_at=?"
                    " WHERE source_id=? AND scope=? AND family='registry'",
                    (now, now, now + 3600, now, sid, host))
        if con.execute("SELECT 1 FROM registry_host_recovery WHERE source_id=? AND scope=?", (sid, host)).fetchone() is None:
            con.execute("INSERT INTO registry_host_recovery(source_id,family,scope,state,gen,last_probe_at,"
                        "last_success_at,next_probe_at,rec_failures,retry_failures,last_error,updated_at)"
                        " VALUES(?,'registry',?,'ok',0,?,?,?,0,0,'',?)", (sid, host, now, now, now + 3600, now))
        if con.execute("SELECT 1 FROM source_health_scope WHERE source_id=? AND family='registry' AND scope=?",
                       (sid, host)).fetchone():
            con.execute("UPDATE source_health_scope SET fails=0, ok_at=?, last_at=?"
                        " WHERE source_id=? AND family='registry' AND scope=?", (now, now, sid, host))
        else:
            con.execute("INSERT INTO source_health_scope(source_id,family,scope,gen,fails,ok_at,last_at)"
                        " VALUES(?,'registry',?,0,0,?,?)", (sid, host, now, now))
    con.commit()
    con.close()
    out["steps"]["fixture_recovery_rows"] = True
    time.sleep(3)

    # 等两个 Host 探活通过
    deadline = time.time() + 90
    caps = []
    while time.time() < deadline:
        st, _, s = http("GET", "/api/sources")
        items = s.get("items") or s.get("sources") or []
        caps = [it for it in items if it.get("id") in ids.values() and it.get("status") in ("usable", "degraded")]
        if len(caps) >= 2:
            break
        time.sleep(2)
    out["steps"]["caps_ready"] = len(caps)

    # 浏览器 7 项
    node = subprocess.run(["node", "/opt/data/cache/scratch/r5/b17_browser.js", BASE, EXE, EVID],
                          capture_output=True, text=True, timeout=300,
                          env={**os.environ, "UNSTARRED_PORT": "19203"})
    out["browser"] = {"exit": node.returncode, "stdout": node.stdout[-8000:], "stderr": node.stderr[-4000:]}

    # 行为断言：参选为空 → 退化顺序（只问 1 个上游）；勾选后 → 真并行；
    # 撤销星标后即使存储里仍有该 id，也不再参与（命中数回落到 1）。
    def hits():
        n = 0
        for name in ("A", "B"):
            pth = f"{ROOT}/logs/up{name}.jsonl"
            if not os.path.exists(pth):
                continue
            for ln in open(pth, encoding="utf-8"):
                if ln.strip():
                    r = json.loads(ln)
                    if "/manifests/" in r.get("path", "") and r.get("method") == "GET":
                        n += 1
        return n

    def manifest(tag):
        r = urllib.request.Request(f"{BASE}/v2/gcr.io/team/app/manifests/{tag}", headers={"User-Agent": "r5b17"})
        t0 = time.time()
        try:
            with urllib.request.urlopen(r, timeout=30) as resp:
                b = resp.read()
                return {"status": resp.status, "ms": int((time.time() - t0) * 1000),
                        "src": resp.headers.get("X-ML-Source"), "cache": resp.headers.get("X-ML-Cache"),
                        "n": len(b)}
        except urllib.error.HTTPError as e:
            return {"status": e.code, "body": e.read()[:200].decode("utf-8", "replace")}

    # 浏览器第 6 步已把副本源（id=2, quay.io）撤星；先恢复星标以验证竞速，再撤星验证退出。
    http("PATCH", f"/api/sources/{ids['B']}", {"preferred": True}, csrf)
    time.sleep(1.0)

    # A. 顺序模式 + 空参选（退化）：只应问 1 个上游
    http("PUT", "/api/cross-registry", {"mode": "sequential", "selected": []}, csrf)
    h0 = hits()
    r_seq = manifest("t-seq-" + str(int(time.time())))
    h1 = hits()

    # B. 竞速 + 勾选副本源：应并行问 2 个上游
    st_sel, _, sel_body = http("PUT", "/api/cross-registry", {"mode": "race", "selected": [ids["B"]]}, csrf)
    r_race = manifest("t-race-" + str(int(time.time())))
    h2 = hits()

    # C. 撤销星标：存储里的 selected 可能仍含该 id，但不得再参与
    http("PATCH", f"/api/sources/{ids['B']}", {"preferred": False}, csrf)
    time.sleep(1.0)
    st_rej, _, rej_body = http("PUT", "/api/cross-registry", {"mode": "race", "selected": [ids["B"]]}, csrf)
    state_after = http("GET", "/api/cross-registry")[2]
    r_unstar = manifest("t-unstar-" + str(int(time.time())))
    h3 = hits()

    out["behaviour"] = {
        "seq": {"result": r_seq, "upstream_delta": h1 - h0},
        "race": {"result": r_race, "upstream_delta": h2 - h1, "put_selected": [st_sel, sel_body]},
        "after_unstar": {"result": r_unstar, "upstream_delta": h3 - h2,
                         "put_rejected": [st_rej, rej_body], "cross_state": state_after},
    }
    out["assertions"] = {
        "seq_single_upstream": {"ok": (h1 - h0) == 1, "detail": h1 - h0},
        "race_parallel_upstream": {"ok": (h2 - h1) >= 2, "detail": h2 - h1},
        "race_200": {"ok": r_race.get("status") == 200, "detail": r_race},
        "unstar_not_participating": {"ok": (h3 - h2) == 1, "detail": h3 - h2},
        "unstar_select_rejected": {"ok": st_rej >= 400, "detail": [st_rej, rej_body]},
    }

    # 收尾：截图目录与日志归档
    for name in os.listdir(f"{ROOT}/logs"):
        src = f"{ROOT}/logs/{name}"
        if os.path.isfile(src):
            shutil.copyfile(src, f"{EVID}/{name}")
    json.dump(out, open(f"{EVID}/b17-drill.json", "w"), ensure_ascii=False, indent=2)
    print(json.dumps(out.get("assertions"), ensure_ascii=False, indent=2))
    print("browser exit", node.returncode)

    p.send_signal(signal.SIGTERM)
    try:
        p.wait(timeout=20)
    except subprocess.TimeoutExpired:
        p.kill()
    for u in ups:
        u.terminate()


if __name__ == "__main__":
    main()
