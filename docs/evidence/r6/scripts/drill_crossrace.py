#!/usr/bin/env python3
"""B 层跨站竞速真实并行取证（B04/B18/B20）。

前置：所有 Source 资格都走产品自己的 Host 探针（不写夹具行）——
`source_host_caps.effective_status=Supported` 只能由 ProbeRegistryHost→CommitHostProbe 提交。

产出：候选诊断 JSON、双上游 JSONL、Gateway 日志、请求结果、Digest 校验、并行重叠判定。
"""
import hashlib
import json
import os
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import common as C  # noqa
from fake_upstream import manifest_for  # noqa

ROOT = f"{C.ROOT}/crossrace"
ADDR = "127.0.0.1:18430"
BASE = "http://" + ADDR
EVID = "/opt/data/cache/scratch/ml8/docs/evidence/r6/crossrace-runtime"
REPO = "team/app"          # 竞速用例（有副本映射）
SEQ_REPO = "seq/app"       # 空参选用例（独立仓库，避免命中竞速写下的缓存）
MAN_SIZE = 4096
DIGEST = "sha256:" + hashlib.sha256(manifest_for(REPO, MAN_SIZE)).hexdigest()
SEQ_DIGEST = "sha256:" + hashlib.sha256(manifest_for(SEQ_REPO, MAN_SIZE)).hexdigest()

CONFIG = f"""\
addr: "{ADDR}"
data: "{ROOT}/data"
no_seed: true
registry:
  enabled: true
  docker_enabled: true
  ghcr_enabled: true
  upstream: "registry-1.docker.io"
  multi_origin_hosts: [gcr.io, quay.io, registry.k8s.io]
  max_file_mb: 64
  tag_ttl: 5m
  replica_mode: sequential
sources:
  enabled: true
probe:
  allow_private: true
  interval_sec: 30
  timeout_sec: 10
logging:
  level: debug
"""


def metrics(base):
    try:
        import urllib.request
        with urllib.request.urlopen(base + "/metrics", timeout=5) as r:
            txt = r.read().decode()
    except Exception as e:
        return {"error": str(e)}
    out = {}
    for ln in txt.splitlines():
        if ln.startswith("#") or not ln.strip():
            continue
        for k in ("attempts_started_total", "attempts_finished_total", "attempts_active",
                  "hedge_extra_requests_total"):
            if ln.startswith("mirror_lens_" + k) or ln.split("{")[0].endswith(k):
                out[ln.split("{")[0]] = ln.rsplit(" ", 1)[-1]
    return out


def hits(path, kind="manifest", repo=REPO):
    rows = [r for r in C.read_jsonl(path) if r.get("kind") == kind]
    if repo is not None:
        rows = [r for r in rows if r.get("repo") == repo]
    return rows


def main():
    os.makedirs(EVID, exist_ok=True)
    C.fresh_root(ROOT)
    ups = []
    ups.append((("A"), 19601, {"probe/app": {"mode": "ok", "size": 4096},
                               REPO: {"mode": "ok", "size": MAN_SIZE}}))
    ups.append((("B"), 19602, {"probe/app": {"mode": "ok", "size": 4096},
                               REPO: {"mode": "slowhdr", "delay_ms": 40, "size": MAN_SIZE},
                               "mism/app": {"mode": "ok", "size": MAN_SIZE, "variant": "B"}}))
    ups.append((("C"), 19603, {"probe/app": {"mode": "ok", "size": 4096},
                               REPO: {"mode": "slowhdr", "delay_ms": 160, "size": MAN_SIZE},
                               "mism/app": {"mode": "ok", "size": MAN_SIZE, "variant": "C"}}))
    procs = [C.start_fake(n, p, b, ROOT) for n, p, b in [(x[0], x[1], x[2]) for x in ups]]
    time.sleep(0.6)

    env = {
        "ML_HOSTPROBE_REPOS": "gcr.io=probe/app,quay.io=probe/app,registry.k8s.io=probe/app",
        "ML_HOSTPROBE_SCAN": "2s",
        "ML_HOSTPROBE_INTERVAL": "5m",
    }
    gw = C.start_gateway(CONFIG, ROOT, env)
    assert C.wait_up(BASE), "gateway 未就绪"
    cl = C.Client(BASE)
    cl.login()

    ids = {}
    for name, port, host in (("A", 19601, "gcr.io"), ("B", 19602, "quay.io"), ("C", 19603, "registry.k8s.io")):
        st, _, j = cl.req("POST", "/api/sources", {"name": f"r6-{name}", "url": f"http://127.0.0.1:{port}", "host": host})
        ids[name] = j.get("id")
        cl.req("PATCH", f"/api/sources/{ids[name]}", {"preferred": True})

    ok, snap = C.wait_caps_ready(f"{ROOT}/data/mirror-lens.db",
                                 [(ids["A"], "gcr.io"), (ids["B"], "quay.io"), (ids["C"], "registry.k8s.io")],
                                 timeout=150)
    diag = {"caps_ready": ok, "digest": DIGEST, "ids": ids}
    if not ok:
        diag["snapshot"] = snap
        json.dump(diag, open(f"{EVID}/crossrace-diag.json", "w"), ensure_ascii=False, indent=2)
        raise SystemExit("Host 探针未提交 Supported，先看 crossrace-diag.json")

    for name, host in (("A", "gcr.io"), ("B", "quay.io"), ("C", "registry.k8s.io")):
        st, _, j = cl.req("POST", "/api/trusted-mappings",
                          {"registry_host": host, "source_id": ids[name], "priority": 100})
        diag[f"mapping_{name}"] = [st, j]
    for host in ("quay.io", "registry.k8s.io"):
        st, _, j = cl.req("POST", "/api/registry-replicas", {
            "authority_registry": "gcr.io", "authority_repository": REPO,
            "replica_registry": host, "replica_repository": REPO,
            "priority": 100 if host == "quay.io" else 200,
        })
        diag[f"replica_{host}"] = [st, j]

    paths = {n: f"{ROOT}/logs/up{n}.jsonl" for n in "ABC"}

    def fetch_manifest(label, repo=REPO, digest=DIGEST):
        url = f"{BASE}/v2/gcr.io/{repo}/manifests/{digest}"
        started = time.time()
        import urllib.error
        import urllib.request
        t0 = time.time()
        try:
            with urllib.request.urlopen(urllib.request.Request(url, headers={"User-Agent": "r6-drill"}), timeout=60) as r:
                body = r.read()
                return {"label": label, "status": r.status, "ms": int((time.time() - t0) * 1000), "start": started,
                        "headers": {k.lower(): v for k, v in r.headers.items()
                                    if k.lower() in ("x-ml-source", "x-ml-cache", "content-length",
                                                     "content-type", "x-request-id")},
                        "bytes": len(body), "sha256": hashlib.sha256(body).hexdigest(),
                        "digest_ok": (hashlib.sha256(body).hexdigest() == digest.split(":", 1)[1]
                                      if digest.startswith("sha256:") else True)}
        except urllib.error.HTTPError as e:
            return {"label": label, "status": e.code, "body": e.read()[:200].decode("utf-8", "replace"),
                    "ms": int((time.time() - t0) * 1000), "start": started,
                    "headers": {k.lower(): v for k, v in e.headers.items()
                                if k.lower() in ("x-ml-source", "x-ml-cache", "x-request-id")}}
        except Exception as e:
            return {"label": label, "status": -1, "read_error": f"{type(e).__name__}: {e}",
                    "ms": int((time.time() - t0) * 1000)}

    res = {}
    # A. 空参选：不派发副本，按固定顺序回退（只问官方 + 该 Host 的顺序候选 A）
    st, _, j = cl.req("PUT", "/api/cross-registry", {"mode": "sequential", "selected": []})
    res["empty_selection_put"] = [st, j]
    n0 = {n: len(hits(paths[n], repo=SEQ_REPO)) for n in "ABC"}
    r_empty = fetch_manifest("empty-selection", SEQ_REPO, SEQ_DIGEST)
    n1 = {n: len(hits(paths[n], repo=SEQ_REPO)) for n in "ABC"}
    res["empty_selection"] = {"result": r_empty, "delta": {n: n1[n] - n0[n] for n in "ABC"}}

    # B. 竞速 + 两个副本参选
    m0 = metrics(BASE)
    st, _, j = cl.req("PUT", "/api/cross-registry", {"mode": "race", "selected": [ids["B"], ids["C"]]})
    res["race_put"] = [st, j]
    n0 = {n: len(hits(paths[n])) for n in "ABC"}
    r_race = fetch_manifest("race")
    n1 = {n: len(hits(paths[n])) for n in "ABC"}
    m1 = metrics(BASE)
    res["race"] = {"result": r_race, "delta": {n: n1[n] - n0[n] for n in "ABC"}}

    # B2. 控制组：两副本内容不一致 → 必须真的问一次官方 Authority（用来证明
    #     「同意组没有派发官方」这个观察手段是灵敏的，而不是自说自话）。
    for h, var in (("quay.io", "B"), ("registry.k8s.io", "C")):
        cl.req("POST", "/api/registry-replicas", {
            "authority_registry": "gcr.io", "authority_repository": "mism/app",
            "replica_registry": h, "replica_repository": "mism/app",
            "priority": 100 if h == "quay.io" else 200})
    r_mism = fetch_manifest("mismatch-control", "mism/app", "latest")
    res["mismatch_control"] = {"result": r_mism}

    # C. 第二次：缓存命中，不得再派发
    n0 = {n: len(hits(paths[n])) for n in "ABC"}
    r_again = fetch_manifest("cache-hit")
    n1 = {n: len(hits(paths[n])) for n in "ABC"}
    res["cache_repeat"] = {"result": r_again, "delta": {n: n1[n] - n0[n] for n in "ABC"}}

    time.sleep(0.5)
    snap2 = C.snapshot(f"{ROOT}/data/mirror-lens.db")
    logl = C.read_log(f"{ROOT}/logs/mirror-lens.log")
    rid_race = r_race.get("headers", {}).get("x-request-id", "\0")
    rid_mism = r_mism.get("headers", {}).get("x-request-id", "\0")
    race_lines = [ln.split("msg=", 1)[-1] for ln in logl if rid_race in ln and "副本候选" in ln]
    mism_lines = [ln.split("msg=", 1)[-1] for ln in logl if rid_mism in ln and "副本候选" in ln]

    # 并行重叠判定：用上游 JSONL 的真实起止时间
    b_rows = hits(paths["B"])[-1:] or []
    c_rows = hits(paths["C"])[-1:] or []
    a_rows = hits(paths["A"])[-1:] or []
    overlap = None
    if b_rows and c_rows:
        b, c = b_rows[0], c_rows[0]
        overlap = {
            "b": {"t0": b["t0"], "t1": b["t1"], "dur_ms": round((b["t1"] - b["t0"]) * 1000, 1)},
            "c": {"t0": c["t0"], "t1": c["t1"], "dur_ms": round((c["t1"] - c["t0"]) * 1000, 1)},
            "c_started_before_b_finished": c["t0"] < b["t1"],
            "b_started_before_c_finished": b["t0"] < c["t1"],
            "start_skew_ms": round(abs(b["t0"] - c["t0"]) * 1000, 1),
            "finish_gap_ms": round(abs(b["t1"] - c["t1"]) * 1000, 1),
        }

    # 落败 Body：进程里不得还挂着 cache 临时/半成品文件的 fd
    open_fds = []
    fddir = f"/proc/{gw.pid}/fd"
    if os.path.isdir(fddir):
        for fd in os.listdir(fddir):
            try:
                tgt = os.readlink(os.path.join(fddir, fd))
            except OSError:
                continue
            if tgt.startswith(f"{ROOT}/data/cache/"):
                open_fds.append(tgt)
    # 落败 Body / 临时文件
    tmpdir = f"{ROOT}/data/cache/tmp"
    leftovers = []
    for base, _, files in os.walk(f"{ROOT}/data/cache"):
        for f in files:
            if f.endswith(".part") or "/tmp/" in base.replace("\\", "/"):
                leftovers.append(os.path.join(base, f))

    assertions = {
        "empty_selection_no_replica_dispatch": {"ok": res["empty_selection"]["delta"]["B"] == 0
                                                and res["empty_selection"]["delta"]["C"] == 0,
                                                "detail": res["empty_selection"]["delta"]},
        "empty_selection_no_auto_starred": {"ok": res["empty_selection"]["delta"]["A"] + res["empty_selection"]["delta"]["B"]
                                            + res["empty_selection"]["delta"]["C"] <= 1,
                                            "detail": {"delta": res["empty_selection"]["delta"], "result": r_empty}},
        "race_200_digest_ok": {"ok": r_race.get("status") == 200 and r_race.get("digest_ok"), "detail": r_race},
        "race_two_upstreams": {"ok": res["race"]["delta"]["B"] >= 1 and res["race"]["delta"]["C"] >= 1,
                               "detail": res["race"]["delta"]},
        "race_authority_not_dispatched": {"ok": (len(race_lines) >= 2
                                                 and not any("target=official" in ln for ln in race_lines)
                                                 and any("target=official" in ln and "status=401" in ln
                                                         for ln in mism_lines)
                                                 and r_mism.get("ms", 0) > 1500),
                                          "detail_ok_rule": "竞速请求无 target=official；控制组必有 target=official status=401 且耗时>1.5s",
                                          "detail": {"request_id": r_race.get("headers", {}).get("x-request-id"),
                                                     "race_ms": r_race.get("ms"),
                                                     "race_dispatch_lines": race_lines,
                                                     "control_ms": r_mism.get("ms"),
                                                     "control_dispatch_lines": mism_lines}},
        "race_parallel_overlap": {"ok": bool(overlap and overlap["c_started_before_b_finished"]
                                             and overlap["b_started_before_c_finished"]), "detail": overlap},
        "race_winner_in_window": {"ok": bool(overlap and 0 <= (c_rows[0]["t1"] - b_rows[0]["t1"]) * 1000 <= 200),
                                  "detail": None if not overlap else round(abs(b_rows[0]["t1"] - c_rows[0]["t1"]) * 1000, 1)},
        "race_source_header_is_winner": {"ok": "B" in (r_race.get("headers", {}).get("x-ml-source") or ""),
                                         "detail": r_race.get("headers")},
        "cache_repeat_hit_no_dispatch": {"ok": r_again.get("headers", {}).get("x-ml-cache") == "HIT"
                                         and sum(res["cache_repeat"]["delta"].values()) == 0,
                                         "detail": res["cache_repeat"]},
        "no_leftover_partial": {"ok": len(leftovers) == 0, "detail": leftovers},
        "loser_body_closed": {"ok": len(open_fds) == 0, "detail": open_fds},
        "single_cache_entry_for_digest": {"ok": len([e for e in snap2["cache_entries"]
                                                     if DIGEST.split(":", 1)[1] in e["sha256"]]) == 1,
                                          "detail": snap2["cache_entries"]},
    }

    # 逐候选诊断（每道门都写实测状态）
    cand_diag = []
    for name, host in (("A", "gcr.io"), ("B", "quay.io"), ("C", "registry.k8s.io")):
        sid = ids[name]
        cap = next((r for r in snap2["caps"] if r["source_id"] == sid and r["host"] == host), None)
        rec = next((r for r in snap2["recovery"] if r["source_id"] == sid and r["scope"] == host), None)
        hp = next((r for r in snap2["policy"] if r["source_id"] == sid and r["host"] == host), None)
        hs = next((r for r in snap2["health_scope"] if r["source_id"] == sid and r["scope"] == host), None)
        src = next((r for r in snap2["sources"] if r["id"] == sid), None)
        mapped = any(m["registry_host"] == host and m["source_id"] == sid for m in snap2["mappings"])
        cand_diag.append({
            "source": name, "source_id": sid, "host": host,
            "caps": cap, "recovery": rec, "realm_policy": hp, "health_scope": hs,
            "source_status": src and src["status"], "preferred": src and src["preferred"],
            "trusted_source_mapping": mapped,
            "in_replica_mapping": host in ("quay.io", "registry.k8s.io"),
            "effective_status_supported": bool(cap and cap["effective_status"] == "Supported"),
            "path_mode_known": bool(cap and cap["registry_path_mode"] in ("native", "host_prefix")),
            "recovery_ok": bool(rec and rec["state"] == "ok"),
            "health_fails": (hs or {}).get("fails"),
            "dispatched_in_race": res["race"]["delta"][name],
            "conclusion": ("进入 crossRace targets 并真实派发" if res["race"]["delta"][name]
                           else "未进入 crossRace targets（不是权威站，也没有副本映射）"),
        })

    out = {
        "digest": DIGEST, "seq_digest": SEQ_DIGEST, "repo": REPO, "seq_repo": SEQ_REPO, "ids": ids, "hosts": {"A": "gcr.io", "B": "quay.io", "C": "registry.k8s.io"},
        "requests": res, "assertions": assertions, "parallel": overlap,
        "metrics_before_race": m0, "metrics_after_race": m1,
        "candidates": cand_diag,
        "snapshot_after": {k: snap2[k] for k in ("caps", "recovery", "settings", "cache_entries",
                                                "mappings", "replicas", "health_scope")},
    }
    json.dump(out, open(f"{EVID}/crossrace-runtime.json", "w"), ensure_ascii=False, indent=2)

    # 原始材料
    for n in "ABC":
        for fn in (f"up{n}.jsonl", f"up{n}.log"):
            src = f"{ROOT}/logs/{fn}"
            if os.path.exists(src):
                with open(src, encoding="utf-8") as r, open(f"{EVID}/{fn}", "w", encoding="utf-8") as w:
                    w.write(r.read())
    open(f"{EVID}/mirror-lens.log", "w", encoding="utf-8").write("\n".join(logl) + "\n")
    json.dump({"lines": [ln for ln in logl if ("跨站" in ln or "副本" in ln or "竞速" in ln
                                              or "回源" in ln or "换下一个" in ln or "Host " in ln)]},
              open(f"{EVID}/gateway-lines.json", "w"), ensure_ascii=False, indent=2)

    C.stop_proc(gw)
    for p in procs:
        C.stop_proc(p)
    print(json.dumps(assertions, ensure_ascii=False, indent=2))
    print("EVID:", EVID)
    bad = [k for k, v in assertions.items() if not v["ok"]]
    print("FAILED:", bad)


if __name__ == "__main__":
    main()
