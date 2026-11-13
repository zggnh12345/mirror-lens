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
DATA_IN=$DATA
DATA=$(cd "$DATA" 2>/dev/null && pwd) || die "数据目录不存在: $DATA_IN"
mkdir -p "$DATA"
exec 9>"$DATA/.restore.lock"
flock -n 9 || die "另一个恢复进程正在操作 $DATA（.restore.lock 被占用）"

JOURNAL_PATH="$DATA/$JOURNAL"

read_manifest() {
  MANIFEST_PATH="$UNIT/$MANIFEST"
  [ -f "$MANIFEST_PATH" ] || die "备份单元缺少 $MANIFEST: $UNIT"
  # manifest 是缩进 JSON，grep 按行工作会把嵌套对象截断；先压成单行。
  MFLAT=$(tr '\n' ' ' <"$MANIFEST_PATH" | tr -s ' ')
  SCHEMA=$(printf '%s' "$MFLAT" | grep -o '"schema"[[:space:]]*:[[:space:]]*[0-9]*' | head -1 | sed 's/.*:[[:space:]]*//')
  APP_VERSION=$(printf '%s' "$MFLAT" | grep -o '"app_version"[[:space:]]*:[[:space:]]*"[^"]*"' | head -1 | sed 's/.*:[[:space:]]*"//; s/"$//' || true)
  DB_FILE=$(printf '%s' "$MFLAT" | grep -o '"file"[[:space:]]*:[[:space:]]*"[^"]*"' | head -1 | sed 's/.*:[[:space:]]*"//; s/"$//')
  DB_SHA=$(printf '%s' "$MFLAT" | grep -o '"db"[^}]*' | grep -o '"sha256"[[:space:]]*:[[:space:]]*"[^"]*"' | sed 's/.*"\([0-9a-fA-F]*\)"/\1/')
  KEY_SHA=$(printf '%s' "$MFLAT" | grep -o '"key"[^}]*' | grep -o '"sha256"[[:space:]]*:[[:space:]]*"[^"]*"' | sed 's/.*"\([0-9a-fA-F]*\)"/\1/')
  DB_SIZE=$(printf '%s' "$MFLAT" | grep -o '"db"[^}]*' | grep -o '"size"[[:space:]]*:[[:space:]]*[0-9]*' | sed 's/.*:[[:space:]]*//')
  KEY_SIZE=$(printf '%s' "$MFLAT" | grep -o '"key"[^}]*' | grep -o '"size"[[:space:]]*:[[:space:]]*[0-9]*' | sed 's/.*:[[:space:]]*//')
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
  # 重入保护：trap 触发的失败不能再来一次回滚。
  if [ "${ROLLBACK_ACTIVE:-0}" = 1 ]; then
    log "回滚已在进行，跳过重入"
    return 1
  fi
  ROLLBACK_ACTIVE=1
  set +e
  log "回滚到恢复前配对（不删除任何现存文件）"
  local rc=0 chk failed
  chk="$(dirname "$DATA")/.ml-rollback-check-${TX:-$$}"
  failed="$DATA/.restore-failed-${TX:-$$}"
  # 重入保护：上一次回滚可能已被 SIGKILL，现场目录里已有文件。绝不覆盖它，
  # 换一个同事务的新目录继续保存本次现场。
  if [ -d "$failed" ] && [ -n "$(ls -A "$failed" 2>/dev/null)" ]; then
    local base=$failed n=1
    while [ -e "$failed" ]; do
      failed="$base.$n"
      n=$((n + 1))
    done
  fi

  # 1. 旧配对必须可证明完整：有记录的哈希、两个文件都在、内容匹配。
  #    任何一项不满足都保持正式目录与 OLD 原样，journal 保留。
  if [ -z "${OLD_DB_SHA:-}" ] || [ -z "${OLD_KEY_SHA:-}" ]; then
    log "错误: 没有可校验的旧配对（old_db_sha/old_key_sha 为空）；正式目录与 $OLD 保持原样，journal 保留"
    set -e; return 1
  fi
  if [ ! -f "$OLD/$DB_NAME" ] || [ ! -f "$OLD/$KEY_NAME" ]; then
    log "错误: 旧配对不完整（$OLD 缺 DB 或 key）；正式目录与现场保持原样，journal 保留"
    set -e; return 1
  fi
  if ! verify_pair "$OLD/$DB_NAME" "$OLD/$KEY_NAME" "$OLD_DB_SHA" "$OLD_KEY_SHA"; then
    log "错误: 旧配对 SHA-256 校验失败；正式目录与现场保持原样，journal 保留"
    set -e; return 1
  fi

  # 2. 在数据目录外复制一份并通过复核，之后所有切换都基于这份副本。
  rm -rf "$chk"
  if ! mkdir -p "$chk" || ! chmod 700 "$chk"; then
    log "错误: 无法创建回滚暂存目录 $chk；未改动任何文件"
    set -e; return 1
  fi
  cp -f "$OLD/$DB_NAME" "$chk/$DB_NAME" || rc=1
  cp -f "$OLD/$KEY_NAME" "$chk/$KEY_NAME" || rc=1
  for f in "$DB_NAME-wal" "$DB_NAME-shm"; do
    [ -f "$OLD/$f" ] && { cp -f "$OLD/$f" "$chk/$f" || rc=1; }
  done
  sync
  if [ $rc -ne 0 ] || ! verify_pair "$chk/$DB_NAME" "$chk/$KEY_NAME" "$OLD_DB_SHA" "$OLD_KEY_SHA"; then
    log "错误: 回滚副本复制/校验失败；正式目录与现场保持原样，journal 保留"
    set -e; return 1
  fi

  # 3. 当前正式文件移入失败现场（不删除），再从副本落回旧配对。
  if ! mkdir -p "$failed" || ! chmod 700 "$failed"; then
    log "错误: 无法创建失败现场目录 $failed；未改动任何文件"
    set -e; return 1
  fi
  for f in "$DB_NAME" "$DB_NAME-wal" "$DB_NAME-shm" "$KEY_NAME"; do
    if [ -f "$DATA/$f" ]; then
      mv -f "$DATA/$f" "$failed/$f" || rc=1
    fi
  done
  sync
  if [ $rc -ne 0 ]; then
    log "错误: 保存当前配对失败；$failed 与 $chk 均保留，journal 保留"
    set -e; return 1
  fi
  # 故障注入点：测试用 SIGKILL 打断「已保存现场、尚未落回旧配对」。
  if [ "${RESTORE_TEST_PAUSE_AFTER_FAILED:-0}" = 1 ]; then
    : >"$DATA/.restore-test-paused"
    # bash 内建 read：不起子进程，SIGKILL 父进程时不会留下仍持有 flock 的 sleep。
    read -r -t 120 _ </dev/null || true
  fi
  mv -f "$chk/$DB_NAME" "$DATA/$DB_NAME" || rc=1
  for f in "$DB_NAME-wal" "$DB_NAME-shm"; do
    [ -f "$chk/$f" ] && { mv -f "$chk/$f" "$DATA/$f" || rc=1; }
  done
  mv -f "$chk/$KEY_NAME" "$DATA/$KEY_NAME" || rc=1
  chmod 600 "$DATA/$DB_NAME" "$DATA/$KEY_NAME" 2>/dev/null
  sync
  if [ $rc -ne 0 ] || ! verify_pair "$DATA/$DB_NAME" "$DATA/$KEY_NAME" "$OLD_DB_SHA" "$OLD_KEY_SHA"; then
    log "错误: 回滚后校验失败；$failed、$OLD、$chk 均保留，journal 保留，请人工处理"
    set -e; return 1
  fi

  rm -f "$JOURNAL_PATH"
  sync
  rmdir "$chk" 2>/dev/null
  log "已回滚；失败现场保留在 $failed，恢复前配对仍在 $OLD"
  set -e; return 0
}

fail_restore() { # $1=错误信息：先尝试回滚，回滚失败则保留现场
  local msg=$1
  rollback || true
  die "$msg"
}

trap 'rc=$?; if [ $rc -ne 0 ] && [ -f "$JOURNAL_PATH" ]; then rollback || true; fi; exit $rc' ERR

if [ "$MODE" = recover ]; then
  if [ ! -f "$JOURNAL_PATH" ]; then
    log "没有未完成的恢复（$JOURNAL_PATH 不存在），无需处理"
    exit 0
  fi
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
      fail_restore "补齐 key 后校验失败，已尝试回滚"
    rm -f "$JOURNAL_PATH"; sync
    log "已补完新配对（DB 已切换，key 先前未落位）"
  elif [ ! -f "$DATA/$DB_NAME" ] && [ -f "$STAGE/$DB_NAME" ] && [ -f "$STAGE/$KEY_NAME" ]; then
    # 数据目录里还没有 DB（切换前中断，或首次安装）：暂存区仍完整，
    # 继续向前完成比回滚更安全，旧配对（若有）仍原样留在 $OLD。
    mv -f "$STAGE/$DB_NAME" "$DATA/$DB_NAME"
    mv -f "$STAGE/$KEY_NAME" "$DATA/$KEY_NAME"
    chmod 600 "$DATA/$DB_NAME" "$DATA/$KEY_NAME"
    sync
    verify_pair "$DATA/$DB_NAME" "$DATA/$KEY_NAME" "$DB_SHA" "$KEY_SHA" ||
      fail_restore "补齐新配对后校验失败，已尝试回滚"
    rm -f "$JOURNAL_PATH"; sync
    log "已补完新配对（数据目录此前没有 DB）"
  elif [ -f "$OLD/$DB_NAME" ] || [ -f "$OLD/$KEY_NAME" ]; then
    rollback || die "回滚失败：正式目录与 $OLD 均保持原样、journal 未清除，请人工处理"
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
  fail_restore "切换后校验失败，已尝试回滚"
# 旧 WAL/SHM 已随旧配对移入 OLD；正式目录里再出现就是被错误关联的残留。
for f in "$DB_NAME-wal" "$DB_NAME-shm"; do
  if [ -f "$DATA/$f" ]; then
    fail_restore "恢复后数据目录仍有 $f，拒绝清除切换日志，已尝试回滚"
  fi
done
crashpoint after_verify

rm -f "$JOURNAL_PATH"
sync
rmdir "$STAGE" 2>/dev/null || true
log "恢复完成：$UNIT -> $DATA（恢复前配对保存在 $OLD）"
