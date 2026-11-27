#!/usr/bin/env python3
"""从 g6-real 原始数据生成 result.json（不手填任何数字）。"""
import json, os, statistics, hashlib

EV = os.path.dirname(os.path.abspath(__file__))

def load(n): return json.load(open(os.path.join(EV,n)))
def pct(xs,p):
    xs=sorted(xs); k=(len(xs)-1)*p; f=int(k); c=min(f+1,len(xs)-1)
    return round(xs[f]+(xs[c]-xs[f])*(k-f),3)

def summ(samples):
    ms=[s["ms"] for s in samples]
    ok=[s for s in samples if s["status"]==200 and s["err"] is None]
    return {"n":len(samples),"success":len(ok),
            "success_rate":round(len(ok)/len(samples),4),
            "p50":pct(ms,.5),"p95":pct(ms,.95),
            "min":round(min(ms),3),"max":round(max(ms),3),
            "errors":sorted(set(s["err"] for s in samples if s["err"]))}

R={}
# 热缓存：3 轮交替，每版合并 1500 样本算 P50/P95
hot={}
for ver in ["324","325"]:
    ss=[]
    for r in [1,2,3]:
        ss += load(f"hot-{ver}-r{r}.json")["samples"]
    hot[ver]=summ(ss)
R["hot_cache"]={"baseline_324":hot["324"],"candidate_325":hot["325"],
    "delta_p50_pct":round((hot["325"]["p50"]-hot["324"]["p50"])/hot["324"]["p50"]*100,2),
    "delta_p95_pct":round((hot["325"]["p95"]-hot["324"]["p95"])/hot["324"]["p95"]*100,2)}
# 每轮方向（证明噪声随机）
rounds=[]
for r in [1,2,3]:
    a=summ(load(f"hot-325-r{r}.json")["samples"]); b=summ(load(f"hot-324-r{r}.json")["samples"])
    rounds.append({"round":r,"325_p50":a["p50"],"324_p50":b["p50"],
        "325_p95":a["p95"],"324_p95":b["p95"],
        "delta_p50_pct":round((a["p50"]-b["p50"])/b["p50"]*100,2),
        "delta_p95_pct":round((a["p95"]-b["p95"])/b["p95"]*100,2)})
R["hot_cache"]["per_round"]=rounds
# 冷缓存
cold24=summ(load("cold-324.json")["samples"]); cold25=summ(load("cold-325.json")["samples"])
R["cold_cache"]={"baseline_324":cold24,"candidate_325":cold25,
    "p95_ratio":round(cold25["p95"]/cold24["p95"],4),
    "warmup":load("cold-324.json")["warmup"]}
# 并发
conc24=summ(load("concurrent-324.json")["samples"]); conc25=summ(load("concurrent-325.json")["samples"])
R["concurrent"]={"baseline_324":conc24,"candidate_325":conc25}
# 门禁判定（冻结阈值）
TH={"hot_p95_ms_max":2.0,"hot_error_rate":0,"cold_p95_ratio_max":1.5,
    "concurrent_success_rate":1.0,"concurrent_p95_ms_max":50.0,"sample_min":20,
    "regress_p50_max_pct":10.0,"regress_p95_max_pct":15.0}
gates={}
h=R["hot_cache"]
gates["hot_p95_le_2ms"] = h["baseline_324"]["p95"]<=2.0 and h["candidate_325"]["p95"]<=2.0
gates["hot_error_rate_0"] = h["baseline_324"]["success_rate"]==1.0 and h["candidate_325"]["success_rate"]==1.0
gates["hot_regress_p50_le_10pct"] = h["delta_p50_pct"]<=10.0
gates["hot_regress_p95_le_15pct"] = h["delta_p95_pct"]<=15.0
c=R["cold_cache"]
gates["cold_p95_ratio_le_1_5"] = c["p95_ratio"]<=1.5
gates["cold_sample_min_20"] = c["baseline_324"]["n"]>=20 and c["candidate_325"]["n"]>=20
cc=R["concurrent"]
gates["concurrent_success_100pct"] = cc["baseline_324"]["success_rate"]==1.0 and cc["candidate_325"]["success_rate"]==1.0
gates["concurrent_p95_le_50ms"] = cc["baseline_324"]["p95"]<=50.0 and cc["candidate_325"]["p95"]<=50.0
R["thresholds_frozen"]=TH
R["gates"]=gates
R["all_gates_pass"]=all(gates.values())
# 原始数据 SHA256 清单
sha={}
for f in sorted(os.listdir(EV)):
    if f.endswith(".json") or f.endswith(".log"):
        sha[f]=hashlib.sha256(open(os.path.join(EV,f),"rb").read()).hexdigest()
R["raw_sha256"]=sha
json.dump(R, open(os.path.join(EV,"result.json"),"w"), indent=2, ensure_ascii=False)
print("=== G6 真实环境门禁 ===")
for k,v in gates.items(): print(f"  {k}: {'PASS' if v else 'FAIL'}")
print("all_gates_pass:", R["all_gates_pass"])
print("\n热缓存合并:", R["hot_cache"]["baseline_324"]["p50"], R["hot_cache"]["baseline_324"]["p95"],
      "vs", R["hot_cache"]["candidate_325"]["p50"], R["hot_cache"]["candidate_325"]["p95"],
      "ΔP50", R["hot_cache"]["delta_p50_pct"], "% ΔP95", R["hot_cache"]["delta_p95_pct"],"%")
print("冷缓存 P95 比值:", R["cold_cache"]["p95_ratio"])
print("并发 P95:", conc24["p95"], "/", conc25["p95"], "成功率", conc24["success_rate"], "/", conc25["success_rate"])
