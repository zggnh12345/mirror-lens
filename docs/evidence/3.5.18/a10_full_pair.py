#!/usr/bin/env python3
"""A10 full pairing matrix. Runs frozen concurrency=8 plus supplementary tiers.
Dynamic digests from source script (no hand-copy). For each tier: baseline(star OFF)
vs race(star ON), N rounds, cold cache, report P50/P95/P99, error rate, winner dist,
body_failures, budget_skipped, fallback. Dishonest results are kept as-is.
"""
import json, subprocess, sys, time, datetime, re

GW_CLI = "ml318-p2-cli"
GW_DB = "ml318-p2-db"
GW = "ml318-p2-gw"
OUTDIR = "/opt/data/cache/scratch/ml8/docs/evidence/3.5.18"


def sh(cmd):
    return subprocess.run(cmd, shell=True, capture_output=True, text=True)


def dexec(container, pycode, arg=None):
    cmd = ["docker", "exec", container, "python3", "-c", pycode]
    if arg is not None:
        cmd.append(arg)
    return subprocess.run(cmd, capture_output=True, text=True, timeout=120)


def get_digests():
    p = dexec(GW_CLI, "import sys;sys.path.insert(0,'/tmp');import race_src;print(','.join(race_src.BY_TAG[t][1] for t in race_src.TAGS))")
    return [d.strip() for d in p.stdout.strip().split(',') if d.strip()]


def set_star(enabled):
    val = '1' if enabled else '0'
    code = ("import sqlite3;db=sqlite3.connect('/data/mirror-lens.db');"
            "db.execute(\"insert or replace into settings(key,value) values('registry_star_race:quay.io','%s')\");"
            "db.commit()" % val)
    return dexec(GW_DB, code).returncode == 0


def clear_state():
    code = ("import sqlite3,glob,os;db=sqlite3.connect('/data/mirror-lens.db');"
            "db.execute('DELETE FROM cache_entries');db.execute('DELETE FROM blobs');"
            "db.execute('DELETE FROM breaker_state');db.execute('DELETE FROM source_events');"
            "db.commit();[os.remove(f) for f in glob.glob('/data/cache/blobs/*/*')]")
    return dexec(GW_DB, code)


def restart_gw():
    sh("docker restart %s >/dev/null 2>&1" % GW)
    for _ in range(30):
        p = dexec(GW_CLI, "import urllib.request;urllib.request.urlopen('http://%s:8080/metrics',timeout=3)" % GW)
        if p.returncode == 0:
            return
        time.sleep(1)


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
        return {'ttfb_ms':round(ttfb,3),'total_ms':round((time.perf_counter()-t0)*1000,3),
                'src':r.headers.get('X-Ml-Source') or '-','dig':r.headers.get('Docker-Content-Digest') or '-',
                'want':d,'bytes':len(b),'status':r.status}
    except Exception as e:
        return {'error':str(e)[:200],'want':d}
with ThreadPoolExecutor(max_workers=conc) as ex:
    res=list(ex.map(one,digs))
print('RESULTS|'+json.dumps(res))
'''


def pct(vals, p):
    if not vals:
        return None
    xs = sorted(vals)
    k = (len(xs) - 1) * p / 100
    f = int(k)
    c = min(f + 1, len(xs) - 1)
    return round(xs[f] + (xs[c] - xs[f]) * (k - f), 3)


def run_tier(conc, rounds, digests):
    print("\n===== TIER conc=%d =====" % conc, flush=True)
    results = {}
    for mode in ['baseline', 'race']:
        enabled = (mode == 'race')
        set_star(enabled)
        restart_gw()
        samples = []
        for i in range(rounds):
            clear_state()
            args = {'digs': digests[:conc], 'gw': GW, 'conc': conc}
            p = dexec(GW_CLI, REQ, json.dumps(args))
            line = [l for l in p.stdout.splitlines() if l.startswith('RESULTS|')]
            if not line:
                samples.append({'round': i + 1, 'error': (p.stdout + p.stderr)[:200]})
                continue
            res = json.loads(line[0][8:])
            for j, s in enumerate(res):
                s['round'] = i + 1
                s['slot'] = j + 1
                s['digest_hit'] = s.get('dig') == s.get('want')
            samples.extend(res)
        ok = [s for s in samples if s.get('status') == 200 and s.get('digest_hit')]
        errs = [s for s in samples if s.get('status') != 200 or not s.get('digest_hit')]
        ttf = [s['ttfb_ms'] for s in ok]
        win = {}
        for s in ok:
            win[s['src']] = win.get(s['src'], 0) + 1
        met = get_metrics()
        summ = {'mode': mode, 'star': enabled, 'concurrency': conc, 'rounds': rounds,
                'n_samples': len(samples), 'n_ok': len(ok), 'n_err': len(errs),
                'err_rate': round(len(errs) / max(len(samples), 1), 4),
                'p50': pct(ttf, 50), 'p95': pct(ttf, 95), 'p99': pct(ttf, 99),
                'winner_dist': win,
                'race_started': met.get('started_total', 0),
                'extra_requests': met.get('extra_requests_total', 0),
                'rank1_wins': met.get('wins_rank1', 0),
                'rank2_wins': met.get('wins_rank2', 0),
                'body_failures': met.get('body_failures_total', 0),
                'fallback': met.get('fallback_total', 0),
                'budget_skipped': met.get('budget_skipped_total', 0),
                'ineligible': met.get('ineligible_total', 0)}
        results[mode] = {'summary': summ, 'samples': samples}
        print(json.dumps(summ), flush=True)
    b = results['baseline']['summary']
    rr = results['race']['summary']
    if b['p95'] and rr['p95'] and b['p95'] > 0:
        imp = round((b['p95'] - rr['p95']) / b['p95'], 4)
        print("TIER_RESULT conc=%d p95_improve=%s met=%s baseline_err=%s race_err=%s" %
              (conc, imp, imp >= 0.5, b['err_rate'], rr['err_rate']), flush=True)
        results['p95_improve'] = imp
    with open("%s/a10full-conc%d.json" % (OUTDIR, conc), "w") as f:
        json.dump({'generated_at': datetime.datetime.now().isoformat(),
                   'tier': conc, 'digests': digests, 'results': results}, f, indent=2)
    return results


digests = get_digests()
print("DIGESTS", len(digests), digests, flush=True)
if len(digests) < 8:
    print("FATAL: need 8 digests, got %d" % len(digests), flush=True)
    sys.exit(1)

allr = {}
allr[8] = run_tier(8, 10, digests)   # 冻结档
allr[3] = run_tier(3, 10, digests)   # 补充档：预算匹配
allr[1] = run_tier(1, 10, digests)   # 补充档：纯隔离
summary_out = {}
for k, v in allr.items():
    summary_out[k] = {m: v[m]['summary'] for m in ['baseline', 'race']}
    if 'p95_improve' in v:
        summary_out[k]['p95_improve'] = v['p95_improve']
with open("%s/a10full-summary.json" % OUTDIR, "w") as f:
    json.dump(summary_out, f, indent=2)
print("\nALL DONE", flush=True)
