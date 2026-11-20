#!/usr/bin/env python3
import json, urllib.request, http.cookiejar
GW="http://ml318-p2-gw:8080"
cj=http.cookiejar.CookieJar();op=urllib.request.build_opener(urllib.request.HTTPCookieProcessor(cj))
def req(m,p,d=None,csrf=False):
    h={"Content-Type":"application/json","Origin":GW}
    if csrf: h["X-CSRF-Token"]=TOK
    b=json.dumps(d).encode() if d is not None else None
    with op.open(urllib.request.Request(GW+p,data=b,headers=h,method=m),timeout=15) as r:
        return r.status,r.read()
st,b=req("POST","/api/login",{"user":"nesttest","pass":"NestTest#2026"})
assert st==200,(st,b[:200])
TOK=json.loads(req("GET","/api/me")[1])["csrf"]
print("GET before:",req("GET","/api/star-race")[1].decode()[:300])
st,b=req("PUT","/api/star-race/quay.io",{"enabled":True},csrf=True)
print("PUT:",st,b.decode()[:300])
print("GET after:",req("GET","/api/star-race")[1].decode()[:300])
