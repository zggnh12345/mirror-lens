#!/usr/bin/env bash
# 3.5.15 -> 3.5.14c 回滚前置检查与执行。
#
# 默认只检查，不改任何文件：
#   scripts/rollback-3.5.15.sh --data /path/to/data
#
# 确认服务已停止后执行：
#   scripts/rollback-3.5.15.sh --data /path/to/data --apply --clean-cache
#
# 处理两件 1.6 D.3 要求：
#   1. 备份并移除 registry_creds.enc，避免 3.5.14c 的 HostProbe 把官方凭据
#      传给候选 Source；
#   2. 删除旧格式 docker:* 缓存索引，确保 3.5.14c 不会按旧键读到私有内容。
# 任何一步失败都退出非零；不会假装回滚成功。
set -euo pipefail

DATA=""
APPLY=0
CLEAN=0
usage() {
  sed -n '2,16p' "$0" | sed 's/^# \{0,1\}//'
  exit 2
}
while [ $# -gt 0 ]; do
  case "$1" in
    --data) DATA=${2:-}; shift 2 ;;
    --apply) APPLY=1; shift ;;
    --clean-cache) CLEAN=1; shift ;;
    -h|--help) usage ;;
    *) echo "[rollback] 未知参数: $1" >&2; usage ;;
  esac
done
[ -n "$DATA" ] || usage
[ -d "$DATA" ] || { echo "[rollback] 数据目录不存在: $DATA" >&2; exit 1; }
command -v python3 >/dev/null 2>&1 || { echo "[rollback] 需要 python3 检查 SQLite" >&2; exit 1; }

DATA=$(cd "$DATA" && pwd)
DB="$DATA/mirror-lens.db"
KEY="$DATA/secret.key"
CREDS="$DATA/registry_creds.enc"
[ -f "$DB" ] || { echo "[rollback] 缺少 $DB" >&2; exit 1; }
[ -f "$KEY" ] || { echo "[rollback] 缺少 $KEY；3.5.14c 无法解密数据库" >&2; exit 1; }

# 和服务共用数据目录锁；服务仍在运行就必须先停，不能热改凭据/缓存。
exec 9>"$DATA/.mirror-lens.lock"
if ! flock -n 9; then
  echo "[rollback] 数据目录被服务或迁移进程占用，请先停止服务" >&2
  exit 1
fi

count_old_cache() {
  python3 - "$DB" <<'PY'
import sqlite3, sys
c = sqlite3.connect(sys.argv[1])
print(c.execute("select count(*) from cache_entries where key like 'docker:%'").fetchone()[0])
PY
}

old=$(count_old_cache)
echo "[rollback] 数据目录: $DATA"
echo "[rollback] 旧格式 docker:* 缓存索引: $old 条"
if [ -f "$CREDS" ]; then
  echo "[rollback] registry_creds.enc: 存在，回滚前必须移走"
else
  echo "[rollback] registry_creds.enc: 不存在"
fi

if [ "$old" -gt 0 ] && [ "$CLEAN" = 0 ]; then
  echo "[rollback] 拒绝继续：旧 docker:* 缓存仍存在；确认清理后加 --clean-cache" >&2
  exit 1
fi
if [ "$APPLY" = 0 ]; then
  echo "[rollback] 检查模式：未改动任何文件；确认后加 --apply"
  exit 0
fi

STAMP=$(date +%Y%m%d%H%M%S)
BACKUP="$DATA/.rollback-3.5.15-$STAMP"
mkdir -m 700 "$BACKUP"
if [ -f "$CREDS" ]; then
  mv -f "$CREDS" "$BACKUP/registry_creds.enc"
  chmod 600 "$BACKUP/registry_creds.enc"
  sync
  echo "[rollback] 凭据已移至 $BACKUP/registry_creds.enc"
fi

if [ "$CLEAN" = 1 ] && [ "$old" -gt 0 ]; then
  python3 - "$DB" <<'PY'
import sqlite3, sys
c = sqlite3.connect(sys.argv[1])
n = c.execute("delete from cache_entries where key like 'docker:%'").rowcount
c.commit()
print(f"[rollback] 已删除旧 docker:* 缓存索引 {n} 条")
PY
  sync
fi

# 最终校验：凭据文件不应再出现在数据目录，旧键应为 0。
[ ! -f "$CREDS" ] || { echo "[rollback] 凭据文件仍在数据目录，回滚失败" >&2; exit 1; }
left=$(count_old_cache)
[ "$left" = 0 ] || { echo "[rollback] 仍有 $left 条旧缓存索引，回滚失败" >&2; exit 1; }
echo "[rollback] 完成：可部署 3.5.14c；凭据备份保留在 $BACKUP"
