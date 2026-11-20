"""第六轮演练公共件：真实进程 + 受控上游 + DB 快照。"""
import http.cookiejar
import json
import os
import shutil
import signal
import sqlite3
import subprocess
import sys
import time
import urllib.error
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
FAKE = os.path.join(HERE, "fake_upstream.py")
ROOT = "/opt/data/cache/scratch/r6/run"
BIN = "/opt/data/cache/scratch/r6/bin/mirror-lens"
USER, PASS = "r6admin", "R6-drill-pass-1"


def log(msg):
    print(f"[{time.strftime('%H:%M:%S')}] {msg}", flush=True)


class Client:
    def __init__(self, base):
        self.base = base
        self.jar = http.cookiejar.CookieJar()
        self.op = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(self.jar))
        self.csrf = ""

    def req(self, method, path, body=None, timeout=30, headers=None):
        data = None
        h = {"Accept": "application/json"}
        if headers:
            h.update(headers)
        if body is not None:
            data = json.dumps(body).encode()
            h["Content-Type"] = "application/json"
        if self.csrf:
            h["X-CSRF-Token"] = self.csrf
        r = urllib.request.Request(self.base + path, data=data, headers=h, method=method)
        try:
            with self.op.open(r, timeout=timeout) as resp:
                raw = resp.read()
                try:
                    return resp.status, dict(resp.headers), json.loads(raw or b"{}")
                except Exception:
                    return resp.status, dict(resp.headers), {"raw": raw[:200].decode("utf-8", "replace")}
        except urllib.error.HTTPError as e:
            raw = e.read()
            try:
                return e.code, dict(e.headers), json.loads(raw or b"{}")
            except Exception:
                return e.code, dict(e.headers), {"raw": raw[:300].decode("utf-8", "replace")}

    def login(self):
        self.req("POST", "/api/setup", {"user": USER, "pass": PASS})
        self.req("POST", "/api/login", {"user": USER, "pass": PASS})
        self.csrf = self.req("GET", "/api/me")[2].get("csrf", "")


def wait_up(base, timeout=40):
    t0 = time.time()
    while time.time() - t0 < timeout:
        try:
            urllib.request.urlopen(base + "/api/me", timeout=1).read()
            return True
        except urllib.error.HTTPError:
            return True
        except Exception:
            time.sleep(0.15)
    return False


def dbq(path, sql, args=()):
    con = sqlite3.connect(path)
    con.row_factory = sqlite3.Row
    try:
        return [dict(r) for r in con.execute(sql, args).fetchall()]
    finally:
        con.close()


def snapshot(path):
    out = {}
    for k, sql in {
        "sources": "SELECT id,name,base_url,status,enabled,preferred,range_cap FROM sources ORDER BY id",
        "caps": "SELECT source_id,host,enabled,last_verdict,effective_status,registry_path_mode,diagnostics FROM source_host_caps ORDER BY source_id,host",
        "policy": "SELECT source_id,host,primary_realm,extra_realms,policy_gen FROM source_realm_policy ORDER BY source_id,host",
        "recovery": "SELECT source_id,family,scope,state,gen,last_probe_at,last_success_at,next_probe_at,last_error FROM registry_host_recovery ORDER BY source_id,scope",
        "health_scope": "SELECT source_id,family,scope,gen,fails,ok_at,last_at FROM source_health_scope ORDER BY source_id,scope",
        "host_gen": "SELECT * FROM registry_host_gen",
        "mappings": "SELECT * FROM trusted_source_mappings ORDER BY registry_host,source_id",
        "replicas": "SELECT * FROM trusted_registry_replicas ORDER BY authority_registry,priority",
        "settings": "SELECT key,value FROM settings WHERE key LIKE 'cross_registry%'",
        "cache_entries": "SELECT key,sha256,size,upstream FROM cache_entries ORDER BY key",
        "blobs": "SELECT sha256,size FROM blobs",
        "proxy_speed": "SELECT source_id,kind,ts,bytes,ms FROM proxy_speed ORDER BY ts",
        "schema_meta": "SELECT * FROM schema_meta",
        "integrity": "SELECT * FROM blob_integrity_failures",
    }.items():
        try:
            out[k] = dbq(path, sql)
        except sqlite3.Error as e:
            out[k] = {"error": str(e)}
    return out


def start_fake(name, port, behavior, root, env_extra=None):
    beh = f"{root}/beh{name}.json"
    with open(beh, "w", encoding="utf-8") as f:
        json.dump(behavior, f)
    lf = open(f"{root}/logs/up{name}.log", "ab")
    p = subprocess.Popen([sys.executable, FAKE, "--port", str(port), "--name", name,
                          "--log", f"{root}/logs/up{name}.jsonl", "--behavior-map", beh],
                         stdout=lf, stderr=subprocess.STDOUT, env={**os.environ, **(env_extra or {})})
    # 端口必须真的由本次进程提供：上一次超时留下的孤儿会占着端口，
    # 记录写到已被 rmtree 的旧文件里，表现为「有流量但日志为空」。
    import urllib.request
    for _ in range(60):
        try:
            urllib.request.urlopen(f"http://127.0.0.1:{port}/v2/", timeout=1).read()
            return p
        except Exception:
            if p.poll() is not None:
                raise RuntimeError(f"假上游 {name}:{port} 启动失败（退出码 {p.returncode}），端口可能被占用")
            time.sleep(0.1)
    raise RuntimeError(f"假上游 {name}:{port} 未就绪")


def start_gateway(cfg_text, root, env_extra=None):
    with open(f"{root}/config.yaml", "w", encoding="utf-8") as f:
        f.write(cfg_text)
    lf = open(f"{root}/logs/mirror-lens.log", "ab")
    env = dict(os.environ)
    env.update(env_extra or {})
    p = subprocess.Popen([BIN, "-config", f"{root}/config.yaml"], env=env,
                         stdout=lf, stderr=subprocess.STDOUT, cwd=root)
    return p


def stop_proc(p, sig=signal.SIGTERM, timeout=30):
    if p is None:
        return None
    if p.poll() is not None:
        return p.returncode
    p.send_signal(sig)
    try:
        return p.wait(timeout=timeout)
    except subprocess.TimeoutExpired:
        p.kill()
        return -9


def read_jsonl(path):
    if not os.path.exists(path):
        return []
    out = []
    with open(path, encoding="utf-8") as f:
        for ln in f:
            ln = ln.strip()
            if ln:
                out.append(json.loads(ln))
    return out


def read_log(path):
    if not os.path.exists(path):
        return []
    with open(path, encoding="utf-8", errors="replace") as f:
        return f.read().splitlines()


def wait_caps_ready(dbpath, want, timeout=120):
    """等真实 Host 探针提交 Supported（不写任何夹具行）。"""
    t0 = time.time()
    last = {}
    while time.time() - t0 < timeout:
        snap = snapshot(dbpath)
        rows = [r for r in (snap.get("caps") or []) if isinstance(r, dict)]
        ok = {(r["source_id"], r["host"]): r for r in rows if r.get("effective_status") == "Supported"}
        # 候选查询还要求 sources.status IN ('usable','degraded')（由 Source 级探针
        # 独立提交，晚于 Host 探针），只等 caps 会让夹具早跑几秒拿到空候选。
        usable = {r["id"]: r["status"] for r in (snap.get("sources") or []) if isinstance(r, dict)}
        missing = [k for k in want if k not in ok or usable.get(k[0]) not in ("usable", "degraded")]
        if not missing:
            return True, snap
        last = snap
        time.sleep(1.5)
    return False, last


def fresh_root(root):
    shutil.rmtree(root, ignore_errors=True)
    os.makedirs(f"{root}/data", exist_ok=True)
    os.makedirs(f"{root}/logs", exist_ok=True)
    return root
