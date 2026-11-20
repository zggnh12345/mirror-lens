#!/usr/bin/env bash
# 发布版本一致性校验（唯一权威来源：internal/version/version.go）。
#
# 用法：
#   scripts/check-version.sh --ref-type tag --tag v3.5.17   # 发布构建：严格校验
#   scripts/check-version.sh --ref-type branch --tag main   # 普通构建：只读常量
#
# 成功时向 stdout 输出版本号（供 Docker build-arg / OCI label 使用）；
# 失败时向 stderr 说明原因并退出 1。
set -euo pipefail

ref_type=branch
tag=""

while [ $# -gt 0 ]; do
  case "$1" in
    --ref-type) ref_type="${2:-}"; shift 2 ;;
    --tag)      tag="${2:-}";      shift 2 ;;
    *) echo "未知参数: $1" >&2; exit 2 ;;
  esac
done

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
src="$root/internal/version/version.go"
if [ ! -f "$src" ]; then
  echo "找不到版本文件: $src" >&2
  exit 1
fi

# 只认 `const Version = "X.Y.Z"` 这一行，避免把注释或 Commit 误当版本。
v=$(sed -n 's/^const Version = "\([^"]*\)"$/\1/p' "$src")
if [ -z "$v" ]; then
  echo "无法从 $src 读取 const Version" >&2
  exit 1
fi

if ! printf '%s' "$v" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$'; then
  echo "Go 版本常量不是合法 semver（需要 X.Y.Z）: $v" >&2
  exit 1
fi

if [ "$ref_type" = "tag" ]; then
  if ! printf '%s' "$tag" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+$'; then
    echo "发布 tag 不是合法格式（需要 vX.Y.Z）: ${tag:-<空>}" >&2
    exit 1
  fi
  tv=${tag#v}
  if [ "$tv" != "$v" ]; then
    echo "tag($tag) 与 Go Version($v) 不一致" >&2
    exit 1
  fi
fi

printf '%s\n' "$v"
