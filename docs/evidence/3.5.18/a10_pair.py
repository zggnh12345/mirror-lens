#!/usr/bin/env python3
"""A10 pairing orchestrator (host). Each round: clear cold cache + failure state via
ml318-db, then issue 8 concurrent digest requests (distinct digests, avoid singleflight)
from an in-network sidecar. 10 rounds -> 80 TTFB samples per group.
Usage: a10_pair.py <digest1,...,digest8> <rounds> <label>
"""
import json, subprocess, sys, datetime

DIGESTS = sys.argv[1].split(",")
N = int(sys.argv[2]) if len(sys.argv) > 2 else 10
LABEL = sys.argv[3] if len(sys.argv) > 3 else "run"
GW = "ml318-perf-gw"
OUT = f"/opt/data/cache/scratch/ml8/docs/evidence/3.5.18/{LABEL}.json"

def clear_state():
    code = ("import sqlite3,glob,os;db=sqlite3.connect('/data/mirror-lens.db');"
            "db.execute('DELETE FROM cache_entries');db.execute('DELETE FROM blobs');"
            "db.execute('DELETE FROM breaker_state');db.execute('DELETE FROM source_events');"
            "db.commit();[os.remove(f) for f in glob.glob('/data/cache/blobs/*/*')]")
    subprocess.run(["docker","exec","ml318-db","python3","-c",code], capture_output=True)

REQ = r'''
import urllib.request,time,sys,json
from concurrent.futures import ThreadPoolExecutor
digs=json.loads(sys.argv[1]); gw=sys.argv[2]
def one(d):
    u='http://%s:8080/v2/quay.io/library/alpine/manifests/%s'%(gw,d)
    req=urllib.request.Request(u,headers={'Accept':'application/vnd.docker.distribution.manifest.v2+json'})
    t0=time.perf_counter()
    try:
        r=urllib.request.urlopen(req,timeout=20)
        ttfb=(time.perf_counter()-t0)*1000
        b=r.read()
        return {'ttfb_ms':round(ttfb,3),'total_ms':round((time.perf_counter()-t0)*1000,3),
                'src':r.headers.get('X-ML-Source') or '-','dig':r.headers.get('Docker-Content-Digest') or '-',
                'want':d,'bytes':len(b),'status':r.status}
    except Exception as e:
        return {'error':str(e)[:200],'want':d}
with ThreadPoolExecutor(max_workers=8) as ex:
    res=list(ex.map(one,digs))
print('RESULTS|'+json.dumps(res))
'''

def one_round():
    clear_state()
    p = subprocess.run(["docker","exec","ml318-perf-cli","python3","-c",REQ,
                        json.dumps(DIGESTS), GW], capture_output=True, text=True, timeout=60)
    line = [l for l in p.stdout.splitlines() if l.startswith("RESULTS|")]
    if not line:
        return [{"error": (p.stdout+p.stderr)[:300]}]
    return json.loads(line[0][len("RESULTS|"):])

samples=[]
for i in range(N):
    try:
        res=one_round()
        for j,s in enumerate(res):
            s["round"]=i+1; s["slot"]=j+1
            s["digest_hit"]= s.get("dig")==s.get("want")
        samples.extend(res)
        ttf=[s.get("ttfb_ms") for s in res if s.get("status")==200 and s.get("digest_hit")]
        wins=[s.get("src") for s in res if s.get("status")==200 and s.get("digest_hit")]
        print(f"{LABEL} r{i+1}: n_ok={len(ttf)}/8 ttfb={sorted(ttf) if ttf else 'ERR'} winners={wins}", flush=True)
    except Exception as e:
        print(f"{LABEL} r{i+1}: ERROR {e}", flush=True)
        samples.append({"round":i+1,"error":str(e)})

def pct(vals,p):
    if not vals: return None
    xs=sorted(vals); k=(len(xs)-1)*p/100; f=int(k); c=min(f+1,len(xs)-1)
    return round(xs[f]+(xs[c]-xs[f])*(k-f),3)

ok=[s for s in samples if s.get("status")==200 and s.get("digest_hit")]
ttfbs=[s["ttfb_ms"] for s in ok]
win={}
for s in ok: win[s["src"]]=win.get(s["src"],0)+1
summary={"label":LABEL,"rounds":N,"concurrency":8,"n_samples":len(samples),"n_ok":len(ok),
 "ttfb_p50":pct(ttfbs,50),"ttfb_p95":pct(ttfbs,95),"ttfb_p99":pct(ttfbs,99),"winner_dist":win}
print("SUMMARY "+json.dumps(summary),flush=True)
with open(OUT,"w") as f:
    json.dump({"generated_at":datetime.datetime.now().isoformat(),"digests":DIGESTS,
               "samples":samples,"summary":summary},f,indent=2)
