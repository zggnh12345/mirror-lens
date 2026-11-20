#!/usr/bin/env python3
import json,urllib.request,http.cookiejar,sys
GW="http://ml318-p2-gw:8080"
TAG=sys.argv[1] if len(sys.argv)>1 else "t1"
# 从源脚本计算 digest（与源完全同源，杜绝手抄）
sys.path.insert(0,"/tmp"); import race_src
body,dig=race_src.BY_TAG[TAG]
cj=http.cookiejar.CookieJar();op=urllib.request.build_opener(urllib.request.HTTPCookieProcessor(cj))
def req(m,p,d=None,csrf=False):
 h={"Content-Type":"application/json","Origin":GW}
 if csrf:h["X-CSRF-Token"]=TOK
 b=json.dumps(d).encode() if d is not None else None
 with op.open(urllib.request.Request(GW+p,data=b,headers=h,method=m),timeout=30) as x:return x.status,x.read(),x.headers
req("POST","/api/login",{"user":"nesttest","pass":"NestTest#2026"})
TOK=json.loads(req("GET","/api/me")[1])["csrf"]
# 清缓存 + breaker 通过 API/重试保证冷
st,data,h=req("GET","/v2/quay.io/library/alpine/manifests/"+dig)
ok = (st==200 and __import__('hashlib').sha256(data).hexdigest()==dig.split(":")[1])
print("STATUS",st)
print("SRC",h.get("X-Ml-Source"),"DIGEST_HEADER_OK",h.get("Docker-Content-Digest")==dig)
print("BODY_SHA_OK",ok,"BODY_LEN",len(data))
