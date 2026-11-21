#!/usr/bin/env python3
"""A10 隔离环境正式 OFF/ON 配对（方案2：关闭 discover，恢复冻结场景的两个 Source）。

隔离手段（不碰产品代码/生产默认配置、不改任何冻结参数）：
  - 专用 --internal Docker 网络：容器内互通，但网关无外网出口，
    main.go:646 启动45s后的 discover.Scan 抓不到公网候选列表 -> 新增 0 源。
  - 全新命名卷 a10iso-data：干净库，no_seed=true，只 seed 2 个 fixture Source。
  - 运行前后核实候选池恒为这 2 个 Source（身份/顺序/星标/能力一致）。
  - 基线组与竞速组唯一实验变量 = registry_star_race:quay.io 开关。

冻结口径不变：并发8/3/1、10轮×8 digest、冷缓存、race_after_ms=50、
fast=0ms+20%×300ms 尾延迟、slow=20ms 稳定、P95 相对改善≥50%。
"""
import json, subprocess, sys, time, datetime, re

GW_CLI="a10iso-cli"; GW_DB="a10iso-db"; GW="a10iso-gw"
NET="ml318-a10iso"; VOL="a10iso-data"
OUTDIR="/opt/data/cache/scratch/ml8/docs/evidence/3.5.18"
CFG="/opt/data/cache/scratch/ml8/docs/evidence/3.5.18/nest-config.yaml"
SRC="/opt/data/cache/scratch/ml8/docs/evidence/3.5.18/race_source.py"
FIX="/opt/data/cache/scratch/ml8/docs/evidence/3.5.18/race-fixture.sql"
IMG="mirror-lens:3.5.18-final"

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

POOL = r'''
import sqlite3,json
db=sqlite3.connect('/data/mirror-lens.db')
rows=db.execute("select id,base_url,name,origin,enabled,status,preferred from sources order by id").fetchall()
disc=db.execute("select count(*) from sources where origin='discovered'").fetchone()[0]
sel=db.execute("select value from settings where key='cross_registry_selected_sources'").fetchone()
star=db.execute("select value from settings where key='registry_star_race:quay.io'").fetchone()
print('POOL|'+json.dumps({'rows':rows,'discovered_count':disc,
      'selected':sel[0] if sel else None,'star':star[0] if star else None}))
'''

def sh(c): return subprocess.run(c,shell=True,capture_output=True,text=True)
def dexec(c,code,arg=None):
    cmd=["docker","exec",c,"python3","-c",code]
    if arg is not None: cmd.append(arg)
    return subprocess.run(cmd,capture_output=True,text=True,timeout=120)

def pool_snapshot(label):
    p=dexec(GW_DB,POOL)
    line=[l for l in p.stdout.splitlines() if l.startswith('POOL|')]
    if not line:
        print("POOLCHECK",label,"FAIL",(p.stdout+p.stderr)[:300],flush=True);return None
    snap=json.loads(line[0][5:])
    print("POOLCHECK",label,json.dumps(snap),flush=True)
    return snap

def setup():
    # 关键：--internal 网络，无外网出口，discover 抓不到公网源列表
    sh("docker network rm %s >/dev/null 2>&1"%NET)
    sh("docker network create --internal %s >/dev/null 2>&1"%NET)
    for c in [GW,GW_DB,GW_CLI,"ml318-a10fast","ml318-a10slow"]:
        sh("docker rm -f %s >/dev/null 2>&1"%c)
    sh("docker volume rm %s >/dev/null 2>&1"%VOL)   # 全新干净库
    for name,base,tp,tm in [("ml318-a10fast","0","0.2","300"),("ml318-a10slow","20","0","0")]:
        sh("docker create --name %s --network %s -e SRC_BASE_MS=%s -e SRC_TAIL_P=%s -e SRC_TAIL_MS=%s python:3.12-alpine python /tmp/race_src.py >/dev/null"%(name,NET,base,tp,tm))
        sh("docker cp %s %s:/tmp/race_src.py >/dev/null"%(SRC,name))
        sh("docker start %s >/dev/null"%name)
    sh("docker create --name %s --network %s -v %s:/data %s >/dev/null"%(GW,NET,VOL,IMG))
    sh("docker cp %s %s:/data/config.yaml >/dev/null"%(CFG,GW))
    sh("docker start %s >/dev/null"%GW)
    sh("docker run -d --name %s --network %s -v %s:/data python:3.12-alpine sleep infinity >/dev/null"%(GW_DB,NET,VOL))
    sh("docker run -d --name %s --network %s python:3.12-alpine sleep infinity >/dev/null"%(GW_CLI,NET))
    sh("docker cp %s %s:/tmp/race_src.py >/dev/null"%(SRC,GW_CLI))
    time.sleep(10)

def seed_fixture():
    sql=open(FIX).read().replace("ml318-src-fast","ml318-a10fast").replace("ml318-src-slow","ml318-a10slow")
    py="import sqlite3;db=sqlite3.connect('/data/mirror-lens.db');db.executescript(%r);db.commit()"%sql
    p=dexec(GW_DB,py); print('SEED SQL',p.returncode,flush=True)

def set_star(star):
    val='1' if star else '0'
    code="""import sqlite3,json
db=sqlite3.connect('/data/mirror-lens.db')
ids=[r[0] for r in db.execute("select id from sources where origin='fixture' order by id")]
for i in ids: db.execute("update sources set preferred=1 where id=?",(i,))
db.execute("insert or replace into settings(key,value) values('cross_registry_selected_sources',?)",(json.dumps(ids),))
db.execute("insert or replace into settings(key,value) values('registry_star_race:quay.io','%s')")
db.commit();print('STAR',ids)
"""%val
    p=dexec(GW_DB,code); print(p.stdout.strip() or p.stderr[:300],flush=True)
    time.sleep(12)  # star 快照 TTL 10s；不重启以免 probe 清 caps / 换源 id

def get_digests():
    p=dexec(GW_CLI,"import sys;sys.path.insert(0,'/tmp');import race_src;print(','.join(race_src.BY_TAG[t][1] for t in race_src.TAGS))")
    return [d.strip() for d in p.stdout.strip().split(',') if d.strip()]

def clear_state():
    code=("import sqlite3,glob,os;db=sqlite3.connect('/data/mirror-lens.db');"
          "db.execute('DELETE FROM cache_entries');db.execute('DELETE FROM blobs');"
          "db.execute('DELETE FROM breaker_state');db.execute('DELETE FROM source_events');"
          "db.commit();[os.remove(f) for f in glob.glob('/data/cache/blobs/*/*')]")
    dexec(GW_DB,code)

def get_metrics():
    p=dexec(GW_CLI,"import urllib.request;print(urllib.request.urlopen('http://%s:8080/metrics',timeout=10).read().decode())"%GW)
    d={}
    for l in p.stdout.splitlines():
        m=re.match(r'^mirrorlens_registry_race_(\w+)\{([^}]*)\}\s+(\d+(?:\.\d+)?)$',l)
        if m:
            name,lab,val=m.group(1),m.group(2),float(m.group(3));k=name
            if name=='wins_total':
                rk=re.search(r'rank="(\d+)"',lab);k='wins_rank'+(rk.group(1) if rk else '?')
            d[k]=d.get(k,0)+val;continue
        m2=re.match(r'^mirrorlens_registry_race_(\w+_total)\s+(\d+(?:\.\d+)?)$',l)
        if m2: d[m2.group(1)]=float(m2.group(2))
    return d

def pct(v,p):
    if not v: return None
    x=sorted(v);k=(len(x)-1)*p/100;f=int(k);c=min(f+1,len(x)-1)
    return round(x[f]+(x[c]-x[f])*(k-f),3)

def run_group(star, digs, rounds=10, conc=8):
    set_star(star)
    pool_snapshot('group_star_%d'%int(star))
    samples=[]
    for i in range(rounds):
        clear_state()
        p=dexec(GW_CLI,REQ,json.dumps({'digs':digs[:conc],'gw':GW,'conc':conc}))
        line=[l for l in p.stdout.splitlines() if l.startswith('RESULTS|')]
        if not line:
            samples.append({'round':i+1,'error':(p.stdout+p.stderr)[:200]});continue
        res=json.loads(line[0][8:])
        for j,s in enumerate(res):
            s['round']=i+1;s['slot']=j+1;s['digest_hit']=s.get('dig')==s.get('want')
        samples.extend(res)
        ok=sum(1 for s in res if s.get('status')==200 and s.get('digest_hit'))
        errs=[s for s in res if s.get('status')!=200]
        print(f"  r{i+1}: ok={ok}/{conc} err={len(errs)}",flush=True)
        time.sleep(0.8)
    ok=[s for s in samples if s.get('status')==200 and s.get('digest_hit')]
    errs=[s for s in samples if s.get('status')!=200 or not s.get('digest_hit')]
    win={}
    for s in ok: win[s['src']]=win.get(s['src'],0)+1
    met=get_metrics()
    return {'star':star,'concurrency':conc,'rounds':rounds,'n_samples':len(samples),
            'n_ok':len(ok),'n_err':len(errs),'err_rate':round(len(errs)/max(len(samples),1),4),
            'official_401':sum(1 for s in errs if '401' in str(s.get('error',''))),
            'p50':pct([s['ttfb_ms'] for s in ok],50),'p95':pct([s['ttfb_ms'] for s in ok],95),
            'p99':pct([s['ttfb_ms'] for s in ok],99),'winner_dist':win,'metrics':met,
            'samples':samples}

def main():
    global CONC, TAG
    CONC = int(sys.argv[1]) if len(sys.argv)>1 else 8
    TAG  = sys.argv[2] if len(sys.argv)>2 else "c8"
    print("CONC",CONC,"TAG",TAG,flush=True)
    print("SETUP",flush=True);setup()
    print("SEED FIXTURE",flush=True);seed_fixture();time.sleep(3)
    pool_snapshot('after_seed')
    digs=get_digests();print("DIGESTS",len(digs),flush=True)
    if len(digs)!=8: print("FATAL",digs,flush=True);return
    print("BASELINE star=OFF",flush=True);base=run_group(False,digs,conc=CONC)
    print("RACE star=ON",flush=True);race=run_group(True,digs,conc=CONC)
    print("POST-RUN POOL CHECK",flush=True);pool_snapshot('after_all')
    imp=round((base['p95']-race['p95'])/base['p95'],4) if base['p95'] and race['p95'] else None
    verdict=None
    if imp is not None:
        same_caliber = (base['official_401']==race['official_401']==0)
        verdict={'p95_improve':imp,'threshold_met':imp>=0.5,
                 'caliber_consistent':same_caliber,
                 'PASS':bool(imp>=0.5 and same_caliber)}
    out={'generated_at':datetime.datetime.now().isoformat(),'image':IMG,
         'fix':'c326538 P0+P1+P2+P3+3.5.18','isolation':'internal-network-no-discover + fresh-volume',
         'frozen_concurrency':8,'run_concurrency':CONC,'run_tag':TAG,
         'baseline':base,'race':race,'verdict':verdict}
    fn="%s/a10iso-pair-%s.json"%(OUTDIR,TAG)
    json.dump(out,open(fn,'w'),indent=2)
    print("VERDICT",json.dumps(verdict),flush=True)
    print("BASE p50/p95/p99",base['p50'],base['p95'],base['p99'],"err",base['err_rate'],"401",base['official_401'],flush=True)
    print("RACE p50/p95/p99",race['p50'],race['p95'],race['p99'],"err",race['err_rate'],"401",race['official_401'],flush=True)
    print("RACE metrics",json.dumps(race['metrics']),flush=True)
    print("DONE",flush=True)

if __name__=='__main__': main()
