#!/usr/bin/env bash
# 从 3.5.4 备份单元恢复 mirror-lens 的 DB/key 配对。
#
#   restore.sh --backup <单元目录> --data <数据目录>
#   restore.sh --recover --data <数据目录>
#
# 备份单元 = mirror-lens.db + secret.key + manifest.json（manifest 最后发布）。
# 恢复按「校验 → 暂存 → 保存现场 → 配对切换 → 复核」执行；任何一步失败都会
# 回滚到原配对。切换过程写 .restore-journal.json，服务启动看到该文件会拒绝
# 启动，必须先用 --recover 完成或回滚。
set -euo pipefail

DB_NAME=mirror-lens.db
KEY_NAME=secret.key
MANIFEST=manifest.json
JOURNAL=.restore-journal.json
SELF=$(basename "$0")

die() { echo "[restore] 错误: $*" >&2; exit 1; }
log() { echo "[restore] $*"; }
crashpoint() {
  if [ "${RESTORE_TEST_CRASH:-}" = "$1" ]; then
    log "故障注入：$1 后 SIGKILL"
    kill -9 $$
  fi
}

sha() { sha256sum "$1" | awk '{print $1}'; }
fsize() { stat -c %s "$1"; }
field() { # field <key> 从 manifest 取字符串值
  grep -o "\"$1\"[[:space:]]*:[[:space:]]*\"[^\"]*\"" "$MANIFEST_PATH" | head -1 |
    sed 's/.*:[[:space:]]*"//; s/"$//'
}
numfield() {
  grep -o "\"$1\"[[:space:]]*:[[:space:]]*[0-9]*" "$MANIFEST_PATH" | head -1 |
    sed 's/.*:[[:space:]]*//'
}

usage() {
  sed -n '2,9p' "$0" | sed 's/^# \{0,1\}//'
  exit 2
}

DATA=""
UNIT=""
MODE=restore
for tool in flock sha256sum stat; do
  command -v "$tool" >/dev/null 2>&1 || { echo "[restore] 错误: 缺少 $tool" >&2; exit 1; }
done
while [ $# -gt 0 ]; do
  case "$1" in
    --backup) UNIT=${2:-}; shift 2 ;;
    --data) DATA=${2:-}; shift 2 ;;
    --recover) MODE=recover; shift ;;
    -h|--help) usage ;;
    *) die "未知参数 $1" ;;
  esac
done
[ -n "$DATA" ] || usage
DATA=$(cd "$DATA" 2>/dev/null && pwd) || die "数据目录不存在: $DATA"
mkdir -p "$DATA"
exec 9>"$DATA/.restore.lock"
flock -n 9 || die "另一个恢复进程正在操作 $DATA（.restore.lock 被占用）"

JOURNAL_PATH="$DATA/$JOURNAL"

read_manifest() {
  MANIFEST_PATH="$UNIT/$MANIFEST"
  [ -f "$MANIFEST_PATH" ] || die "备份单元缺少 $MANIFEST: $UNIT"
  SCHEMA=$(numfield schema)
  APP_VERSION=$(field app_version)
  DB_FILE=$(field file | head -1)
  DB_SHA=$(grep -o '"db"[^}]*' "$MANIFEST_PATH" | grep -o '"sha256"[[:space:]]*:[[:space:]]*"[^"]*"' | sed 's/.*"\([0-9a-fA-F]*\)"/\1/')
  KEY_SHA=$(grep -o '"key"[^}]*' "$MANIFEST_PATH" | grep -o '"sha256"[[:space:]]*:[[:space:]]*"[^"]*"' | sed 's/.*"\([0-9a-fA-F]*\)"/\1/')
  DB_SIZE=$(grep -o '"db"[^}]*' "$MANIFEST_PATH" | grep -o '"size"[[:space:]]*:[[:space:]]*[0-9]*' | sed 's/.*:[[:space:]]*//')
  KEY_SIZE=$(grep -o '"key"[^}]*' "$MANIFEST_PATH" | grep -o '"size"[[:space:]]*:[[:space:]]*[0-9]*' | sed 's/.*:[[:space:]]*//')
  [ "$SCHEMA" = "1" ] || die "manifest schema 不支持: $SCHEMA"
  [ -n "$APP_VERSION" ] || die "manifest 缺少 app_version"
  [ -n "$DB_SHA" ] && [ -n "$KEY_SHA" ] || die "manifest 缺少 sha256"
  [ "$DB_FILE" = "$DB_NAME" ] || die "manifest 的 db.file 非法: $DB_FILE"
  [ -f "$UNIT/$DB_NAME" ] || die "备份单元缺少 $DB_NAME"
  [ -f "$UNIT/$KEY_NAME" ] || die "备份单元缺少 $KEY_NAME"
  [ "$(fsize "$UNIT/$DB_NAME")" = "$DB_SIZE" ] || die "DB 字节数与 manifest 不符"
  [ "$(fsize "$UNIT/$KEY_NAME")" = "$KEY_SIZE" ] || die "key 字节数与 manifest 不符"
  [ "$(sha "$UNIT/$DB_NAME")" = "$DB_SHA" ] || die "DB SHA-256 与 manifest 不符"
  [ "$(sha "$UNIT/$KEY_NAME")" = "$KEY_SHA" ] || die "key SHA-256 与 manifest 不符"
}

write_journal() {
  local tmp="$JOURNAL_PATH.tmp"
  cat >"$tmp" <<EOF
{"txid":"$TX","unit":"$UNIT","stage":"$STAGE","old_dir":"$OLD",
 "db_sha":"$DB_SHA","db_size":$DB_SIZE,"key_sha":"$KEY_SHA","key_size":$KEY_SIZE,
 "old_db_sha":"$OLD_DB_SHA","old_key_sha":"$OLD_KEY_SHA"}
EOF
  sync "$tmp" 2>/dev/null || sync
  mv -f "$tmp" "$JOURNAL_PATH"
  sync
}

verify_pair() { # verify_pair <db> <key> <sha_db> <sha_key>
  [ -f "$1" ] && [ -f "$2" ] || return 1
  [ "$(sha "$1")" = "$3" ] || return 1
  [ "$(sha "$2")" = "$4" ] || return 1
}

rollback() {
  log "回滚到恢复前配对"
  rm -f "$DATA/$DB_NAME" "$DATA/$DB_NAME-wal" "$DATA/$DB_NAME-shm" "$DATA/$KEY_NAME"
  [ -f "$OLD/$DB_NAME" ] && mv -f "$OLD/$DB_NAME" "$DATA/$DB_NAME"
  [ -f "$OLD/$DB_NAME-wal" ] && mv -f "$OLD/$DB_NAME-wal" "$DATA/$DB_NAME-wal"
  [ -f "$OLD/$DB_NAME-shm" ] && mv -f "$OLD/$DB_NAME-shm" "$DATA/$DB_NAME-shm"
  [ -f "$OLD/$KEY_NAME" ] && mv -f "$OLD/$KEY_NAME" "$DATA/$KEY_NAME"
  sync
  if [ -n "${OLD_DB_SHA:-}" ] && [ -n "${OLD_KEY_SHA:-}" ]; then
    verify_pair "$DATA/$DB_NAME" "$DATA/$KEY_NAME" "$OLD_DB_SHA" "$OLD_KEY_SHA" ||
      die "回滚后校验失败，现场保留在 $OLD，请人工处理"
  fi
  rm -f "$JOURNAL_PATH"
  sync
  log "已回滚；恢复前配对仍在 $OLD"
}

trap 'rc=$?; if [ $rc -ne 0 ] && [ -f "$JOURNAL_PATH" ]; then rollback || true; fi; exit $rc' ERR

if [ "$MODE" = recover ]; then
  [ -f "$JOURNAL_PATH" ] || die "没有未完成的恢复（$JOURNAL_PATH 不存在）"
  # 极简解析：脚本自己写出的扁平 JSON。
  get() { grep -o "\"$1\"[[:space:]]*:[[:space:]]*\(\"[^\"]*\"\|[0-9]*\)" "$JOURNAL_PATH" | head -1 | sed 's/.*:[[:space:]]*//; s/"//g'; }
  TX=$(get txid); UNIT=$(get unit); STAGE=$(get stage); OLD=$(get old_dir)
  DB_SHA=$(get db_sha); KEY_SHA=$(get key_sha)
  OLD_DB_SHA=$(get old_db_sha); OLD_KEY_SHA=$(get old_key_sha)
  if verify_pair "$DATA/$DB_NAME" "$DATA/$KEY_NAME" "$DB_SHA" "$KEY_SHA"; then
    rm -f "$JOURNAL_PATH"; sync
    [ -n "${STAGE:-}" ] && rm -rf "$STAGE"
    log "上次恢复其实已完成，已清理切换日志"
  elif [ -n "$OLD_DB_SHA" ] && [ -n "$OLD_KEY_SHA" ] &&
       verify_pair "$DATA/$DB_NAME" "$DATA/$KEY_NAME" "$OLD_DB_SHA" "$OLD_KEY_SHA"; then
    rm -f "$JOURNAL_PATH"; sync
    [ -n "${STAGE:-}" ] && rm -rf "$STAGE"
    log "切换尚未开始（原配对完好），已清理切换日志"
  elif [ -f "$STAGE/$KEY_NAME" ] && [ "$(sha "$DATA/$DB_NAME" 2>/dev/null)" = "$DB_SHA" ]; then
    mv -f "$STAGE/$KEY_NAME" "$DATA/$KEY_NAME"; sync
    verify_pair "$DATA/$DB_NAME" "$DATA/$KEY_NAME" "$DB_SHA" "$KEY_SHA" ||
      die "补齐 key 后校验失败，现场保留，请人工处理"
    rm -f "$JOURNAL_PATH"; sync
    log "已补完新配对（DB 已切换，key 先前未落位）"
  elif [ -f "$OLD/$DB_NAME" ] || [ -f "$OLD/$KEY_NAME" ]; then
    rollback
  else
    die "无法自动判定恢复方向；DB/key/日志均保留，请人工处理"
  fi
  exit 0
fi

[ -n "$UNIT" ] || usage
UNIT=$(cd "$UNIT" 2>/dev/null && pwd) || die "备份单元不存在: $UNIT"
[ -f "$JOURNAL_PATH" ] && die "存在未完成的恢复，请先执行 $SELF --recover --data $DATA"
read_manifest

TX=$(date +%Y%m%d%H%M%S)-$$
STAGE="$(dirname "$DATA")/.ml-restore-stage-$TX"
OLD="$DATA/.restore-old-$TX"
OLD_DB_SHA=""; OLD_KEY_SHA=""
[ -f "$DATA/$DB_NAME" ] && OLD_DB_SHA=$(sha "$DATA/$DB_NAME")
[ -f "$DATA/$KEY_NAME" ] && OLD_KEY_SHA=$(sha "$DATA/$KEY_NAME")

mkdir -p "$STAGE" "$OLD"
chmod 700 "$STAGE" "$OLD"
write_journal
crashpoint after_journal

cp -f "$UNIT/$DB_NAME" "$STAGE/$DB_NAME"
cp -f "$UNIT/$KEY_NAME" "$STAGE/$KEY_NAME"
chmod 600 "$STAGE/$DB_NAME" "$STAGE/$KEY_NAME"
sync
crashpoint after_stage

# 保存现场：先移走旧配对，再落新配对。日志已存在，任何中断都可 --recover。
for f in "$DB_NAME" "$DB_NAME-wal" "$DB_NAME-shm" "$KEY_NAME"; do
  [ -f "$DATA/$f" ] && mv -f "$DATA/$f" "$OLD/$f"
done
sync
crashpoint after_save

mv -f "$STAGE/$DB_NAME" "$DATA/$DB_NAME"
sync
crashpoint after_db

mv -f "$STAGE/$KEY_NAME" "$DATA/$KEY_NAME"
chmod 600 "$DATA/$KEY_NAME"
sync
crashpoint after_key

verify_pair "$DATA/$DB_NAME" "$DATA/$KEY_NAME" "$DB_SHA" "$KEY_SHA" ||
  die "切换后校验失败"
crashpoint after_verify

rm -f "$JOURNAL_PATH"
sync
rmdir "$STAGE" 2>/dev/null || true
log "恢复完成：$UNIT -> $DATA（恢复前配对保存在 $OLD）"
