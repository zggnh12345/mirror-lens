#!/usr/bin/env python3
"""Real HTTP upstream for A10 race pairing. Serves 8 genuine Docker v2 manifests
(t1..t8, each with its own content-computed digest). Latency is injected ONLY on the
handler side; content bytes and digests are genuine and self-consistent.
Env:
  SRC_BASE_MS   base latency every request (default 0)
  SRC_TAIL_P    probability [0,1] of injecting tail latency (default 0)
  SRC_TAIL_MS   tail latency ms (default 300)
  SRC_PORT      listen port (default 19001)
"""
import hashlib, json, os, random
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

BASE_MS = int(os.environ.get("SRC_BASE_MS", "0"))
TAIL_P = float(os.environ.get("SRC_TAIL_P", "0"))
TAIL_MS = int(os.environ.get("SRC_TAIL_MS", "300"))
PORT = int(os.environ.get("SRC_PORT", "19001"))
TAGS = [f"t{i}" for i in range(1, 9)]

def manifest_for(tag):
    cfg = json.dumps({"architecture":"amd64","os":"linux",
                      "rootfs":{"type":"layers","diff_ids":[]}}).encode()
    layers = [{"mediaType":"application/vnd.docker.image.rootfs.diff.tar.gzip",
               "size":2048+i, "digest":"sha256:"+hashlib.sha256(f"layer-{tag}-{i}".encode()).hexdigest()}
              for i in range(3)]
    body = json.dumps({"schemaVersion":2,
        "mediaType":"application/vnd.docker.distribution.manifest.v2+json",
        "config":{"mediaType":"application/vnd.docker.container.image.v1+json",
                  "size":len(cfg),"digest":"sha256:"+hashlib.sha256(cfg).hexdigest()},
        "layers":layers}, separators=(",",":")).encode()
    dig = "sha256:"+hashlib.sha256(body).hexdigest()
    return body, dig

BY_TAG = {t: manifest_for(t) for t in TAGS}
BY_DIGEST = {dig: body for body, dig in BY_TAG.values()}
DEFAULT = BY_TAG["t1"]

class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    def log_message(self, *a): pass
    def do_GET(self):
        delay = BASE_MS
        if TAIL_P > 0 and random.random() < TAIL_P:
            delay += TAIL_MS
        import time
        if delay > 0: time.sleep(delay/1000.0)
        if not (self.path.startswith("/v2/") or self.path == "/v2/"):
            self.send_response(404); self.send_header("Content-Length","0"); self.end_headers(); return
        last = self.path.rsplit("/", 1)[-1].split("?")[0]
        if last.startswith("sha256:") and last in BY_DIGEST:
            body, dig = BY_DIGEST[last], last
        elif last in BY_TAG:
            body, dig = BY_TAG[last]
        elif last == "latest":
            body, dig = DEFAULT
        else:
            body, dig = DEFAULT
        self.send_response(200)
        self.send_header("Content-Type","application/vnd.docker.distribution.manifest.v2+json")
        self.send_header("Docker-Content-Digest", dig)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

if __name__ == "__main__":
    print(f"SRC base={BASE_MS}ms tailP={TAIL_P} tailMs={TAIL_MS}ms", flush=True)
    for t in TAGS:
        print(f"{t} {BY_TAG[t][1]}", flush=True)
    ThreadingHTTPServer(("0.0.0.0", PORT), H).serve_forever()
