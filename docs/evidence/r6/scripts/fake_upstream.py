#!/usr/bin/env python3
"""第六轮受控 Registry 假上游（真实 HTTP/1.1 over TCP）。

与第五轮相比的增量（只为取证，不改变被测行为）：
  * 每条记录带 t0/t1（请求开始/结束的单调+墙钟时间），用于证明两路请求区间重叠；
  * 新增 206 长度错误三种形态：bad206（Content-Range 总长不符）、
    cl_bad（Content-Length 与切片长度不符）、trunc206（头部正确、正文提前截断）；
  * 新增 manifest 延迟（slowhdr 复用）与 per-repo 尺寸。

行为按 <repo> 指定，未指定走 "ok"：
    ok / slowhdr / slowbody / cut / bad206 / cl_bad / trunc206 / prefixbad / miss
"""
import argparse
import hashlib
import json
import os
import socket
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

LOG_LOCK = threading.Lock()
ARGS = None
BEHAVIORS = {}
VARIANTS = {}
COUNTER = [0]
MANIFEST_TYPE = "application/vnd.oci.image.manifest.v1+json"


def content(repo: str, size: int, variant=None) -> bytes:
    if variant is None:
        variant = VARIANTS.get(repo, "")
    seed = f"{repo}|{variant}".encode()
    out = bytearray()
    h = hashlib.sha256(seed).digest()
    while len(out) < size:
        h = hashlib.sha256(h).digest()
        out += h
    return bytes(out[:size])


def manifest_for(repo: str, size: int, variant=None) -> bytes:
    blob = content(repo, size, variant)
    digest = "sha256:" + hashlib.sha256(blob).hexdigest()
    m = {
        "schemaVersion": 2,
        "mediaType": MANIFEST_TYPE,
        "config": {
            "mediaType": "application/vnd.oci.image.config.v1+json",
            "digest": "sha256:" + hashlib.sha256(b"config|" + repo.encode()).hexdigest(),
            "size": 64,
        },
        "layers": [{"mediaType": "application/vnd.oci.image.layer.v1.tar", "digest": digest, "size": len(blob)}],
    }
    return json.dumps(m, separators=(",", ":")).encode()


def behavior_of(repo: str) -> dict:
    return BEHAVIORS.get(repo, {"mode": "ok"})


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *a):
        pass

    def _record(self, t0, **kw):
        kw.update({"t0": t0, "t1": time.time(), "port": ARGS.port, "inst": ARGS.name})
        with LOG_LOCK:
            COUNTER[0] += 1
            kw["seq"] = COUNTER[0]
            with open(ARGS.log, "a", encoding="utf-8") as f:
                f.write(json.dumps(kw, ensure_ascii=False) + "\n")

    def _send_json(self, code, body, ctype=MANIFEST_TYPE, extra=None):
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        for k, v in (extra or {}).items():
            self.send_header(k, v)
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        self._handle(head=False)

    def do_HEAD(self):
        self._handle(head=True)

    def _handle(self, head: bool):
        t0 = time.time()
        path = self.path.split("?")[0]
        rng = self.headers.get("Range")
        if path in ("/v2/", "/v2"):
            self._record(t0, method=self.command, path=path, status=200, bytes=2, range=rng)
            self._send_json(200, b"{}", "application/json")
            return
        if "/manifests/" in path:
            repo = path.split("/v2/", 1)[1].split("/manifests/")[0]
            mb = behavior_of(repo)
            if mb.get("mode") in ("slowhdr", "slowbody") and mb.get("delay_ms"):
                time.sleep(mb["delay_ms"] / 1000.0)
            size = mb.get("size", 4096)
            body = manifest_for(repo, size, mb.get("variant"))
            self._record(t0, method=self.command, path=path, status=200, bytes=len(body),
                         range=rng, repo=repo, kind="manifest")
            self._send_json(200, body, MANIFEST_TYPE,
                            {"Docker-Content-Digest": "sha256:" + hashlib.sha256(body).hexdigest()})
            return
        if "/blobs/" in path:
            self._serve_blob(path, rng, head, t0)
            return
        self._record(t0, method=self.command, path=path, status=404, bytes=0, range=rng)
        self._send_json(404, b'{"errors":[{"code":"NAME_UNKNOWN"}]}', "application/json")

    def _serve_blob(self, path, rng, head, t0):
        repo = path.split("/v2/", 1)[1].split("/blobs/")[0]
        b = behavior_of(repo)
        mode = b.get("mode", "ok")
        size = b.get("size", 4096)
        if b.get("variant") is not None:
            VARIANTS[repo] = b["variant"]
        blob = content(repo, size)
        digest = "sha256:" + hashlib.sha256(blob).hexdigest()
        eta = {"Docker-Content-Digest": digest, "Accept-Ranges": "bytes"}
        start = 0
        if rng and rng.startswith("bytes="):
            try:
                start = int(rng.split("=", 1)[1].split("-")[0] or 0)
            except Exception:
                start = 0

        if mode == "miss":
            self._record(t0, method=self.command, path=path, status=404, bytes=0, range=rng, repo=repo,
                         kind="blob", mode=mode)
            self._send_json(404, b'{"errors":[{"code":"NAME_UNKNOWN"}]}', "application/json")
            return

        # 206 形态（Range 请求）
        if rng and rng.startswith("bytes=") and (mode in ("ok_range", "bad206", "cl_bad", "trunc206") or b.get("range_ok")):
            part = blob[start:]
            total_decl = len(blob)
            clen = len(part)
            if mode == "bad206":
                total_decl = len(blob) + 97          # Content-Range 总长不符
            elif mode == "cl_bad":
                clen = len(part) + 512                # Content-Length 与实际切片不符
            self.send_response(206)
            self.send_header("Content-Type", "application/octet-stream")
            self.send_header("Content-Length", str(clen))
            self.send_header("Content-Range", f"bytes {start}-{len(blob)-1}/{total_decl}")
            for k, v in eta.items():
                self.send_header(k, v)
            self.end_headers()
            send = part
            if mode == "trunc206":
                send = part[: max(1, len(part) // 2)]  # 正文提前截断
            sent = self._write_all(send)
            self._record(t0, method=self.command, path=path, status=206, bytes=sent, range=rng,
                         repo=repo, kind="blob", mode=mode, total=len(blob), declared_clen=clen,
                         declared_total=total_decl, truncated=(mode == "trunc206"))
            if mode in ("trunc206", "cl_bad"):
                self.close_connection = True
                try:
                    self.connection.shutdown(socket.SHUT_RDWR)
                except Exception:
                    pass
            return

        # 200 整份（忽略 Range）
        body = blob
        if mode == "prefixbad":
            bad = bytearray(blob)
            for i in range(min(64, len(bad))):
                bad[i] ^= 0xFF
            body = bytes(bad)
        self.send_response(200)
        self.send_header("Content-Type", "application/octet-stream")
        self.send_header("Content-Length", str(len(body)))
        for k, v in eta.items():
            self.send_header(k, v)
        self.end_headers()
        if head:
            self._record(t0, method=self.command, path=path, status=200, bytes=0, range=rng, repo=repo,
                         kind="blob", mode=mode, head=True)
            return
        if mode == "cut":
            part = body[: len(body) // 2]
            sent = self._write_all(part)
            self._record(t0, method=self.command, path=path, status=200, bytes=sent, range=rng, repo=repo,
                         kind="blob", mode=mode, aborted=True, total=len(body))
            self.close_connection = True
            try:
                self.connection.shutdown(socket.SHUT_RDWR)
            except Exception:
                pass
            return
        if mode == "slowbody":
            chunk = b.get("chunk", max(1, len(body) // 8))
            delay = b.get("chunk_ms", 150) / 1000.0
            sent = 0
            for i in range(0, len(body), chunk):
                sent += self._write_all(body[i:i + chunk])
                time.sleep(delay)
            self._record(t0, method=self.command, path=path, status=200, bytes=sent, range=rng, repo=repo,
                         kind="blob", mode=mode, total=len(body), chunk_ms=b.get("chunk_ms"))
            return
        sent = self._write_all(body)
        self._record(t0, method=self.command, path=path, status=200, bytes=sent, range=rng, repo=repo,
                     kind="blob", mode=mode, total=len(body), wrong_content=(mode == "prefixbad"))

    def _write_all(self, data):
        try:
            self.wfile.write(data)
            self.wfile.flush()
            return len(data)
        except Exception:
            return 0


def main():
    global ARGS
    p = argparse.ArgumentParser()
    p.add_argument("--port", type=int, required=True)
    p.add_argument("--name", default="A")
    p.add_argument("--log", required=True)
    p.add_argument("--behavior-map", default="")
    a = p.parse_args()
    ARGS = a
    if a.behavior_map and os.path.exists(a.behavior_map):
        with open(a.behavior_map, encoding="utf-8") as f:
            BEHAVIORS.update(json.load(f))
    srv = ThreadingHTTPServer(("127.0.0.1", a.port), Handler)
    srv.daemon_threads = True
    print(json.dumps({"ready": True, "port": a.port, "inst": a.name}), flush=True)
    srv.serve_forever()


if __name__ == "__main__":
    main()
