#!/usr/bin/env python3
"""Push real multi-layer test manifests into registry:2 via Distribution HTTP API.
Run inside a container on the same docker network as the registries.
"""
import hashlib, json, sys, urllib.request, urllib.error

def sha(b): return "sha256:" + hashlib.sha256(b).hexdigest()

def put_blob(reg, repo, data):
    digest = sha(data)
    # initiate upload
    url = f"http://{reg}/v2/{repo}/blobs/uploads/"
    req = urllib.request.Request(url, method="POST")
    r = urllib.request.urlopen(req, timeout=30)
    loc = r.headers.get("Location")
    if loc.startswith("/"):
        loc = f"http://{reg}" + loc
    # monolithic PUT with digest
    sep = "&" if "?" in loc else "?"
    req = urllib.request.Request(loc + sep + "digest=" + digest, data=data, method="PUT",
                                 headers={"Content-Type": "application/octet-stream"})
    r = urllib.request.urlopen(req, timeout=30)
    if r.status not in (201, 202):
        raise RuntimeError(f"blob put status {r.status}")
    return digest, len(data)

def put_manifest(reg, repo, tag, manifest):
    body = json.dumps(manifest, separators=(",", ":")).encode()
    url = f"http://{reg}/v2/{repo}/manifests/{tag}"
    req = urllib.request.Request(url, data=body, method="PUT", headers={
        "Content-Type": "application/vnd.docker.distribution.manifest.v2+json"})
    r = urllib.request.urlopen(req, timeout=30)
    if r.status != 201:
        raise RuntimeError(f"manifest put status {r.status}")
    return sha(body)

def build(reg, tag, nlayers):
    repo = "library/alpine"
    config = json.dumps({"architecture": "amd64", "os": "linux", "rootfs": {"type": "layers", "diff_ids": []}}).encode()
    cdig, csize = put_blob(reg, repo, config)
    layers = []
    for i in range(nlayers):
        blob = (f"layer-{tag}-{i}-" + "x"*2048).encode()
        d, n = put_blob(reg, repo, blob)
        layers.append({"mediaType": "application/vnd.docker.image.rootfs.diff.tar.gzip", "size": n, "digest": d})
    mf = {"schemaVersion": 2, "mediaType": "application/vnd.docker.distribution.manifest.v2+json",
          "config": {"mediaType": "application/vnd.docker.container.image.v1+json", "size": csize, "digest": cdig},
          "layers": layers}
    md = put_manifest(reg, repo, tag, mf)
    return md

if __name__ == "__main__":
    regs = sys.argv[1:] or ["reg-a:5000", "reg-b:5000"]
    out = {}
    for reg in regs:
        out[reg] = {}
        for tag, nl in [("t1", 2), ("t2", 3), ("t3", 4), ("t4", 2), ("t5", 3)]:
            out[reg][tag] = build(reg, tag, nl)
            print(f"{reg} {tag} -> {out[reg][tag]}")
    print(json.dumps(out, indent=2))
