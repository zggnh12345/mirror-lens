#!/usr/bin/env python3
"""P0 fix real-environment verification: rebuild A10 concurrency=8 scenario.
Goal: prove errRaceNoBudget no longer causes 401 official fallback after the
tried-rollback fix. Uses same fixture as a10_full_pair.py but with the fixed image.
Dynamic digests from source script (no hand-copy). Reports per-sample errors,
winner dist, and race counters (correct regex).
"""
import json, subprocess, sys, time, datetime, re

GW_CLI = "p0fix-cli"
GW_DB = "p0fix-db"
GW = "p0fix-gw"
OUT = "/opt/data/cache/scratch/ml8/docs/evidence/3.5.18/p0fix-real-verify-clean.json"
NET = "ml318-p0fix"

REQ = r'''
import urllib.request,time,sys,json
from concurrent.futures import ThreadPoolExecutor
a=json.loads(sys.argv[1]);digs=a['digs'];gw=a['gw'];conc=a['conc']
def one(d):
    u='http://%s:8080/v2/quay.io/library/alpine/manifests/%s'%(gw,d)
    req=urllib.request.Request(u,headers={'Accept':'application/vnd.docker.distribution.manifest.v2+json'})
    t0=time.perf_counter()
    try:
        r=urllib.request.urlopen(req,timeout=30)
        ttfb=(time.perf_counter()-t0)*1000;b=r.read()
        return {'ttfb_ms':round(ttfb,3),'src':r.headers.get('X-Ml-Source') or '-',
                'dig':r.headers.get('Docker-Content-Digest') or '-','want':d,
                'bytes':len(b),'status':r.status}
    except Exception as e:
        return {'error':str(e)[:200],'want':d,'status':0}
with ThreadPoolExecutor(max_workers=conc) as ex:
    res=list(ex.map(one,digs))
print('RESULTS|'+json.dumps(res))
'''

def dexec(c, code, arg=None):
    cmd = ["docker", "exec", c, "python3", "-c", code]
    if arg is not None:
        cmd.append(arg)
    return subprocess.run(cmd, capture_output=True, text=True, timeout=120)

def sh(c):
    return subprocess.run(c, shell=True, capture_output=True, text=True)

def setup():
    sh("docker network rm %s >/dev/null 2>&1" % NET)
    sh("docker network create %s >/dev/null 2>&1" % NET)
    for c in [GW, GW_CLI, "ml318-src-fast", "ml318-src-slow", GW_DB]:
        sh("docker rm -f %s >/dev/null 2>&1" % c)
    sh("docker volume rm ml318-p0fix-data >/dev/null 2>&1")
    cfg = "/opt/data/cache/scratch/ml8/docs/evidence/3.5.18/nest-config.yaml"
    srcpy = "/opt/data/cache/scratch/ml8/docs/evidence/3.5.18/race_source.py"
    specs = [
        ("ml318-src-fast", "0", "0.2", "300"),
        ("ml318-src-slow", "20", "0", "0"),
    ]
    for name, base, tailp, tailms in specs:
        sh("docker create --name %s --network %s -e SRC_BASE_MS=%s -e SRC_TAIL_P=%s -e SRC_TAIL_MS=%s python:3.12-alpine python /tmp/race_src.py >/dev/null" % (name, NET, base, tailp, tailms))
        sh("docker cp %s %s:/tmp/race_src.py >/dev/null" % (srcpy, name))
        sh("docker start %s >/dev/null" % name)
    # 网关：先起一个占位容器建卷，再 docker cp 配置进去，最后起真网关
    sh("docker create --name %s --network %s -v ml318-p0fix-data:/data mirror-lens:3.5.18-p0fix >/dev/null" % (GW, NET))
    sh("docker cp %s %s:/data/config.yaml >/dev/null" % (cfg, GW))
    sh("docker start %s >/dev/null" % GW)
    # db 工具容器 + cli 工具容器：同卷，docker cp 脚本
    sh("docker run -d --name %s --network %s -v ml318-p0fix-data:/data python:3.12-alpine sleep infinity >/dev/null" % (GW_DB, NET))
    sh("docker run -d --name %s --network %s python:3.12-alpine sleep infinity >/dev/null" % (GW_CLI, NET))
    sh("docker cp %s %s:/tmp/race_src.py >/dev/null" % (srcpy, GW_CLI))
    time.sleep(10)


def seed():
    # 复用已验证的 race-fixture.sql（两源 caps/recovery/mapping 齐全）
    sql = open("/opt/data/cache/scratch/ml8/docs/evidence/3.5.18/race-fixture.sql").read()
    # sqlite3 没有 CLI，包成 Python 执行；executescript 兼容 PRAGMA/BEGIN/COMMIT
    pycode = "import sqlite3;db=sqlite3.connect('/data/mirror-lens.db');db.executescript(%r);db.commit()" % sql
    pr = dexec(GW_DB, pycode)
    print('SEED SQL rc', pr.returncode, pr.stderr[:300] if pr.returncode else 'ok')
    # 星标开关 + host 竞速开关 + 选中源（fixture 源 id 由 SQL 插入）
    code = r"""
import sqlite3, json
db = sqlite3.connect('/data/mirror-lens.db')
ids = [r[0] for r in db.execute("select id from sources where origin='fixture' order by id")]
for i in ids:
    db.execute("update sources set preferred=1 where id=?", (i,))
db.execute("insert or replace into settings(key,value) values('cross_registry_selected_sources',?)", (json.dumps(ids),))
db.execute("insert or replace into settings(key,value) values('registry_star_race:quay.io','1')")
db.commit()
print('SEEDED', ids)
"""
    p = dexec(GW_DB, code)
    print(p.stdout.strip() or p.stderr[:500])
    # 不重启网关：重启会触发 probe reconcile 清掉 fixture caps。
    # star 快照 TTL 10s，等待自然刷新即可。
    time.sleep(12)


def get_digests():
    p = dexec(GW_CLI, "import sys;sys.path.insert(0,'/tmp');import race_src;print(','.join(race_src.BY_TAG[t][1] for t in race_src.TAGS))")
    return [d.strip() for d in p.stdout.strip().split(',') if d.strip()]

def clear_state():
    code = ("import sqlite3,glob,os;db=sqlite3.connect('/data/mirror-lens.db');"
            "db.execute('DELETE FROM cache_entries');db.execute('DELETE FROM blobs');"
            "db.execute('DELETE FROM breaker_state');db.execute('DELETE FROM source_events');"
            "db.commit();[os.remove(f) for f in glob.glob('/data/cache/blobs/*/*')]")
    dexec(GW_DB, code)

def get_metrics():
    p = dexec(GW_CLI, "import urllib.request;print(urllib.request.urlopen('http://%s:8080/metrics',timeout=10).read().decode())" % GW)
    d = {}
    for l in p.stdout.splitlines():
        m = re.match(r'^mirrorlens_registry_race_(\w+)\{([^}]*)\}\s+(\d+(?:\.\d+)?)$', l)
        if m:
            name, labels, val = m.group(1), m.group(2), float(m.group(3))
            key = name
            if name == 'wins_total':
                rk = re.search(r'rank="(\d+)"', labels)
                key = 'wins_rank' + (rk.group(1) if rk else '?')
            d[key] = d.get(key, 0) + val
            continue
        m2 = re.match(r'^mirrorlens_registry_race_(\w+_total)\s+(\d+(?:\.\d+)?)$', l)
        if m2:
            d[m2.group(1)] = float(m2.group(2))
    return d

def main():
    print("SETUP", flush=True)
    setup()
    print("SEED", flush=True)
    seed()
    digs = get_digests()
    print("DIGESTS", len(digs), flush=True)
    if len(digs) != 8:
        print("FATAL digests", digs, flush=True)
        return
    samples = []
    for i in range(10):
        clear_state()
        args = {'digs': digs[:8], 'gw': GW, 'conc': 8}
        p = dexec(GW_CLI, REQ, json.dumps(args))
        line = [l for l in p.stdout.splitlines() if l.startswith('RESULTS|')]
        if not line:
            samples.append({'round': i+1, 'error': (p.stdout+p.stderr)[:200]})
            print(f"r{i+1}: ERROR", flush=True)
            continue
        res = json.loads(line[0][8:])
        for j, s in enumerate(res):
            s['round'] = i+1; s['slot'] = j+1
            s['digest_hit'] = s.get('dig') == s.get('want')
        samples.extend(res)
        ok = sum(1 for s in res if s.get('status') == 200 and s.get('digest_hit'))
        errs = [s for s in res if s.get('status') != 200]
        print(f"r{i+1}: ok={ok}/8 err={len(errs)} errs={[e.get('error','?')[:40] for e in errs]}", flush=True)
        time.sleep(0.8)  # 避开入口每IP 300/分限流，与本 bug 无关的环境噪声
    met = get_metrics()
    ok = [s for s in samples if s.get('status') == 200 and s.get('digest_hit')]
    errs = [s for s in samples if s.get('status') != 200 or not s.get('digest_hit')]
    win = {}
    for s in ok:
        win[s['src']] = win.get(s['src'], 0) + 1
    result = {'generated_at': datetime.datetime.now().isoformat(),
              'image': 'mirror-lens:3.5.18-p0fix', 'concurrency': 8, 'rounds': 10,
              'n_samples': len(samples), 'n_ok': len(ok), 'n_err': len(errs),
              'err_rate': round(len(errs)/max(len(samples),1), 4),
              'winner_dist': win, 'metrics': met, 'samples': samples,
              'official_401_count': sum(1 for s in errs if '401' in str(s.get('error','')))}
    with open(OUT, 'w') as f:
        json.dump(result, f, indent=2)
    print("RESULT", json.dumps({k: result[k] for k in ['n_ok','n_err','err_rate','winner_dist','official_401_count']}), flush=True)
    print("METRICS", json.dumps(met), flush=True)
    print("DONE", flush=True)

if __name__ == '__main__':
    main()
