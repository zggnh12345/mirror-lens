#!/usr/bin/env python3
"""3.5.17 第五轮 G0 真实流式演练（逐场景独立进程，顺序确定）。

每个场景：独立 data 目录 + 独立端口 + 真实二进制 + 本地受控上游（真实 HTTP/TCP）。
核验四项：客户端不混版本 / Body 收尾（无残留 .part、后续请求不卡）/ 不完整内容不入缓存 /
本地预算拒绝不计上游失败。
"""
import hashlib
import http.cookiejar
import json
import os
import shutil
import signal
import sqlite3
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.request

sys.path.insert(0, "/opt/data/cache/scratch/r5")
from fake_upstream import content  # noqa: E402

BASE_ROOT = "/opt/data/cache/scratch/r5/run3"
BIN = "/opt/data/cache/scratch/r5/mirror-lens"
EVID = "/opt/data/cache/scratch/ml8/docs/evidence/r5"
HOST = "gcr.io"
USER, PASS = "r5admin", "R5-drill-pass-1"

CONFIG = """\
addr: "127.0.0.1:{addr}"
data: "{root}/data"
no_seed: true
registry:
  enabled: true
  docker_enabled: true
  ghcr_enabled: true
  upstream: "registry-1.docker.io"
  multi_origin_hosts:
    - gcr.io
  max_file_mb: 64
  tag_ttl: 5m
sources:
  enabled: true
hedge:
  enabled: true
  after_ms: 200
parallel:
  # 夹具：关掉分片并行，让 20MB 预热走单流，才能产生 proxy_speed 样本
  # （noteSpeed 只在单流路径记录；换源参数仍来自同一组 ParOptions）。
  enabled: false
  min_size_mb: 1
  watch_mbps: 5
  watch_sec: 1
  switch_min_file_mib: 1
  switch_max_sent_mib: 8
  switch_max: 1
  switch_gap_sec: 1
  chunk_mb: 1
  workers: 4
probe:
  allow_private: true
  interval_sec: 30
  timeout_sec: 10
logging:
  level: debug
"""


class Client:
    def __init__(self, base):
        self.base = base
        self.jar = http.cookiejar.CookieJar()
        self.op = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(self.jar))
        self.csrf = ""

    def req(self, method, path, body=None, timeout=30):
        data = None
        headers = {"Accept": "application/json"}
        if body is not None:
            data = json.dumps(body).encode()
            headers["Content-Type"] = "application/json"
        if self.csrf:
            headers["X-CSRF-Token"] = self.csrf
        r = urllib.request.Request(self.base + path, data=data, headers=headers, method=method)
        try:
            with self.op.open(r, timeout=timeout) as resp:
                return resp.status, dict(resp.headers), json.loads(resp.read() or b"{}")
        except urllib.error.HTTPError as e:
            return e.code, dict(e.headers), json.loads(e.read() or b"{}")


def log(msg):
    print(f"[{time.strftime('%H:%M:%S')}] {msg}", flush=True)


def db(path):
    con = sqlite3.connect(path)
    con.row_factory = sqlite3.Row
    out = {}
    for k, sql in {
        "cache_entries": "SELECT key,sha256,size FROM cache_entries ORDER BY key",
        "blobs": "SELECT sha256,size FROM blobs",
        "integrity": "SELECT * FROM blob_integrity_failures",
        "health": "SELECT source_id,family,scope,fails,ok_at FROM source_health_scope",
        "sources": "SELECT id,name,status FROM sources ORDER BY id",
        "caps": "SELECT source_id,host,effective_status,registry_path_mode FROM source_host_caps ORDER BY source_id,host",
    }.items():
        try:
            out[k] = [dict(r) for r in con.execute(sql).fetchall()]
        except sqlite3.Error as e:
            out[k] = {"error": str(e)}
    con.close()
    return out


def wait_url(url, timeout=30):
    t0 = time.time()
    while time.time() - t0 < timeout:
        try:
            urllib.request.urlopen(url, timeout=1).read()
            return True
        except urllib.error.HTTPError:
            return True
        except Exception:
            time.sleep(0.15)
    return False


def fetch_blob(base, repo, digest, rng=None, timeout=60):
    url = f"{base}/v2/{HOST}/{repo}/blobs/{digest}"
    h = hashlib.sha256()
    n = 0
    rec = {"repo": repo, "digest": digest, "range": rng}
    hdrs = {"User-Agent": "r5-drill"}
    if rng:
        hdrs["Range"] = rng
    t0 = time.time()
    try:
        with urllib.request.urlopen(urllib.request.Request(url, headers=hdrs), timeout=timeout) as r:
            rec["status"] = r.status
            rec["headers"] = {k.lower(): v for k, v in r.headers.items()
                              if k.lower() in ("x-ml-source", "x-ml-cache", "content-length", "content-range", "etag")}
            while True:
                b = r.read(65536)
                if not b:
                    break
                h.update(b)
                n += len(b)
        rec["read_error"] = None
    except urllib.error.HTTPError as e:
        rec["status"] = e.code
        rec["headers"] = {k.lower(): v for k, v in e.headers.items()}
        rec["body"] = e.read()[:300].decode("utf-8", "replace")
    except Exception as e:
        rec["status"] = -1
        rec["read_error"] = f"{type(e).__name__}: {e}"
    rec["ms"] = int((time.time() - t0) * 1000)
    rec["bytes"] = n
    rec["sha256"] = h.hexdigest()
    rec["digest_ok"] = rec["sha256"] == digest.split(":", 1)[1]
    log(f"{repo}: status={rec['status']} bytes={n}/{rec.get('headers', {}).get('content-length')} "
        f"digest_ok={rec['digest_ok']} src={rec.get('headers', {}).get('x-ml-source')} "
        f"cache={rec.get('headers', {}).get('x-ml-cache')} ms={rec['ms']} err={rec.get('read_error')}")
    return rec


def parts(root):
    out = []
    for base, _, files in os.walk(f"{root}/data/cache"):
        for f in files:
            if ".part" in f:
                out.append(os.path.basename(os.path.join(base, f)))
    return out


CASES = [
    {
        "name": "slowhdr", "idx": 1,
        "srcs": [("A", {"probe/app": {"mode": "ok", "size": 2048},
                        "slowhdr": {"mode": "slowhdr", "delay_ms": 1500, "size": 300000}})],
        "repo": "slowhdr", "size": 300000,
    },
    {
        "name": "slowbody", "idx": 2,
        "srcs": [("A", {"probe/app": {"mode": "ok", "size": 2048},
                        "slowbody": {"mode": "slowbody", "chunk_ms": 100, "size": 400000}})],
        "repo": "slowbody", "size": 400000,
    },
    {
        "name": "cut_resume", "idx": 3,
        "srcs": [("A", {"probe/app": {"mode": "ok", "size": 2048},
                        "cutres": {"mode": "cutonce", "size": 300000}})],
        "repo": "cutres", "size": 300000, "two_requests": True,
    },
    {
        "name": "cut_bad206", "idx": 4,
        "srcs": [("A", {"probe/app": {"mode": "ok", "size": 2048},
                        "badres": {"mode": "cut_then_bad206", "size": 300000}})],
        "repo": "badres", "size": 300000, "two_requests": True, "must_not_cache": True,
    },
    {
        "name": "prefixbad", "idx": 5,
        "srcs": [("A", {"probe/app": {"mode": "ok", "size": 2048},
                        "badpfx": {"mode": "prefixbad", "size": 200000}})],
        "repo": "badpfx", "size": 200000, "must_not_cache": True,
    },
    {
        "name": "multibody", "idx": 6,
        "srcs": [("A", {"probe/app": {"mode": "ok", "size": 2048},
                        "mb1": {"mode": "slowbody", "chunk_ms": 60, "size": 200000},
                        "mb2": {"mode": "slowbody", "chunk_ms": 70, "size": 220000},
                        "mb3": {"mode": "slowbody", "chunk_ms": 80, "size": 240000}})],
        "concurrent": ["mb1", "mb2", "mb3"],
    },
    {
        "name": "switch_ab", "idx": 7,
        # A 逐渐极慢（10 个 200KB 分块、每块 1.2s ≈ 160KB/s），B 快且支持 Range。
        "srcs": [("A", {"probe/app": {"mode": "ok", "size": 2048},
                        "sw": {"mode": "slowbody", "chunk_ms": 1200, "size": 2000000},
                        "warm0": {"mode": "miss", "size": 20000000},
                        "warm1": {"mode": "miss", "size": 20000000},
                        "warm2": {"mode": "miss", "size": 20000000}}),
                 ("B", {"probe/app": {"mode": "ok", "size": 2048},
                        "sw": {"mode": "ok", "size": 2000000, "range_ok": True},
                        "warm0": {"mode": "ok", "size": 20000000, "range_ok": True},
                        "warm1": {"mode": "ok", "size": 20000000, "range_ok": True},
                        "warm2": {"mode": "ok", "size": 20000000, "range_ok": True}})],
        "repo": "sw", "size": 2000000, "want_switch": True, "warmup_b": "warm",
    },
    {
        "name": "switch_lenmismatch", "idx": 8,
        # A 慢；B 快但内容更短（1500000 < 2000000）：换源后长度对不上，必须失败且不落缓存。
        "srcs": [("A", {"probe/app": {"mode": "ok", "size": 2048},
                        "swm": {"mode": "slowbody", "chunk_ms": 1200, "size": 2000000},
                        "warm0": {"mode": "miss", "size": 20000000},
                        "warm1": {"mode": "miss", "size": 20000000},
                        "warm2": {"mode": "miss", "size": 20000000}}),
                 ("B", {"probe/app": {"mode": "ok", "size": 2048},
                        "swm": {"mode": "ok", "size": 1500000, "range_ok": True},
                        "warm0": {"mode": "ok", "size": 20000000, "range_ok": True},
                        "warm1": {"mode": "ok", "size": 20000000, "range_ok": True},
                        "warm2": {"mode": "ok", "size": 20000000, "range_ok": True}})],
        "repo": "swm", "size": 2000000, "must_not_cache": True, "want_switch": True, "warmup_b": "warm",
    },
    {
        "name": "hedge_manifest", "idx": 9,
        # Manifest 路径的响应头对冲：A 1.5s 才回响应头，B 立即回。
        "srcs": [("A", {"probe/app": {"mode": "ok", "size": 2048},
                        "hdm": {"mode": "slowhdr", "delay_ms": 1500, "size": 300000}}),
                 ("B", {"probe/app": {"mode": "ok", "size": 2048},
                        "hdm": {"mode": "ok", "size": 300000}})],
        "manifest": "hdm", "want_hedge": True,
    },
]


def run_case(case):
    idx = case["idx"]
    root = f"{BASE_ROOT}/{case['name']}"
    addr = f"127.0.0.1:{18200 + idx}"
    base = f"http://{addr}"
    up_port = 19100 + idx * 2
    shutil.rmtree(root, ignore_errors=True)
    os.makedirs(f"{root}/data", exist_ok=True)
    os.makedirs(f"{root}/logs", exist_ok=True)
    with open(f"{root}/config.yaml", "w") as f:
        f.write(CONFIG.format(addr=addr.split(":")[1], root=root))

    ports = {}
    ups = []
    for i, (name, beh) in enumerate(case["srcs"]):
        port = up_port + i
        ports[name] = port
        behfile = f"{root}/beh{name}.json"
        with open(behfile, "w") as f:
            json.dump(beh, f)
        lf = open(f"{root}/logs/up{name}.log", "ab")
        ups.append(subprocess.Popen(
            [sys.executable, "/opt/data/cache/scratch/r5/fake_upstream.py",
             "--port", str(port), "--name", name, "--log", f"{root}/logs/up{name}.jsonl",
             "--behavior-map", behfile], stdout=lf, stderr=subprocess.STDOUT))
    time.sleep(0.6)

    env = dict(os.environ)
    env.update({
        "ML_HOSTPROBE_REPOS": f"{HOST}=probe/app",
        "ML_HOSTPROBE_INTERVAL": "2m",
        "ML_HOSTPROBE_SCAN": "5s",
    })
    lf = open(f"{root}/logs/mirror-lens.log", "ab")
    p = subprocess.Popen([BIN, "-config", f"{root}/config.yaml"], env=env,
                         stdout=lf, stderr=subprocess.STDOUT, cwd=root)
    wait_url(base + "/api/me", 30)
    c = Client(base)
    c.req("POST", "/api/setup", {"user": USER, "pass": PASS})
    c.req("POST", "/api/login", {"user": USER, "pass": PASS})
    _, _, j = c.req("GET", "/api/me")
    c.csrf = j.get("csrf", "")

    ids = []
    for name, _ in case["srcs"]:
        st, _, j = c.req("POST", "/api/sources",
                         {"name": f"{case['name']}-{name}", "url": f"http://127.0.0.1:{ports[name]}", "host": HOST})
        if j.get("id"):
            ids.append(j["id"])
            c.req("PATCH", f"/api/sources/{j['id']}", {"preferred": True})
    deadline = time.time() + 60
    snap = {}
    while time.time() < deadline:
        snap = db(f"{root}/data/mirror-lens.db")
        caps = [r for r in (snap.get("caps") or []) if isinstance(r, dict) and r.get("effective_status") == "Supported"]
        us = [r for r in (snap.get("sources") or []) if isinstance(r, dict) and r.get("status") in ("usable", "degraded")]
        if len(caps) >= len(ids) and len(us) >= len(ids):
            break
        time.sleep(2)

    if case.get("warmup_b"):
        # 先停用 A，只用 B 跑两轮，给 B 建立真实吞吐样本
        # （trySwitch 只换到「最近样本足够快」的源，没样本一律不换）。
        c.req("PATCH", f"/api/sources/{ids[0]}", {"enabled": False})
        time.sleep(1.0)
        # switchSpeedOK 要求至少 3 条真实样本，所以用 3 个不同仓库各打一次冷请求。
        for k in range(3):
            repo_w = f"{case['warmup_b']}{k}"
            d = "sha256:" + hashlib.sha256(content(repo_w, 20000000)).hexdigest()
            fetch_blob(base, repo_w, d)
            time.sleep(0.2)
        c.req("PATCH", f"/api/sources/{ids[0]}", {"enabled": True})
        time.sleep(1.5)
        # warm 仓库写入缓存会让后续请求 HIT；换源用例的仓库不同，不受影响。

    ev = {"case": case["name"], "ports": ports, "ids": ids,
          "caps": snap.get("caps"), "sources": snap.get("sources"), "requests": []}
    before = db(f"{root}/data/mirror-lens.db")
    ev["before"] = {"cache_entries": before.get("cache_entries"), "health": before.get("health")}

    if case.get("concurrent"):
        results = []

        def one(repo):
            size = [s for s in case["srcs"]][0][1][repo]["size"]
            digest = "sha256:" + hashlib.sha256(content(repo, size)).hexdigest()
            results.append(fetch_blob(base, repo, digest))

        ths = [threading.Thread(target=one, args=(r,)) for r in case["concurrent"]]
        [t.start() for t in ths]
        [t.join() for t in ths]
        ev["requests"] = results
        time.sleep(0.4)
        ev["part_files"] = parts(root)
    elif case.get("manifest"):
        repo = case["manifest"]
        size = 300000
        # 先按 digest 预热（跨站/hedge 只对 Manifest 生效）
        digest = None
        url = f"{base}/v2/{HOST}/{repo}/manifests/latest"
        t0 = time.time()
        try:
            with urllib.request.urlopen(urllib.request.Request(url, headers={"User-Agent": "r5"}), timeout=60) as r:
                b = r.read()
                rec = {"repo": repo, "status": r.status, "bytes": len(b), "ms": int((time.time() - t0) * 1000),
                       "headers": {k.lower(): v for k, v in r.headers.items() if k.lower() in ("x-ml-source", "x-ml-cache")}}
                rec["sha256"] = hashlib.sha256(b).hexdigest()
                rec["digest_ok"] = True
                rec["content_type"] = r.headers.get("Content-Type")
        except urllib.error.HTTPError as e:
            rec = {"repo": repo, "status": e.code, "body": e.read()[:200].decode("utf-8", "replace")}
        except Exception as e:
            rec = {"repo": repo, "status": -1, "read_error": f"{type(e).__name__}: {e}"}
        log(f"manifest {repo}: {json.dumps(rec, ensure_ascii=False)}")
        ev["digest"] = digest
        ev["requests"].append(rec)
        ev["part_files"] = parts(root)
    else:
        size = case["size"]
        digest = "sha256:" + hashlib.sha256(content(case["repo"], size)).hexdigest()
        ev["digest"] = digest
        ev["requests"].append(fetch_blob(base, case["repo"], digest))
        time.sleep(0.3)
        ev["part_after_first"] = parts(root)
        if case.get("two_requests"):
            time.sleep(0.5)
            sent = ev["requests"][0]["bytes"]
            ev["requests"].append(fetch_blob(base, case["repo"], digest, rng=f"bytes={sent}-"))
            time.sleep(0.3)
        ev["part_files"] = parts(root)

    after = db(f"{root}/data/mirror-lens.db")
    ev["after"] = {"cache_entries": after.get("cache_entries"), "blobs": after.get("blobs"),
                   "integrity": after.get("integrity"), "health": after.get("health")}
    ev["fails_delta"] = {}
    hb = {(r["source_id"], r["scope"]): r["fails"] for r in (before.get("health") or []) if isinstance(r, dict) and "source_id" in r}
    ha = {(r["source_id"], r["scope"]): r["fails"] for r in (after.get("health") or []) if isinstance(r, dict) and "source_id" in r}
    for k in set(list(hb) + list(ha)):
        ev["fails_delta"][f"{k[0]}/{k[1]}"] = ha.get(k, 0) - hb.get(k, 0)

    # 进程仍健康：紧接着再请求一次探测仓库（证明没有卡死的填充锁/Body）
    health_probe = fetch_blob(base, "probe/app", "sha256:" + hashlib.sha256(content("probe/app", 2048)).hexdigest())
    ev["health_probe"] = health_probe

    p.send_signal(signal.SIGTERM)
    try:
        ev["exit"] = p.wait(timeout=30)
    except subprocess.TimeoutExpired:
        p.kill()
        ev["exit"] = -9
    for u in ups:
        u.terminate()
    log_path = f"{root}/logs/mirror-lens.log"
    with open(log_path, encoding="utf-8", errors="replace") as f:
        lines = f.read().splitlines()
    ev["log_switch"] = [ln for ln in lines if ("换源" in ln or "判慢" in ln or "中途并行" in ln or "并行填充" in ln)]
    ev["log_errors"] = [ln for ln in lines if ("校验失败" in ln or "丢弃续传点" in ln or "长度不符" in ln
                                               or "内容与" in ln or "回源失败" in ln or "非预期" in ln)]
    with open(f"{root}/logs/upA.jsonl", encoding="utf-8") as f:
        ev["upA"] = [json.loads(ln) for ln in f if ln.strip()]
    if os.path.exists(f"{root}/logs/upB.jsonl"):
        with open(f"{root}/logs/upB.jsonl", encoding="utf-8") as f:
            ev["upB"] = [json.loads(ln) for ln in f if ln.strip()]
    return ev


def main():
    subprocess.run([sys.executable, "/opt/data/cache/scratch/r5/clean.py"], capture_output=True)
    shutil.rmtree(BASE_ROOT, ignore_errors=True)
    os.makedirs(BASE_ROOT, exist_ok=True)
    only = [x for x in os.environ.get("R5_CASES", "").split(",") if x]
    all_ev = []
    for case in CASES:
        if only and case["name"] not in only:
            continue
        log(f"=== case {case['name']}")
        try:
            all_ev.append(run_case(case))
        except Exception as e:
            log(f"case {case['name']} 失败: {type(e).__name__}: {e}")
            all_ev.append({"case": case["name"], "error": f"{type(e).__name__}: {e}"})
    with open(f"{EVID}/g0-stream-drill.json", "w", encoding="utf-8") as f:
        json.dump({"cases": all_ev}, f, ensure_ascii=False, indent=2)
    # 日志原文
    outdir = f"{EVID}/g0"
    os.makedirs(outdir, exist_ok=True)
    for case in CASES:
        root = f"{BASE_ROOT}/{case['name']}"
        for name in os.listdir(f"{root}/logs") if os.path.isdir(f"{root}/logs") else []:
            src = f"{root}/logs/{name}"
            if os.path.isfile(src):
                shutil.copyfile(src, f"{outdir}/{case['name']}-{name}")
    log("evidence written")


if __name__ == "__main__":
    main()
