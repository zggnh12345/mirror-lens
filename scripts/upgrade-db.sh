#!/usr/bin/env bash
# 离线升级 mirror-lens 数据库到 Canonical Schema（3.5.5）。
#
#   upgrade-db.sh --data <数据目录> --tool <db-migrate> --container <容器> \\
#                  [--expect-image <镜像>] --baseline 3.4.6|3.5.4
#   upgrade-db.sh --recover --data <数据目录> --tool <db-migrate> --container <容器>
#
# 约束：
#   - 必须确认目标容器存在、镜像身份匹配且已经停止；脚本不会替你停容器。
#   - 不依赖旧版本不会创建的 .mirror-lens.lock 来判断服务状态。
#   - 升级前检查没有进程仍持有正式库/WAL/SHM；有则拒绝。
#   - 正式库从不被原地改写；先复制到 staging，checkpoint 后由 db-migrate 只读搬迁。
#   - checkpoint 后 staging 不得残留 -wal/-shm。
#   - 切换前把正式库复制成 mirror-lens.db.pre-3.5.5，并 fsync + sha256。
#   - secret.key 不参与切换、不被替换；另存一份 600 的 secret.key.pre-3.5.5
#     并写 manifest（DB/key 的 SHA-256），只用于灾难恢复核对。
#   - 切换中间态写 .db-upgrade-journal.json；服务启动见到它会 fail closed，
#     只能用 --recover 完成或回滚。
set -euo pipefail

DB_NAME=mirror-lens.db
PRE_NAME=mirror-lens.db.pre-3.5.5
KEY_NAME=secret.key
KEY_PRE_NAME=secret.key.pre-3.5.5
MANIFEST_NAME=mirror-lens.db.pre-3.5.5.manifest
JOURNAL=.db-upgrade-journal.json
TOOL=./db-migrate
CONTAINER=""
EXPECT_IMAGE=""
DOCKER=${DOCKER:-docker}

die() { echo "[upgrade-db] 错误: $*" >&2; exit 1; }
log() { echo "[upgrade-db] $*"; }
crashpoint() {
  if [ "${DB_UPGRADE_CRASH:-}" = "$1" ]; then
    log "故障注入：$1 后 SIGKILL"
    kill -9 $$
  fi
}
sha() { sha256sum "$1" | awk '{print $1}'; }
fsync_path() {
  # GNU coreutils 的 sync -f 只同步指定文件；不支持时退回全局 sync。
  sync -f "$1" 2>/dev/null || sync
}
field() { # field <key> 从单行 journal 取字符串值
  grep -o "\"$1\"[[:space:]]*:[[:space:]]*\"[^\"]*\"" "$JOURNAL_PATH" | head -1 |
    sed 's/.*:[[:space:]]*"//; s/"$//'
}

# 服务状态的唯一可信来源是容器本身，不是旧版本不一定创建的锁文件。
# 本函数只检查，不停止、不重启容器；生产停机必须由操作者单独完成。
check_container_stopped() {
  [ -n "$CONTAINER" ] || die "缺少 --container；升级/恢复必须指定生产容器"
  command -v "$DOCKER" >/dev/null 2>&1 || die "找不到 docker 命令: $DOCKER"
  local name image running
  name=$("$DOCKER" inspect --format '{{.Name}}' "$CONTAINER" 2>/dev/null) ||
    die "目标容器不存在或不可 inspect: $CONTAINER"
  [ "$name" = "/$CONTAINER" ] || [ "$name" = "$CONTAINER" ] ||
    die "容器身份不匹配：请求 $CONTAINER，inspect 返回 $name"
  image=$("$DOCKER" inspect --format '{{.Config.Image}}' "$CONTAINER" 2>/dev/null) ||
    die "无法读取容器镜像: $CONTAINER"
  if [ -n "$EXPECT_IMAGE" ] && [ "$image" != "$EXPECT_IMAGE" ]; then
    die "容器镜像不匹配：期望 $EXPECT_IMAGE，实际 $image"
  fi
  running=$("$DOCKER" inspect --format '{{.State.Running}}' "$CONTAINER" 2>/dev/null) ||
    die "无法读取容器运行状态: $CONTAINER"
  case "$running" in
    false) ;;
    true) die "容器 $CONTAINER 仍在运行；请先单独停机，脚本不会替你停容器" ;;
    *) die "无法识别容器运行状态: $CONTAINER=$running" ;;
  esac
}

# 不依赖 fuser/lsof：直接扫描 /proc 的 fd 链接，能看到宿主上的服务进程。
check_no_db_holders() {
  local fd target pids=""
  for fd in /proc/[0-9]*/fd/*; do
    [ -e "$fd" ] || continue
    target=$(readlink "$fd" 2>/dev/null) || continue
    case "$target" in
      "$DB"|"$DB-wal"|"$DB-shm"|"$DB (deleted)"|"$DB-wal (deleted)"|"$DB-shm (deleted)")
        pids="$pids ${fd#/proc/}"
        ;;
    esac
  done
  [ -z "$pids" ] || die "仍有进程持有正式库/WAL/SHM:$pids；先停服务并确认进程退出"
}

usage() {
  sed -n '2,15p' "$0" | sed 's/^# \{0,1\}//'
  exit 2
}

DATA=""
MODE=upgrade
BASELINE=""
for tool in flock sha256sum stat sync; do
  command -v "$tool" >/dev/null 2>&1 || die "缺少 $tool"
done
while [ $# -gt 0 ]; do
  case "$1" in
    --data) DATA=${2:-}; shift 2 ;;
    --tool) TOOL=${2:-}; shift 2 ;;
    --baseline) BASELINE=${2:-}; shift 2 ;;
    --container) CONTAINER=${2:-}; shift 2 ;;
    --expect-image) EXPECT_IMAGE=${2:-}; shift 2 ;;
    --recover) MODE=recover; shift ;;
    -h|--help) usage ;;
    *) die "未知参数 $1" ;;
  esac
done
[ -n "$DATA" ] || usage
DATA_IN=$DATA
DATA=$(cd "$DATA" 2>/dev/null && pwd) || die "数据目录不存在: $DATA_IN"
[ -x "$TOOL" ] || die "db-migrate 不可执行: $TOOL"

JOURNAL_PATH="$DATA/$JOURNAL"
DB="$DATA/$DB_NAME"
PRE="$DATA/$PRE_NAME"
KEY="$DATA/$KEY_NAME"
KEY_PRE="$DATA/$KEY_PRE_NAME"
MANIFEST="$DATA/$MANIFEST_NAME"

check_container_stopped
check_no_db_holders

# 与服务、restore.sh、离线迁移共用同一把锁；但锁不再是服务运行状态的证据。
exec 8>"$DATA/.mirror-lens.lock"
chmod 600 "$DATA/.mirror-lens.lock" 2>/dev/null || true
flock -n 8 || die "另一个迁移进程正在操作 $DATA（.mirror-lens.lock 被占用）"

recover() {
  [ -f "$JOURNAL_PATH" ] || { log "没有未完成的升级，无需恢复"; return 0; }
  STATE=$(field state)
  WORK_SAVED=$(field work_dir)
  # 备份阶段中断：正式库还没有被移动，直接清掉半成品备份后重跑即可。
  if [ "$STATE" = "backup" ]; then
    if [ -f "$DB" ] && ! "$TOOL" verify --db "$DB" >/dev/null 2>&1; then
      log "备份阶段中断，正式库未改动；清理半成品备份"
      rm -f "$PRE" "$PRE-wal" "$PRE-shm" "$JOURNAL_PATH" "$KEY_PRE" "$MANIFEST"
      [ -n "$WORK_SAVED" ] && rm -rf "$WORK_SAVED"
      return 0
    fi
    die "journal 处于 backup 状态但正式库已是 Canonical，拒绝自动处理"
  fi
  PRE_SAVED=$(field backup_db)
  PRE_SHA=$(field backup_sha)
  [ -n "$PRE_SAVED" ] && [ -n "$PRE_SHA" ] || die "journal 不完整，拒绝自动恢复"
  if [ -f "$DB" ] && "$TOOL" verify --db "$DB" >/dev/null 2>&1; then
    log "正式库已是完整 Canonical 库，清除 journal"
    rm -f "$JOURNAL_PATH"
    [ -n "$WORK_SAVED" ] && rm -rf "$WORK_SAVED"
    return 0
  fi
  [ -f "$PRE_SAVED" ] || die "备份库不存在: $PRE_SAVED"
  [ "$(sha "$PRE_SAVED")" = "$PRE_SHA" ] || die "备份库 SHA-256 不符，拒绝恢复"
  # key 不在切换中替换；manifest 只用来发现“升级期间 key 被换过”的情况。
  if [ -f "$MANIFEST" ]; then
    KEY_SHA_SAVED=$(grep '^key_sha=' "$MANIFEST" | cut -d= -f2)
    [ -f "$KEY" ] || die "manifest 存在但 secret.key 缺失，拒绝自动恢复"
    [ -z "$KEY_SHA_SAVED" ] || [ "$(sha "$KEY")" = "$KEY_SHA_SAVED" ] ||
      die "secret.key 与升级前 manifest 不符，拒绝自动恢复"
  fi
  log "回滚到升级前数据库"
  rm -f "$DB" "$DB-wal" "$DB-shm"
  cp --reflink=auto "$PRE_SAVED" "$DB"
  [ -f "$PRE_SAVED-wal" ] && cp --reflink=auto "$PRE_SAVED-wal" "$DB-wal"
  [ -f "$PRE_SAVED-shm" ] && cp --reflink=auto "$PRE_SAVED-shm" "$DB-shm"
  chmod 600 "$DB" "$DB-wal" "$DB-shm" 2>/dev/null || true
  fsync_path "$DB"
  sync
  [ "$(sha "$DB")" = "$PRE_SHA" ] || die "回滚后 SHA-256 不符，请人工处理"
  rm -f "$JOURNAL_PATH"
  [ -n "$WORK_SAVED" ] && rm -rf "$WORK_SAVED"
  log "回滚完成；secret.key 未改动"
  return 0
}

if [ "$MODE" = recover ]; then
  recover
  exit 0
fi

[ -n "$BASELINE" ] || die "缺少 --baseline 3.4.6|3.5.4"
[ -f "$DB" ] || die "正式库不存在: $DB"
[ -f "$KEY" ] || die "secret.key 不存在；拒绝在缺少配对密钥时升级"
[ ! -f "$JOURNAL_PATH" ] || die "存在未完成的升级 journal，先运行 --recover"
[ ! -e "$PRE" ] || die "备份已存在，拒绝覆盖: $PRE"
[ ! -e "$KEY_PRE" ] || die "key 备份已存在，拒绝覆盖: $KEY_PRE"
[ ! -e "$MANIFEST" ] || die "备份 manifest 已存在，拒绝覆盖: $MANIFEST"

TS=$(date +%Y%m%d-%H%M%S)
WORK="$DATA/.db-upgrade-$TS"
mkdir -p "$WORK"
trap 'rm -rf "$WORK" 2>/dev/null || true' EXIT

log "复制正式库到 staging（正式库只读）"
cp --reflink=auto "$DB" "$WORK/old.db"
[ -f "$DB-wal" ] && cp --reflink=auto "$DB-wal" "$WORK/old.db-wal"
[ -f "$DB-shm" ] && cp --reflink=auto "$DB-shm" "$WORK/old.db-shm"
fsync_path "$WORK/old.db"
log "在 staging 上 checkpoint WAL"
"$TOOL" checkpoint --db "$WORK/old.db"
[ ! -e "$WORK/old.db-wal" ] || die "staging checkpoint 后仍残留 old.db-wal，拒绝迁移"
[ ! -e "$WORK/old.db-shm" ] || die "staging checkpoint 后仍残留 old.db-shm，拒绝迁移"
log "迁移到 Canonical"
"$TOOL" migrate --in "$WORK/old.db" --out "$WORK/new.db" --baseline "$BASELINE"
"$TOOL" verify --db "$WORK/new.db"
crashpoint after_migrate

log "备份正式库到 $PRE_NAME"
# 先进 backup 状态：即使复制到一半崩溃，--recover 也知道正式库尚未被移动。
printf '{"state":"backup","work_dir":"%s"}\n' "$WORK" >"$JOURNAL_PATH"
fsync_path "$JOURNAL_PATH"
cp --reflink=auto "$DB" "$PRE"
[ -f "$DB-wal" ] && cp --reflink=auto "$DB-wal" "$PRE-wal"
[ -f "$DB-shm" ] && cp --reflink=auto "$DB-shm" "$PRE-shm"
chmod 600 "$PRE" "$PRE-wal" "$PRE-shm" 2>/dev/null || true
fsync_path "$PRE"
sync
PRE_SHA=$(sha "$PRE")
# key 不参与切换；只额外留一份 600 的灾难恢复副本，并记录它和 DB 的哈希。
cp --reflink=auto "$KEY" "$KEY_PRE"
chmod 600 "$KEY_PRE"
fsync_path "$KEY_PRE"
KEY_SHA=$(sha "$KEY")
printf 'db_sha=%s\nkey_sha=%s\ntool_sha=%s\nbaseline=%s\ncreated=%s\n' \
  "$PRE_SHA" "$KEY_SHA" "$(sha "$TOOL")" "$BASELINE" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" >"$MANIFEST"
fsync_path "$MANIFEST"
sync
crashpoint after_backup

# 备份已完成；journal 切换到 switching 后才能动正式库，并 fsync 目录项。
printf '{"state":"switching","backup_db":"%s","backup_sha":"%s","work_dir":"%s"}\n' \
  "$PRE" "$PRE_SHA" "$WORK" >"$JOURNAL_PATH"
fsync_path "$JOURNAL_PATH"
crashpoint after_journal

# 正式库的 -wal/-shm 属于旧库，切换时一并移走，避免新库误读旧 WAL。
rm -f "$DB" "$DB-wal" "$DB-shm"
crashpoint after_remove_old
mv "$WORK/new.db" "$DB"
chmod 600 "$DB"
fsync_path "$DB"
sync
crashpoint after_switch

"$TOOL" verify --db "$DB" || die "切换后校验失败；journal 保留，请运行 --recover"
rm -f "$JOURNAL_PATH"
rm -rf "$WORK"
trap - EXIT
log "完成：$DB -> Canonical 3.5.5；回滚副本 $PRE（含 -wal/-shm，如存在）"
log "灾难恢复材料：$KEY_PRE（600）、$MANIFEST；secret.key 本体未改动"
