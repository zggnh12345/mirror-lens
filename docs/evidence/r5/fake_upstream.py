#!/usr/bin/env python3
"""受控 Registry 假上游（真实 HTTP/1.1 over TCP）。

用法：
    python3 fake_upstream.py --port 19001 --name A --log /path/upA.jsonl \
        [--behavior-map /path/behA.json]

行为按 <repo> 指定，未指定走 "ok"：
    ok          正常整份返回
    slowhdr     N 毫秒后才发响应头
    slowbody    响应头立即，正文按 chunk 慢发
    cut         发 total/2 后直接断开连接（不留结束）
    bad206      对 Range 请求返回 206 但 Content-Range 总长写错
    prefixbad   返回 200 且正文与请求 digest 不符（首段就坏）
    big         大正文（用于并发/预算）
另外 --content-variant X 让同一 repo 的字节与其他实例不同（模拟不同版本）。
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

MANIFEST_TYPE = "application/vnd.oci.image.manifest.v1+json"


def content(repo: str, size: int) -> bytes:
    """确定性伪随机内容：同 repo + 同 variant 得到同一份字节。"""
    variant = VARIANTS.get(repo, "")
    seed = f"{repo}|{variant}".encode()
    out = bytearray()
    h = hashlib.sha256(seed).digest()
    while len(out) < size:
        h = hashlib.sha256(h).digest()
        out += h
    return bytes(out[:size])


def manifest_for(repo: str, size: int) -> bytes:
    blob = content(repo, size)
    digest = "sha256:" + hashlib.sha256(blob).hexdigest()
    m = {
        "schemaVersion": 2,
        "mediaType": MANIFEST_TYPE,
        "config": {
            "mediaType": "application/vnd.oci.image.config.v1+json",
            "digest": "sha256:" + hashlib.sha256(b"config|" + repo.encode()).hexdigest(),
            "size": 64,
        },
        "layers": [
            {
                "mediaType": "application/vnd.oci.image.layer.v1.tar",
                "digest": digest,
                "size": len(blob),
            }
        ],
    }
    return json.dumps(m, separators=(",", ":")).encode()


def behavior_of(repo: str) -> dict:
    return BEHAVIORS.get(repo, {"mode": "ok"})


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *a):  # 静音默认 stderr 日志
        pass

    def _record(self, **kw):
        kw.update({"ts": time.time(), "port": ARGS.port, "inst": ARGS.name})
        line = json.dumps(kw, ensure_ascii=False)
        with LOG_LOCK:
            with open(ARGS.log, "a", encoding="utf-8") as f:
                f.write(line + "\n")

    def _send_json(self, code: int, body: bytes, ctype=MANIFEST_TYPE, extra=None):
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        for k, v in (extra or {}).items():
            self.send_header(k, v)
        self.end_headers()
        self.wfile.write(body)

    # ---- 路由 ----
    def do_GET(self):
        self._handle(head=False)

    def do_HEAD(self):
        self._handle(head=True)

    def _handle(self, head: bool):
        path = self.path.split("?")[0]
        rng = self.headers.get("Range")
        if path == "/v2/" or path == "/v2":
            self._record(method=self.command, path=path, status=200, bytes=2, range=rng)
            self._send_json(200, b"{}", "application/json")
            return
        if "/manifests/" in path:
            repo = path.split("/v2/", 1)[1].split("/manifests/")[0]
            mb = behavior_of(repo)
            if mb.get("mode") == "slowhdr":
                time.sleep(mb.get("delay_ms", 1500) / 1000.0)
            size = mb.get("size", 4096)
            body = manifest_for(repo, size)
            self._record(method=self.command, path=path, status=200, bytes=len(body),
                         range=rng, repo=repo, kind="manifest")
            self._send_json(200, body, MANIFEST_TYPE,
                            {"Docker-Content-Digest": "sha256:" + hashlib.sha256(body).hexdigest()})
            return
        if "/blobs/" in path:
            self._serve_blob(path, rng, head)
            return
        self._record(method=self.command, path=path, status=404, bytes=0, range=rng)
        self._send_json(404, b'{"errors":[{"code":"NAME_UNKNOWN"}]}', "application/json")

    def _serve_blob(self, path, rng, head):
        repo = path.split("/v2/", 1)[1].split("/blobs/")[0]
        want = path.rsplit("/blobs/", 1)[1]
        b = behavior_of(repo)
        mode = b.get("mode", "ok")
        size = b.get("size", 4096)
        variant = b.get("variant")
        if variant is not None:
            VARIANTS[repo] = variant
        blob = content(repo, size)
        digest = "sha256:" + hashlib.sha256(blob).hexdigest()
        eta = {"Docker-Content-Digest": digest, "Accept-Ranges": "bytes"}

        if mode == "miss":
            self._record(method=self.command, path=path, status=404, bytes=0, range=rng, repo=repo, kind="blob", mode=mode)
            self._send_json(404, b'{"errors":[{"code":"NAME_UNKNOWN"}]}', "application/json")
            return
        if mode == "slowhdr":
            time.sleep(b.get("delay_ms", 1500) / 1000.0)

        # 通用 Range 支持（range_ok=true 时按 206 正确切片），用于分片/换源/中途并行。
        if b.get("range_ok") and rng and rng.startswith("bytes=") and mode not in ("bad206", "cut_then_bad206"):
            try:
                start, _, end = rng.split("=", 1)[1].partition("-")
                start = int(start or 0)
            except Exception:
                start = 0
            part = blob[start:]
            self.send_response(206)
            self.send_header("Content-Type", "application/octet-stream")
            self.send_header("Content-Length", str(len(part)))
            self.send_header("Content-Range", f"bytes {start}-{len(blob)-1}/{len(blob)}")
            for k, v in eta.items():
                self.send_header(k, v)
            self.end_headers()
            sent = self._write_all(part)
            self._record(method=self.command, path=path, status=206, bytes=sent, range=rng,
                         repo=repo, kind="blob", mode=mode, total=len(blob))
            return

        if mode == "bad206":
            # 206 + 错误的 Content-Range 总长：长度校验必须失败
            start = 0
            if rng and rng.startswith("bytes="):
                try:
                    start = int(rng.split("=", 1)[1].split("-")[0])
                except Exception:
                    start = 0
            part = blob[start:]
            wrong_total = len(blob) + 97
            self.send_response(206)
            self.send_header("Content-Type", "application/octet-stream")
            self.send_header("Content-Length", str(len(part)))
            self.send_header("Content-Range", f"bytes {start}-{len(blob)-1}/{wrong_total}")
            for k, v in eta.items():
                self.send_header(k, v)
            self.end_headers()
            self._record(method=self.command, path=path, status=206, bytes=len(part),
                         range=rng, repo=repo, kind="blob", mode=mode, total=len(blob), wrong_total=wrong_total)
            if not head:
                self._write_all(part)
            return

        # 可续传模式：带 Range 时按 206 正确切片（用于验证 .part 续传）
        if mode in ("cutonce", "cut_then_bad206") and rng and rng.startswith("bytes="):
            start = 0
            try:
                start = int(rng.split("=", 1)[1].split("-")[0])
            except Exception:
                start = 0
            part = blob[start:]
            if mode == "cut_then_bad206":
                # 续传返回长度对不上的 206：必须被识破，绝不能提交缓存
                wrong_total = len(blob) + 97
                self.send_response(206)
                self.send_header("Content-Type", "application/octet-stream")
                self.send_header("Content-Length", str(len(part)))
                self.send_header("Content-Range", f"bytes {start}-{len(blob)-1}/{wrong_total}")
                for k, v in eta.items():
                    self.send_header(k, v)
                self.end_headers()
                self._record(method=self.command, path=path, status=206, bytes=len(part), range=rng,
                             repo=repo, kind="blob", mode=mode, total=len(blob), wrong_total=wrong_total)
                self._write_all(part)
                return
            self.send_response(206)
            self.send_header("Content-Type", "application/octet-stream")
            self.send_header("Content-Length", str(len(part)))
            self.send_header("Content-Range", f"bytes {start}-{len(blob)-1}/{len(blob)}")
            for k, v in eta.items():
                self.send_header(k, v)
            self.end_headers()
            sent = self._write_all(part)
            self._record(method=self.command, path=path, status=206, bytes=sent, range=rng,
                         repo=repo, kind="blob", mode=mode, total=len(blob))
            return

        # 其余模式都回 200 整份（忽略 Range，模拟不支持 Range 的源）
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
        if mode in ("cutonce", "cut_then_bad206"):
            self.send_header("ETag", '"' + digest.split(":", 1)[1][:16] + '"')
            self.send_header("Last-Modified", "Tue, 18 Nov 2025 10:00:00 GMT")
        self.end_headers()
        if head:
            self._record(method=self.command, path=path, status=200, bytes=0,
                         range=rng, repo=repo, kind="blob", mode=mode, head=True)
            return
        sent = 0
        if mode in ("cut", "cutonce", "cut_then_bad206"):
            part = body[: len(body) // 2]
            sent += self._write_all(part)
            self._record(method=self.command, path=path, status=200, bytes=sent,
                         range=rng, repo=repo, kind="blob", mode=mode, aborted=True, total=len(body))
            self.close_connection = True
            try:
                self.connection.shutdown(socket.SHUT_RDWR)
            except Exception:
                pass
            return
        if mode == "slowbody":
            chunk = b.get("chunk", max(1, len(body) // 8))
            delay = b.get("chunk_ms", 150) / 1000.0
            for i in range(0, len(body), chunk):
                sent += self._write_all(body[i:i + chunk])
                time.sleep(delay)
            self._record(method=self.command, path=path, status=200, bytes=sent,
                         range=rng, repo=repo, kind="blob", mode=mode, total=len(body))
            return
        sent += self._write_all(body)
        self._record(method=self.command, path=path, status=200, bytes=sent,
                     range=rng, repo=repo, kind="blob", mode=mode, total=len(body),
                     wrong_content=(mode == "prefixbad"))

    def _write_all(self, data: bytes) -> int:
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
