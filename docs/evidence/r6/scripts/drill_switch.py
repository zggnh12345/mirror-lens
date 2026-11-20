#!/usr/bin/env python3
"""真实中途换源（onSlow → trySwitch → 第二个 Source 真发 Range）。

不降低任何生产门禁：minProxySpeedBytes=16MiB（常量）、switch_min_mbps=8（生产默认）、
watch_mbps=3 / watch_sec=10 / switch_min_file_mib=60 / switch_max_sent_mib=48 全部保持默认。
B 的换源资格来自 3 次真实 ≥16MiB 单流样本（写进 proxy_speed，由 Scheduler 读取）。
"""
import hashlib
import json
import os
import sys
import time
import urllib.error
import urllib.request

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import common as C  # noqa
from fake_upstream import content  # noqa

ROOT = f"{C.ROOT}/switch"
ADDR = "127.0.0.1:18440"
BASE = "http://" + ADDR
EVID = "/opt/data/cache/scratch/ml8/docs/evidence/r6/switch"
HOST = "gcr.io"
SW_REPO = "sw/app"
SW_SIZE = 64 << 20          # ≥ switch_min_file_mib(60MiB)
WARM_SIZE = 24 << 20        # ≥ minProxySpeedBytes(16MiB)
WARM_REPOS = ["warm/one", "warm/two", "warm/three"]

CONFIG = f"""\
addr: "{ADDR}"
data: "{ROOT}/data"
no_seed: true
registry:
  enabled: true
  docker_enabled: true
  ghcr_enabled: true
  upstream: "registry-1.docker.io"
  multi_origin_hosts: [gcr.io]
  max_file_mb: 512
  tag_ttl: 5m
sources:
  enabled: true
parallel:
  enabled: false          # 单流路径才会写 proxy_speed 样本
  workers: 4
  min_size_mb: 8
  chunk_mb: 4
  watch_mbps: 3
  watch_sec: 10
  switch_min_mbps: 8
  switch_min_file_mib: 60
  switch_max_sent_mib: 48
  switch_max: 2
  switch_gap_sec: 30
probe:
  allow_private: true
  interval_sec: 30
  timeout_sec: 10
logging:
  level: debug
"""


def fetch_blob(repo, digest, timeout=180):
    url = f"{BASE}/v2/{HOST}/{repo}/blobs/{digest}"
    h = hashlib.sha256()
    n = 0
    t0 = time.time()
    rec = {"repo": repo, "digest": digest}
    try:
        with urllib.request.urlopen(urllib.request.Request(url, headers={"User-Agent": "r6-switch"}), timeout=timeout) as r:
            rec["status"] = r.status
            rec["headers"] = {k.lower(): v for k, v in r.headers.items()
                              if k.lower() in ("content-length", "x-ml-source", "x-ml-cache")}
            while True:
                b = r.read(1 << 16)
                if not b:
                    break
                h.update(b)
                n += len(b)
        rec["read_error"] = None
    except urllib.error.HTTPError as e:
        rec["status"] = e.code
        rec["body"] = e.read()[:300].decode("utf-8", "replace")
    except Exception as e:
        rec["status"] = -1
        rec["read_error"] = f"{type(e).__name__}: {e}"
    rec["ms"] = int((time.time() - t0) * 1000)
    rec["bytes"] = n
    rec["sha256"] = h.hexdigest()
    rec["digest_ok"] = rec["sha256"] == digest.split(":", 1)[1]
    C.log(f"{repo}: status={rec['status']} bytes={n} digest_ok={rec['digest_ok']} ms={rec['ms']} err={rec.get('read_error')}")
    return rec


def main():
    os.makedirs(EVID, exist_ok=True)
    C.fresh_root(ROOT)
    slow_rate = 1 << 20  # 1MB/s < watch_mbps(3MB/s)，10s 窗口约 10MB，远小于 switch_max_sent(48MiB)
    behA = {"probe/app": {"mode": "ok", "size": 4096},
            SW_REPO: {"mode": "slowbody", "size": SW_SIZE, "chunk": 1 << 20, "chunk_ms": 1000}}
    behB = {"probe/app": {"mode": "ok", "size": 4096},
            SW_REPO: {"mode": "ok", "size": SW_SIZE, "range_ok": True}}
    for r in WARM_REPOS:
        # 真实单流 ≥16MiB，按 1MiB/25ms 发：上游耗时 ~600ms，下游阻塞占比远低于
        # 1/3，样本不会被 noteSpeed 的下游背压判据丢弃（loopback 全速时 24MiB
        # 只花 ~27ms，几乎全被算成阻塞）。
        behB[r] = {"mode": "slowbody", "size": WARM_SIZE, "range_ok": True,
                   "chunk": 1 << 20, "chunk_ms": 25}
    ups = [C.start_fake("A", 19621, behA, ROOT), C.start_fake("B", 19622, behB, ROOT)]
    time.sleep(0.6)
    gw = C.start_gateway(CONFIG, ROOT, {"ML_HOSTPROBE_REPOS": f"{HOST}=probe/app",
                                        "ML_HOSTPROBE_SCAN": "2s", "ML_HOSTPROBE_INTERVAL": "5m"})
    assert C.wait_up(BASE), "gateway 未就绪"
    cl = C.Client(BASE)
    cl.login()

    ids = {}
    for name, port in (("A", 19621), ("B", 19622)):
        st, _, j = cl.req("POST", "/api/sources", {"name": f"sw-{name}", "url": f"http://127.0.0.1:{port}", "host": HOST})
        ids[name] = j.get("id")
        cl.req("PATCH", f"/api/sources/{ids[name]}", {"preferred": True})
    ok, snap = C.wait_caps_ready(f"{ROOT}/data/mirror-lens.db", [(ids["A"], HOST), (ids["B"], HOST)], timeout=150)
    out = {"ids": ids, "caps_ready": ok, "slow_bytes_per_s": slow_rate}

    # A 排在前：建一个 rank=1 的分组把 A 放进去（产品自身的优先级模型）。
    st, _, g = cl.req("POST", "/api/groups", {"name": "first"})
    gid = g.get("id")
    st2, _, mv = cl.req("POST", "/api/group-members", {"group_id": gid, "ids": [ids["A"]]})
    out["group"] = {"create": [st, g], "members": [st2, mv]}
    assert mv.get("n") == 1, f"分组未真正生效: {mv}"

    # 预热 B：临时停用 A，只用 B 下 3 个 ≥16MiB 真实整份（写 proxy_speed）。
    cl.req("PATCH", f"/api/sources/{ids['A']}", {"enabled": False})
    time.sleep(1.2)
    warm = []
    for r in WARM_REPOS:
        d = "sha256:" + hashlib.sha256(content(r, WARM_SIZE)).hexdigest()
        warm.append(fetch_blob(r, d))
    cl.req("PATCH", f"/api/sources/{ids['A']}", {"enabled": True})
    time.sleep(1.5)
    out["warm"] = warm

    # 样本是异步落库：等到 B 的 ≥16MiB 样本满 3 条再往下走（有上限，避免死等）。
    ps = []
    for _ in range(40):
        _snap = C.snapshot(f"{ROOT}/data/mirror-lens.db")
        ps = [r for r in _snap["proxy_speed"] if r["source_id"] == ids["B"]]
        if len(ps) >= 3:
            break
        time.sleep(0.25)
    snap = C.snapshot(f"{ROOT}/data/mirror-lens.db")
    sp = [r["bytes"] // r["ms"] for r in ps if r["ms"] > 0]
    out["proxy_speed_B"] = {"rows": ps, "kb_per_s": sp,
                            "median_kb": sorted(sp)[len(sp) // 2] if sp else 0,
                            "switch_min_kbps": 8 * 1024,
                            "sample_gate_bytes": 16 << 20}

    digest = "sha256:" + hashlib.sha256(content(SW_REPO, SW_SIZE)).hexdigest()
    out["digest"] = digest
    a0 = len(C.read_jsonl(f"{ROOT}/logs/upA.jsonl"))
    b0 = len(C.read_jsonl(f"{ROOT}/logs/upB.jsonl"))
    # 重启 Gateway 清掉预热阶段形成的 scheduler 候选 ListTTL 缓存，让本次
    # 请求真正按「A 优先」排序；否则预热时的源顺序会被缓存复用。
    log_before = C.read_log(f"{ROOT}/logs/mirror-lens.log")
    C.stop_proc(gw)
    time.sleep(0.5)
    gw = C.start_gateway(CONFIG, ROOT, {"ML_HOSTPROBE_REPOS": f"{HOST}=probe/app",
                                        "ML_HOSTPROBE_SCAN": "30s", "ML_HOSTPROBE_INTERVAL": "5m"})
    assert C.wait_up(BASE), "重启后 gateway 未就绪"
    time.sleep(0.5)
    t0 = time.time()
    rec = fetch_blob(SW_REPO, digest, timeout=300)
    rec["wall_ms"] = int((time.time() - t0) * 1000)
    time.sleep(0.5)
    upA = [r for r in C.read_jsonl(f"{ROOT}/logs/upA.jsonl")[a0:] if r.get("repo") == SW_REPO]
    upB = [r for r in C.read_jsonl(f"{ROOT}/logs/upB.jsonl")[b0:] if r.get("repo") == SW_REPO]
    out["request"] = rec
    out["upA"] = upA
    out["upB"] = upB

    snap2 = C.snapshot(f"{ROOT}/data/mirror-lens.db")
    logl = log_before + C.read_log(f"{ROOT}/logs/mirror-lens.log")
    out["log_switch"] = [ln for ln in logl if ("判慢" in ln or "中途并行" in ln or "换源" in ln or "丢弃续传点" in ln)]
    out["cache_entries"] = snap2["cache_entries"]
    out["blobs"] = snap2["blobs"]
    parts = []
    for base, _, files in os.walk(f"{ROOT}/data/cache"):
        for f in files:
            if f.endswith(".part") or f.endswith(".meta"):
                parts.append(os.path.join(base, f))
    out["partials"] = parts

    b_range = [r for r in upB if r.get("status") == 206]
    assertions = {
        "warm_samples_real": {"ok": len(ps) >= 3 and all(r["bytes"] >= (16 << 20) for r in ps),
                              "detail": [(r["bytes"], r["ms"]) for r in ps]},
        "warm_median_over_gate": {"ok": bool(sp) and sorted(sp)[len(sp) // 2] >= 8 * 1024,
                                  "detail": sp},
        "switch_200_digest_ok": {"ok": rec.get("status") == 200 and rec.get("digest_ok"), "detail": rec},
        "second_source_range_request": {"ok": len(b_range) == 1 and "bytes=" in (b_range[0].get("range") or ""),
                                        "detail": b_range},
        "second_source_206_headers": {"ok": bool(b_range) and b_range[0].get("declared_total") == SW_SIZE
                                      and b_range[0].get("bytes", 0) > 0,
                                      "detail": b_range},
        "first_source_served_prefix_only": {"ok": bool(upA) and all(r.get("bytes", 0) < SW_SIZE for r in upA)
                                            and len(b_range) == 1,
                                            "detail": upA},
        "no_parallel_fallback": {"ok": not any("中途并行" in ln for ln in out["log_switch"]),
                                 "detail": out["log_switch"]},
        "no_partial_left": {"ok": len(parts) == 0, "detail": parts},
        "cache_entry_committed": {"ok": any(e["sha256"] == digest.split(":", 1)[1] for e in snap2["cache_entries"]),
                                  "detail": snap2["cache_entries"]},
        # 换源后本次 .part 是混合来源，生产规则是任何未完成退出都不留续传点；
        # 但成功收完 100% 时会提交一条完整缓存（upstream 字段记初始源，这是
        # 内容寻址的正常行为，不能写成「错误绑定旧 Source」）。这里只断言
        # 内容完整、没有 .part 残留、且只有一条缓存记录。
        "cross_source_no_partial": {"ok": len(parts) == 0 and rec.get("digest_ok"), "detail": parts},
        "single_cache_entry": {"ok": len([r for r in snap2["cache_entries"]
                                         if r["sha256"] == digest.split(":", 1)[1]]) == 1,
                               "detail": None},
    }
    out["assertions"] = assertions
    json.dump(out, open(f"{EVID}/switch-runtime.json", "w"), ensure_ascii=False, indent=2)
    for n in ("A", "B"):
        for fn in (f"up{n}.jsonl", f"up{n}.log"):
            src = f"{ROOT}/logs/{fn}"
            if os.path.exists(src):
                with open(src, encoding="utf-8") as r, open(f"{EVID}/{fn}", "w", encoding="utf-8") as w:
                    w.write(r.read())
    open(f"{EVID}/mirror-lens.log", "w", encoding="utf-8").write("\n".join(logl) + "\n")

    C.stop_proc(gw)
    for p in ups:
        C.stop_proc(p)
    print(json.dumps(assertions, ensure_ascii=False, indent=2))
    print("FAILED:", [k for k, v in assertions.items() if not v["ok"]])


if __name__ == "__main__":
    main()
