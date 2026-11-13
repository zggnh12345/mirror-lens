#!/usr/bin/env bash
# 离线升级 mirror-lens 数据库到 Canonical Schema（3.5.6）。
#
#   upgrade-db.sh --data <数据目录> --tool <db-migrate> --container <容器> \
#                  --expect-image <镜像> --expect-image-id sha256:... --baseline 3.4.6|3.5.4
#   upgrade-db.sh --recover --data <数据目录> --tool <db-migrate> --container <容器> \
#                  --expect-image <镜像> --expect-image-id sha256:...
#
# 约束：
#   - 必须确认目标容器存在、镜像身份匹配且已经停止；脚本不会替你停容器。
#     --expect-image-id 是必填的不可变身份；--expect-image 只做可选 tag 校验。
#     绝不只比较可变 tag。
#   - 不依赖旧版本不会创建的 .mirror-lens.lock 来判断服务状态。
#   - 升级前检查没有进程仍持有正式库/WAL/SHM；有则拒绝。
#     该检查需要 root 或能读取所有其他 UID 的 /proc/<pid>/fd；读不到就
#     fail-closed（M6），不会把“未知”当成 0 个持有者。
#   - 正式库从不被原地改写；先复制到 staging，checkpoint 后由 db-migrate 只读搬迁。
#   - checkpoint 后 staging 不得残留 -wal/-shm。
#   - 切换前把正式库复制成 mirror-lens.db.pre-3.5.6，并 fsync + sha256。
#   - secret.key 不参与切换、不被替换；另存一份 600 的 secret.key.pre-3.5.6
#     并写 manifest（DB/key 的 SHA-256），只用于灾难恢复核对。
#   - 切换中间态写 .db-upgrade-journal.json；服务启动见到它会 fail closed，
#     只能用 --recover 完成或回滚。
set -euo pipefail

DB_NAME=mirror-lens.db
PRE_NAME=mirror-lens.db.pre-3.5.6
KEY_NAME=secret.key
KEY_PRE_NAME=secret.key.pre-3.5.6
MANIFEST_NAME=mirror-lens.db.pre-3.5.6.manifest
JOURNAL=.db-upgrade-journal.json
TOOL=./db-migrate
CONTAINER=""
EXPECT_IMAGE=""
EXPECT_IMAGE_ID=""
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
  [ -n "$EXPECT_IMAGE_ID" ] ||
    die "缺少 --expect-image-id；升级必须提供不可变镜像 ID（sha256:...），不能只凭可变 tag 通过身份校验"
  local name image image_id state
  name=$("$DOCKER" inspect --format '{{.Name}}' "$CONTAINER" 2>/dev/null) ||
    die "目标容器不存在或不可 inspect: $CONTAINER"
  [ "$name" = "/$CONTAINER" ] || [ "$name" = "$CONTAINER" ] ||
    die "容器身份不匹配：请求 $CONTAINER，inspect 返回 $name"
  image_id=$("$DOCKER" inspect --format '{{.Image}}' "$CONTAINER" 2>/dev/null) ||
    die "无法读取容器实际镜像 ID: $CONTAINER"
  [ -n "$image_id" ] || die "容器实际镜像 ID 为空；拒绝继续: $CONTAINER"
  if [ -n "$EXPECT_IMAGE_ID" ]; then
    [ "$image_id" = "$EXPECT_IMAGE_ID" ] ||
      die "容器镜像 ID 不匹配：期望 $EXPECT_IMAGE_ID，实际 $image_id"
  fi
  if [ -n "$EXPECT_IMAGE" ]; then
    image=$("$DOCKER" inspect --format '{{.Config.Image}}' "$CONTAINER" 2>/dev/null) ||
      die "无法读取容器镜像 tag: $CONTAINER"
    [ "$image" = "$EXPECT_IMAGE" ] ||
      die "容器镜像 tag 不匹配：期望 $EXPECT_IMAGE，实际 $image"
  fi
  # 不只看 Running：restarting/dead/paused 的 Running 也可能是 false，
  # 但都不是可以安全搬运数据库的“已停止”状态。
  state=$("$DOCKER" inspect --format '{{.State.Status}}' "$CONTAINER" 2>/dev/null) ||
    die "无法读取容器运行状态: $CONTAINER"
  case "$state" in
    exited|created) ;;
    *) die "容器 $CONTAINER 状态是 $state，不是已停止的 exited/created；请先单独停机，脚本不会替你停容器" ;;
  esac
}

# 不依赖 fuser/lsof：直接扫描 /proc 的 fd 链接，能看到宿主上的服务进程。
# 注意：/proc 只能证明可见进程没有持有；它不能替代“停服窗口 + 禁止并发启动”的
# 操作约束，也不能证明检查结束后不会再有进程打开数据库。
check_no_db_holders() {
  local fd target pids="" unreadable="" hidden=0 pid owner me procroot
  me=$(id -u)
  procroot=${PROC_ROOT:-/proc}
  for fd in "$procroot"/[0-9]*/fd/*; do
    # 先取链接再判断存在性：进程持有已被 unlink 的数据库时，fd 链接目标
    # 带 " (deleted)"，不能因为 [ -e ] 失败就把它当成没有持有。
    target=$(readlink "$fd" 2>/dev/null) || {
      pid=${fd#"$procroot"/}; pid=${pid%%/*}
      # 进程在扫描间隙退出时 fd 会消失；这不是“持有数据库”，不能误杀。
      [ -d "$procroot/$pid" ] || continue
      target=$(readlink "$fd" 2>/dev/null) || {
        [ -d "$procroot/$pid" ] || continue
        owner=$(stat -c %u "$procroot/$pid" 2>/dev/null || echo '?')
        # 扫描期间进程可能刚退出/变成僵尸，或某个 fd 刚关闭；再确认一次。
        [ -d "$procroot/$pid" ] || continue
        [ -e "$fd" ] || continue
        [ "$(awk '{print $3}' "$procroot/$pid/stat" 2>/dev/null)" = "Z" ] && continue
        if [ "$owner" = "$me" ]; then
          unreadable="$unreadable $pid($(cat "$procroot/$pid/comm" 2>/dev/null || echo '?'))"
        else
          hidden=$((hidden+1))
        fi
        continue
      }
    }
    case "$target" in
      "$DB"|"$DB-wal"|"$DB-shm"|"$DB-journal"|\
      "$DB (deleted)"|"$DB-wal (deleted)"|"$DB-shm (deleted)"|"$DB-journal (deleted)")
        pids="$pids ${fd#"$procroot"/}"
        ;;
    esac
  done
  # 目录本身不可读时，上面的通配符看不到任何 fd；这也是“状态不可确认”，
  # 不能当作 0 个持有者。其他 UID 的进程尤其如此。
  for p in "$procroot"/[0-9]*; do
    [ -d "$p" ] || continue
    owner=$(stat -c %u "$p" 2>/dev/null || echo '?')
    [ "$owner" = "$me" ] && continue
    if ! ls "$p/fd" >/dev/null 2>&1; then
      hidden=$((hidden+1))
    fi
  done
  [ -z "$pids" ] || die "仍有进程持有正式库/WAL/SHM/journal:$pids；先停服务并确认进程退出"
  # 同 UID 的 fd 本应可读；读不到说明权限/命名空间被限制，不能静默放行。
  [ -z "$unreadable" ] || die "同 UID 进程的数据库 fd 无法读取（pid:$unreadable）；请以宿主同 UID/root 运行"
  # 其他 UID 的 fd 不可读时也无法证明它没有持有数据库。带着未知写入者
  # 切换就是把旧库交给一个看不见的进程，必须 fail-closed。
  [ "$hidden" -eq 0 ] ||
    die "有 $hidden 个其他 UID 的进程 fd 不可读，无法确认数据库持有者；请以宿主 root/足够权限运行后再升级"
}

# checkpoint 并关闭连接后，WAL/SHM 必须收敛；残留的 sidecar 不能被静默忽略。
check_no_sidecar() { # check_no_sidecar <db-path> <stage>
  [ ! -e "$1-wal" ] || die "$2 仍残留 -wal: $1-wal"
  [ ! -e "$1-shm" ] || die "$2 仍残留 -shm: $1-shm"
}

mval() { # mval <key>：manifest 是 key=value，值可含等号
  awk -v k="$1" 'index($0, k"=")==1 { print substr($0, length(k)+2); exit }' "$MANIFEST"
}

# 备份单元一致性：文件名、大小、哈希、sidecar 状态都必须与 manifest 一致。
# 这个函数只读，不修改任何文件；rename 前和回滚前都调用。
check_manifest() {
  [ -f "$MANIFEST" ] || die "备份 manifest 缺失，拒绝继续: $MANIFEST"
  [ "$(mval manifest_version)" = "2" ] || die "manifest 版本不是 2，拒绝继续"
  [ -n "$(mval backup_id)" ] || die "manifest 缺少 backup_id"
  [ "$(mval db_file)" = "$DB_NAME" ] ||
    die "manifest DB 文件名不匹配：$(mval db_file) != $DB_NAME"
  [ -f "$PRE" ] || die "manifest 指向的备份库不存在: $PRE"
  [ "$(stat -c %s "$PRE")" = "$(mval db_size)" ] || die "备份 DB 大小与 manifest 不符"
  [ "$(sha "$PRE")" = "$(mval db_sha)" ] || die "备份 DB SHA-256 与 manifest 不符"
  [ "$(mval key_file)" = "$KEY_NAME" ] ||
    die "manifest key 文件名不匹配：$(mval key_file) != $KEY_NAME"
  [ -f "$KEY" ] || die "manifest 存在但 secret.key 缺失，拒绝继续"
  [ "$(stat -c %s "$KEY")" = "$(mval key_size)" ] || die "secret.key 大小与 manifest 不符"
  [ "$(sha "$KEY")" = "$(mval key_sha)" ] || die "secret.key 与升级前 manifest 不符"

  local state file size hash
  state=$(mval wal_state)
  case "$state" in
    absent)
      [ ! -e "$PRE-wal" ] || die "manifest 声明无 WAL，但备份 sidecar 存在: $PRE-wal"
      [ "$(mval wal_file)" = "absent" ] || die "WAL 状态字段不一致"
      [ "$(mval wal_size)" = "absent" ] || die "WAL absent 时 wal_size 必须是 absent"
      [ "$(mval wal_sha)" = "absent" ] || die "WAL absent 时 wal_sha 必须是 absent"
      ;;
    present)
      file=$(mval wal_file); size=$(mval wal_size); hash=$(mval wal_sha)
      [ "$file" = "$PRE_NAME-wal" ] || die "manifest WAL 文件名不匹配: $file"
      [ -f "$PRE-wal" ] || die "manifest 声明有 WAL，但备份 sidecar 缺失: $PRE-wal"
      [ "$(stat -c %s "$PRE-wal")" = "$size" ] || die "备份 WAL 大小与 manifest 不符"
      [ "$(sha "$PRE-wal")" = "$hash" ] || die "备份 WAL SHA-256 与 manifest 不符"
      ;;
    *) die "manifest WAL 状态非法: $state" ;;
  esac

  state=$(mval shm_state)
  case "$state" in
    absent)
      [ ! -e "$PRE-shm" ] || die "manifest 声明无 SHM，但备份 sidecar 存在: $PRE-shm"
      [ "$(mval shm_file)" = "absent" ] || die "SHM 状态字段不一致"
      [ "$(mval shm_size)" = "absent" ] || die "SHM absent 时 shm_size 必须是 absent"
      [ "$(mval shm_sha)" = "absent" ] || die "SHM absent 时 shm_sha 必须是 absent"
      ;;
    present)
      file=$(mval shm_file); size=$(mval shm_size); hash=$(mval shm_sha)
      [ "$file" = "$PRE_NAME-shm" ] || die "manifest SHM 文件名不匹配: $file"
      [ -f "$PRE-shm" ] || die "manifest 声明有 SHM，但备份 sidecar 缺失: $PRE-shm"
      [ "$(stat -c %s "$PRE-shm")" = "$size" ] || die "备份 SHM 大小与 manifest 不符"
      [ "$(sha "$PRE-shm")" = "$hash" ] || die "备份 SHM SHA-256 与 manifest 不符"
      ;;
    *) die "manifest SHM 状态非法: $state" ;;
  esac

  [ -n "$(mval tool_sha)" ] || die "manifest 缺少 tool_sha"
  [ -n "$(mval baseline)" ] || die "manifest 缺少 baseline"
  [ -n "$(mval target_schema_hash)" ] || die "manifest 缺少 target_schema_hash"
  [ -n "$(mval created)" ] || die "manifest 缺少 created"
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
    --expect-image-id) EXPECT_IMAGE_ID=${2:-}; shift 2 ;;
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

# 先拿数据目录锁，再扫描 FD：服务、restore.sh 和离线迁移共用同一把锁。
# 锁必须早于持有者扫描，否则扫描结束到 flock 之间可能有人打开数据库。
exec 8>"$DATA/.mirror-lens.lock"
chmod 600 "$DATA/.mirror-lens.lock" 2>/dev/null || true
flock -n 8 || die "另一个迁移进程正在操作 $DATA（.mirror-lens.lock 被占用）"

# 扫描结果只对“拿到锁之后”的时刻负责；切换前还会再扫描一次。
check_no_db_holders

check_work_dir() {
  [ -n "$WORK_SAVED" ] || return 0
  case "$WORK_SAVED" in
    "$DATA"/.db-upgrade-*) ;;
    *) die "journal work_dir 不在数据目录的 .db-upgrade-* 下，拒绝删除: $WORK_SAVED" ;;
  esac
  case "$WORK_SAVED" in
    *..*) die "journal work_dir 含 ..，拒绝删除: $WORK_SAVED" ;;
  esac
}

remove_work() {
  [ -n "$WORK_SAVED" ] || return 0
  check_work_dir
  rm -rf "$WORK_SAVED"
}

recover() {
  [ -f "$JOURNAL_PATH" ] || { log "没有未完成的升级，无需恢复"; return 0; }
  STATE=$(field state)
  WORK_SAVED=$(field work_dir)
  case "$STATE" in
    staging|migrating)
      [ -n "$WORK_SAVED" ] || die "journal 处于 $STATE 但缺少 work_dir，拒绝恢复"
      check_work_dir
      [ -f "$DB" ] || die "journal 处于 $STATE 但正式库不存在，拒绝清理 staging"
      if "$TOOL" verify --db "$DB" >/dev/null 2>&1; then
        die "journal 处于 $STATE 但正式库已是 Canonical，拒绝自动清理"
      fi
      log "迁移前/迁移中崩溃，正式库未改动；清理 staging"
      rm -f "$JOURNAL_PATH"
      remove_work
      return 0
      ;;
    backup)
      # 备份阶段中断：正式库还没有被移动，直接清掉半成品备份后重跑即可。
      if [ -f "$DB" ] && ! "$TOOL" verify --db "$DB" >/dev/null 2>&1; then
        log "备份阶段中断，正式库未改动；清理半成品备份"
        check_work_dir
        rm -f "$PRE" "$PRE-wal" "$PRE-shm" "$JOURNAL_PATH" "$KEY_PRE" "$MANIFEST"
        remove_work
        return 0
      fi
      die "journal 处于 backup 状态但正式库已是 Canonical，拒绝自动处理"
      ;;
    switching)
      ;;
    *)
      die "journal state 非法: ${STATE:-<empty>}；拒绝按未知状态恢复"
      ;;
  esac
  PRE_SAVED=$(field backup_db)
  PRE_SHA=$(field backup_sha)
  [ -n "$PRE_SAVED" ] && [ -n "$PRE_SHA" ] || die "journal 不完整，拒绝自动恢复"
  check_work_dir
  if [ -f "$DB" ] && "$TOOL" verify --db "$DB" >/dev/null 2>&1; then
    log "正式库已是完整 Canonical 库，清除 journal"
    rm -f "$JOURNAL_PATH"
    remove_work
    return 0
  fi
  [ "$PRE_SAVED" = "$PRE" ] || die "journal 备份路径不是预期备份单元: $PRE_SAVED"
  [ -f "$PRE_SAVED" ] || die "备份库不存在: $PRE_SAVED"
  # 回滚前再次校验完整备份单元：DB、key、manifest、sidecar 必须互相一致。
  check_manifest
  [ "$(sha "$PRE_SAVED")" = "$PRE_SHA" ] || die "备份库 SHA-256 与 journal 不符，拒绝恢复"
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
  remove_work
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
# 先落 journal 再创建 staging：SIGKILL 发生在迁移前也能让 --recover 找到并清理
# 残留目录，而不是留下一个没有 journal 的 .db-upgrade-*。
printf '{"state":"staging","work_dir":"%s"}\n' "$WORK" >"$JOURNAL_PATH"
fsync_path "$JOURNAL_PATH"
mkdir -p "$WORK"
trap 'rm -rf "$WORK" 2>/dev/null || true' EXIT

log "复制正式库到 staging（正式库只读）"
cp --reflink=auto "$DB" "$WORK/old.db"
[ -f "$DB-wal" ] && cp --reflink=auto "$DB-wal" "$WORK/old.db-wal"
[ -f "$DB-shm" ] && cp --reflink=auto "$DB-shm" "$WORK/old.db-shm"
fsync_path "$WORK/old.db"
log "在 staging 上 checkpoint WAL"
"$TOOL" checkpoint --db "$WORK/old.db"
check_no_sidecar "$WORK/old.db" "staging 旧库 checkpoint 后"
log "迁移到 Canonical"
"$TOOL" migrate --in "$WORK/old.db" --out "$WORK/new.db" --baseline "$BASELINE"
# 新库也必须 checkpoint 并关闭后再切换；否则 new.db-wal 会被遗留/删除，导致丢事务。
"$TOOL" checkpoint --db "$WORK/new.db"
check_no_sidecar "$WORK/new.db" "新库 checkpoint 后"
VERIFY_OUT=$("$TOOL" verify --db "$WORK/new.db")
check_no_sidecar "$WORK/new.db" "新库 verify 后"
echo "$VERIFY_OUT"
TARGET_HASH=$(printf '%s\n' "$VERIFY_OUT" | sed -n 's/.*hash=//p')
[ -n "$TARGET_HASH" ] || die "无法从 verify 输出解析 target schema hash"
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
[ -f "$PRE-wal" ] && fsync_path "$PRE-wal"
[ -f "$PRE-shm" ] && fsync_path "$PRE-shm"
sync
PRE_SHA=$(sha "$PRE")
PRE_SIZE=$(stat -c %s "$PRE")
# key 不参与切换；只额外留一份 600 的灾难恢复副本，并记录它和 DB 的哈希。
cp --reflink=auto "$KEY" "$KEY_PRE"
chmod 600 "$KEY_PRE"
fsync_path "$KEY_PRE"
KEY_SHA=$(sha "$KEY")
KEY_SIZE=$(stat -c %s "$KEY")

WAL_STATE=absent; WAL_FILE=absent; WAL_SIZE=absent; WAL_SHA=absent
if [ -f "$PRE-wal" ]; then
  WAL_STATE=present; WAL_FILE=$PRE_NAME-wal
  WAL_SIZE=$(stat -c %s "$PRE-wal"); WAL_SHA=$(sha "$PRE-wal")
fi
SHM_STATE=absent; SHM_FILE=absent; SHM_SIZE=absent; SHM_SHA=absent
if [ -f "$PRE-shm" ]; then
  SHM_STATE=present; SHM_FILE=$PRE_NAME-shm
  SHM_SIZE=$(stat -c %s "$PRE-shm"); SHM_SHA=$(sha "$PRE-shm")
fi

MANIFEST_TMP="$MANIFEST.tmp-$$"
printf 'manifest_version=2\nbackup_id=%s-%s\ndb_file=%s\ndb_size=%s\ndb_sha=%s\nkey_file=%s\nkey_size=%s\nkey_sha=%s\nwal_state=%s\nwal_file=%s\nwal_size=%s\nwal_sha=%s\nshm_state=%s\nshm_file=%s\nshm_size=%s\nshm_sha=%s\ntool_sha=%s\nbaseline=%s\ntarget_schema_hash=%s\ncreated=%s\n' \
  "$TS" "${PRE_SHA:0:12}" "$DB_NAME" "$PRE_SIZE" "$PRE_SHA" \
  "$KEY_NAME" "$KEY_SIZE" "$KEY_SHA" \
  "$WAL_STATE" "$WAL_FILE" "$WAL_SIZE" "$WAL_SHA" \
  "$SHM_STATE" "$SHM_FILE" "$SHM_SIZE" "$SHM_SHA" \
  "$(sha "$TOOL")" "$BASELINE" "$TARGET_HASH" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" >"$MANIFEST_TMP"
chmod 600 "$MANIFEST_TMP"
fsync_path "$MANIFEST_TMP"
mv "$MANIFEST_TMP" "$MANIFEST"
fsync_path "$MANIFEST"
sync
crashpoint after_backup

# 备份单元必须自洽后才能把 journal 推进到 switching。
log "校验备份单元 manifest"
check_manifest

# 备份已完成；journal 切换到 switching 后才能动正式库，并 fsync 目录项。
printf '{"state":"switching","backup_db":"%s","backup_sha":"%s","work_dir":"%s"}\n' \
  "$PRE" "$PRE_SHA" "$WORK" >"$JOURNAL_PATH"
fsync_path "$JOURNAL_PATH"
crashpoint after_journal

# 真正 rename 前再校验一次，防止备份在 journal 写入后被改动。
check_manifest
# 锁已持有，但仍要在动正式库前再确认一次：任何在前期扫描后打开数据库的
# 进程都必须在切换前退出，不能把未知写入者带进切换窗口。
check_no_db_holders
# 正式库的 -wal/-shm 属于旧库。先把 sidecar 移到 work（不是删除），
# 再由一次 rename 原子替换正式库；任一步崩溃都由 --recover 依据完整备份恢复。
if [ -f "$DB-wal" ]; then mv -f "$DB-wal" "$WORK/old-wal"; fi
if [ -f "$DB-shm" ]; then mv -f "$DB-shm" "$WORK/old-shm"; fi
crashpoint after_remove_old
mv -f "$WORK/new.db" "$DB"
crashpoint after_rename
chmod 600 "$DB"
fsync_path "$DB"
fsync_path "$DATA"
sync
crashpoint after_switch
rm -f "$WORK/old-wal" "$WORK/old-shm"

"$TOOL" verify --db "$DB" || die "切换后校验失败；journal 保留，请运行 --recover"
rm -f "$JOURNAL_PATH"
rm -rf "$WORK"
trap - EXIT
log "完成：$DB -> Canonical 3.5.6；回滚副本 $PRE（含 -wal/-shm，如存在）"
log "灾难恢复材料：$KEY_PRE（600）、$MANIFEST；secret.key 本体未改动"
