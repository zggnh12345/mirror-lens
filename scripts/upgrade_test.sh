#!/usr/bin/env bash
# upgrade-db.sh 的故障注入测试。全部在 mktemp 目录，使用真实 3.4.6 副本的
# 结构指纹（行数据会被裁到 1 条 sources，Schema 指纹不变）。不读取生产库本身。
#   REAL_346=/path/to/3.4.6.db TOOL=./db-migrate scripts/upgrade_test.sh
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
TOOL=${TOOL:-$HERE/../db-migrate}
REAL_346=${REAL_346:-/opt/data/cache/scratch/ml-355/in/3.4.6.db}
UP="$HERE/upgrade-db.sh"
PASS=0; FAIL=0; SKIP=0
ok() { PASS=$((PASS+1)); echo "ok   - $1"; }
bad() { FAIL=$((FAIL+1)); echo "FAIL - $1"; }
skip() { SKIP=$((SKIP+1)); echo "skip - $1"; }
[ -f "$REAL_346" ] || { echo "skip - 缺少真实 3.4.6 副本: $REAL_346（设置 REAL_346 后重跑）"; exit 0; }

# 在已关闭的 fixture 上开启 WAL，提交一帧，再在连接仍打开时复制出 DB/WAL/SHM；
# 这样升级脚本看到的是“有 sidecar 的旧库”，而不是 checkpoint 后的单文件。
make_wal() {
  python3 - "$1/mirror-lens.db" <<'PY'
import os, shutil, sqlite3, sys
db=sys.argv[1]
c=sqlite3.connect(db)
c.execute('PRAGMA journal_mode=WAL')
c.execute('PRAGMA wal_autocheckpoint=0')
c.execute("INSERT INTO sources(base_url,name,docker_cap,github_cap,ghcr_cap,status,enabled,created_at) VALUES('https://b.example','b',1,-1,-1,'usable',1,2)")
c.commit()
tmp=db+'.seed'
shutil.copyfile(db,tmp)
for ext in ('-wal','-shm'):
    if os.path.exists(db+ext):
        shutil.copyfile(db+ext,tmp+ext)
c.close()
os.replace(tmp,db)
for ext in ('-wal','-shm'):
    if os.path.exists(tmp+ext):
        os.replace(tmp+ext,db+ext)
PY
}

[ -x "$TOOL" ] || { echo "缺少 db-migrate: $TOOL" >&2; exit 2; }

# 假 docker 只提供升级脚本需要的 inspect 字段；它不启动/停止任何容器。
FAKEBIN=$(mktemp -d)
trap 'rm -rf "$FAKEBIN"' EXIT
cat >"$FAKEBIN/docker" <<'FAKE'
#!/usr/bin/env bash
[ "${FAKE_MISSING:-}" = "1" ] && exit 1
case "$*" in
  *'{{.Name}}'*)          echo "${FAKE_NAME:-/test}" ;;
  *'{{.Config.Image}}'*)  echo "${FAKE_IMAGE:-test:1}" ;;
  *'{{.State.Status}}'*)  echo "${FAKE_STATUS:-exited}" ;;
  *'{{.State.Running}}'*) echo "${FAKE_RUNNING:-false}" ;;
  *'{{.Image}}'*)         echo "${FAKE_IMAGE_ID:-sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa}" ;;
  *'{{.Id}}'*)            echo "${FAKE_IMAGE_ID:-sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa}" ;;
  *) exit 1 ;;
esac
FAKE
chmod +x "$FAKEBIN/docker"
run_up() { PATH="$FAKEBIN:$PATH" "$UP" --container test --expect-image test:1 --expect-image-id "${FAKE_IMAGE_ID:-sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa}" "$@"; }

fixture() {
  local d=$1
  mkdir -p "$d"
  # 使用真实 3.4.6 Schema；只裁业务行，不动表/列/索引，所以输入指纹保持冻结值。
  cp "$REAL_346" "$d/mirror-lens.db"
  python3 - "$d/mirror-lens.db" <<'PY'
import sqlite3, sys
c = sqlite3.connect(sys.argv[1])
tables = [r[0] for r in c.execute("select name from sqlite_master where type='table' and name not like 'sqlite_%'")]
for t in tables:
    if t == 'sources':
        c.execute('DELETE FROM sources WHERE id NOT IN (SELECT id FROM sources ORDER BY id LIMIT 1)')
    elif t != 'sqlite_sequence':
        c.execute('DELETE FROM "' + t.replace('"','""') + '"')
c.commit()
c.close()
PY
  printf 'TEST-KEY' >"$d/secret.key"; chmod 600 "$d/mirror-lens.db" "$d/secret.key"
}

count_sources() {
  python3 - "$1" <<'PY'
import sqlite3,sys
print(sqlite3.connect(sys.argv[1]).execute('select count(*) from sources').fetchone()[0])
PY
}

untouched() { # 旧库仍可读、未被升级、没有留下 journal
  [ "$(count_sources "$1/mirror-lens.db")" = 1 ] &&
    ! "$TOOL" verify --db "$1/mirror-lens.db" >/dev/null 2>&1 &&
    [ ! -f "$1/.db-upgrade-journal.json" ]
}

no_staging() {
  [ -z "$(find "$1" -maxdepth 1 -name '.db-upgrade-*' -print -quit 2>/dev/null)" ]
}

# 0. 停服/身份门禁：运行中、restarting、paused、缺失、身份不符、镜像不符都必须拒绝
for kind in running restarting paused missing identity image; do
  r=$(mktemp -d); fixture "$r"
  case $kind in
    running)    FAKE_STATUS=running run_up --data "$r" --tool "$TOOL" --baseline 3.4.6 >"$r/log" 2>&1 ;;
    restarting) FAKE_STATUS=restarting run_up --data "$r" --tool "$TOOL" --baseline 3.4.6 >"$r/log" 2>&1 ;;
    paused)     FAKE_STATUS=paused run_up --data "$r" --tool "$TOOL" --baseline 3.4.6 >"$r/log" 2>&1 ;;
    missing)    FAKE_MISSING=1 run_up --data "$r" --tool "$TOOL" --baseline 3.4.6 >"$r/log" 2>&1 ;;
    identity)   FAKE_NAME=/other run_up --data "$r" --tool "$TOOL" --baseline 3.4.6 >"$r/log" 2>&1 ;;
    image)      FAKE_IMAGE=other run_up --data "$r" --tool "$TOOL" --baseline 3.4.6 >"$r/log" 2>&1 ;;
  esac
  if [ $? -eq 0 ]; then
    bad "$kind 状态应被停服门禁拒绝却成功"
  elif untouched "$r"; then
    ok "$kind 状态被拒绝且正式库未动"
  else
    bad "$kind 被拒绝但正式库/journal 已改变: $(tail -3 "$r/log")"
  fi
  rm -rf "$r"
done

# 0a. 不带 --expect-image-id 也必须拒绝，不能只凭容器名/可变 tag 通过身份校验
r=$(mktemp -d); fixture "$r"
if "$UP" --container test --data "$r" --tool "$TOOL" --baseline 3.4.6 >"$r/log" 2>&1; then
  bad "缺少 --expect-image-id 仍执行了升级"
elif grep -q '缺少 --expect-image-id' "$r/log" && untouched "$r"; then
  ok "缺少 --expect-image-id 被拒绝"
else
  bad "缺少 --expect-image-id 的拒绝信息/状态不对: $(tail -3 "$r/log")"
fi
rm -rf "$r"

# 0b. 没有 --container 不允许执行；不能留下绕过口
r=$(mktemp -d); fixture "$r"
if "$UP" --data "$r" --tool "$TOOL" --baseline 3.4.6 >"$r/log" 2>&1; then
  bad "缺少 --container 仍执行了升级"
elif grep -q '缺少 --container' "$r/log" && untouched "$r"; then
  ok "缺少 --container 被拒绝"
else
  bad "缺少 --container 的拒绝信息/状态不对: $(tail -3 "$r/log")"
fi
rm -rf "$r"

# 0b2. 不可变镜像 ID 不匹配：即使容器名/标签看起来正确也必须拒绝
r=$(mktemp -d); fixture "$r"
if FAKE_IMAGE_ID=sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb \
  PATH="$FAKEBIN:$PATH" "$UP" --container test --data "$r" --tool "$TOOL" --baseline 3.4.6 \
  --expect-image-id sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc >"$r/log" 2>&1; then
  bad "镜像 ID 不匹配仍执行了升级"
elif grep -q '镜像' "$r/log" && untouched "$r"; then
  ok "镜像 ID 不匹配被拒绝"
else
  bad "镜像 ID 不匹配的拒绝信息/状态不对: $(tail -3 "$r/log")"
fi
rm -rf "$r"

# 0c. 仍有进程持有正式库：必须拒绝
r=$(mktemp -d); fixture "$r"
python3 - "$r/mirror-lens.db" <<'PY' >"$r/holder.log" 2>&1 &
import sqlite3,sys,time
c=sqlite3.connect(sys.argv[1])
c.execute('select count(*) from sources').fetchone()
print('READY', flush=True)
time.sleep(10)
PY
hp=$!
for _ in $(seq 1 100); do grep -q READY "$r/holder.log" 2>/dev/null && break; sleep 0.05; done
if run_up --data "$r" --tool "$TOOL" --baseline 3.4.6 >"$r/log" 2>&1; then
  bad "有进程持有数据库时仍执行了升级"
elif grep -q '仍有进程持有' "$r/log" && untouched "$r"; then
  ok "数据库仍被持有：拒绝升级"
else
  bad "持有检查拒绝信息/状态不对: $(tail -3 "$r/log")"
fi
kill "$hp" 2>/dev/null; wait "$hp" 2>/dev/null || true
rm -rf "$r"

# 0d. 库被 unlink 但进程仍持有 fd：readlink 目标带 " (deleted)"，也必须拒绝
r=$(mktemp -d); fixture "$r"
python3 - "$r/mirror-lens.db" <<'PY' >"$r/holder.log" 2>&1 &
import os,sqlite3,sys,time
c=sqlite3.connect(sys.argv[1])
c.execute('select count(*) from sources').fetchone()
os.unlink(sys.argv[1])
print('READY', flush=True)
time.sleep(10)
PY
hp=$!
for _ in $(seq 1 100); do grep -q READY "$r/holder.log" 2>/dev/null && break; sleep 0.05; done
if run_up --data "$r" --tool "$TOOL" --baseline 3.4.6 >"$r/log" 2>&1; then
  bad "库已 unlink 但 fd 仍被持有时执行了升级"
elif grep -q '仍有进程持有' "$r/log" && [ ! -f "$r/.db-upgrade-journal.json" ]; then
  ok "已删除但仍被持有：拒绝升级"
else
  bad "deleted fd 检查拒绝信息/状态不对: $(tail -3 "$r/log")"
fi
kill "$hp" 2>/dev/null; wait "$hp" 2>/dev/null || true
rm -rf "$r"

# 1. 正常升级
r=$(mktemp -d); fixture "$r"
if run_up --data "$r" --tool "$TOOL" --baseline 3.4.6 >"$r/log" 2>&1 &&
   "$TOOL" verify --db "$r/mirror-lens.db" >/dev/null &&
   [ "$(count_sources "$r/mirror-lens.db")" = 1 ] &&
   [ -f "$r/mirror-lens.db.pre-3.5.6" ] && [ "$(cat "$r/secret.key")" = TEST-KEY ] &&
   [ -f "$r/mirror-lens.db.pre-3.5.6.manifest" ] &&
   grep -q '^manifest_version=2$' "$r/mirror-lens.db.pre-3.5.6.manifest" &&
   grep -q '^db_size=' "$r/mirror-lens.db.pre-3.5.6.manifest" &&
   grep -q '^wal_state=absent$' "$r/mirror-lens.db.pre-3.5.6.manifest" &&
   grep -q '^shm_state=absent$' "$r/mirror-lens.db.pre-3.5.6.manifest" &&
   grep -q '^target_schema_hash=' "$r/mirror-lens.db.pre-3.5.6.manifest" &&
   [ ! -e "$r/mirror-lens.db.pre-3.5.6-wal" ] && [ ! -e "$r/mirror-lens.db.pre-3.5.6-shm" ] &&
   [ ! -e "$r/mirror-lens.db-wal" ] && [ ! -e "$r/mirror-lens.db-shm" ] &&
   [ "$(stat -c %a "$r/mirror-lens.db.pre-3.5.6")" = 600 ] &&
   no_staging "$r" &&
   [ ! -f "$r/.db-upgrade-journal.json" ]; then
  ok "正常升级 + 完整 manifest + 回滚副本 + key 不动"
else
  bad "正常升级失败: $(tail -5 "$r/log")"
fi
rm -rf "$r"

# 2b. 备份保留 WAL/SHM 时：manifest 记录 present，回滚副本带 sidecar；篡改 sidecar 必须拒绝
r=$(mktemp -d); fixture "$r"; make_wal "$r"
if [ -f "$r/mirror-lens.db-wal" ]; then
  if run_up --data "$r" --tool "$TOOL" --baseline 3.4.6 >"$r/log" 2>&1 &&
     grep -q '^wal_state=present$' "$r/mirror-lens.db.pre-3.5.6.manifest" &&
     [ -f "$r/mirror-lens.db.pre-3.5.6-wal" ] &&
     "$TOOL" verify --db "$r/mirror-lens.db" >/dev/null 2>&1; then
    ok "WAL sidecar manifest=present、回滚副本保留 WAL"
  else
    bad "WAL sidecar 正常升级失败: $(tail -4 "$r/log")"
  fi
else
  skip "make_wal 未生成 WAL sidecar"
fi
rm -rf "$r"

r=$(mktemp -d); fixture "$r"; make_wal "$r"
DB_UPGRADE_CRASH=after_journal run_up --data "$r" --tool "$TOOL" --baseline 3.4.6 >"$r/log" 2>&1
if [ -f "$r/mirror-lens.db.pre-3.5.6-wal" ]; then
  printf 'X' >>"$r/mirror-lens.db.pre-3.5.6-wal"
  if run_up --recover --data "$r" --tool "$TOOL" >"$r/recover.log" 2>&1; then
    bad "WAL sidecar 被篡改仍恢复成功"
  elif [ -f "$r/.db-upgrade-journal.json" ]; then
    ok "WAL sidecar 篡改被拒绝且 journal 保留"
  else
    bad "WAL sidecar 篡改被拒绝但 journal 丢失"
  fi
else
  skip "崩溃后没有 WAL 回滚副本，无法测 WAL 篡改"
fi
rm -rf "$r"

# 2. 备份单元篡改/缺失/截断/文件名不匹配：必须拒绝恢复且保留 journal
for kind in db-tamper db-truncate manifest-missing db-filename; do
  r=$(mktemp -d); fixture "$r"
  DB_UPGRADE_CRASH=after_journal run_up --data "$r" --tool "$TOOL" --baseline 3.4.6 >"$r/log" 2>&1
  case $kind in
    db-tamper)       printf 'X' >>"$r/mirror-lens.db.pre-3.5.6" ;;
    db-truncate)     truncate -s 3 "$r/mirror-lens.db.pre-3.5.6" ;;
    manifest-missing) rm -f "$r/mirror-lens.db.pre-3.5.6.manifest" ;;
    db-filename)     sed -i 's/^db_file=.*/db_file=wrong.db/' "$r/mirror-lens.db.pre-3.5.6.manifest" ;;
  esac
  if run_up --recover --data "$r" --tool "$TOOL" >"$r/recover.log" 2>&1; then
    bad "$kind 应被拒绝却恢复成功"
  elif [ "$(count_sources "$r/mirror-lens.db")" = 1 ] &&
       ! "$TOOL" verify --db "$r/mirror-lens.db" >/dev/null 2>&1 &&
       [ -f "$r/.db-upgrade-journal.json" ]; then
    ok "$kind 被拒绝且正式库未坏、journal 保留"
  else
    bad "$kind 被拒绝但状态不对: $(tail -3 "$r/recover.log")"
  fi
  rm -rf "$r"
done

# 2a. manifest 声明 absent 时必须连 size/hash 一起 absent，不能接受虚构哈希
r=$(mktemp -d); fixture "$r"
DB_UPGRADE_CRASH=after_journal run_up --data "$r" --tool "$TOOL" --baseline 3.4.6 >"$r/log" 2>&1
sed -i 's/^wal_size=.*/wal_size=123/; s/^wal_sha=.*/wal_sha=deadbeef/' "$r/mirror-lens.db.pre-3.5.6.manifest"
if run_up --recover --data "$r" --tool "$TOOL" >"$r/recover.log" 2>&1; then
  bad "manifest absent 却带虚构 size/hash 仍恢复成功"
elif [ -f "$r/.db-upgrade-journal.json" ] && [ "$(count_sources "$r/mirror-lens.db")" = 1 ]; then
  ok "manifest absent 虚构 size/hash 被拒绝"
else
  bad "manifest absent 虚构字段被拒绝但状态不对: $(tail -3 "$r/recover.log")"
fi
rm -rf "$r"

# 2b. journal 的 work_dir 必须限制在数据目录的 .db-upgrade-* 下
r=$(mktemp -d); fixture "$r"
DB_UPGRADE_CRASH=after_journal run_up --data "$r" --tool "$TOOL" --baseline 3.4.6 >"$r/log" 2>&1
mkdir -p "$r/canary"; printf 'keep' >"$r/canary/keep"
python3 - "$r" <<'PY'
import json,sys
p=sys.argv[1]+'/.db-upgrade-journal.json'
j=json.load(open(p)); j['work_dir']=sys.argv[1]+'/canary'; json.dump(j,open(p,'w'))
PY
if run_up --recover --data "$r" --tool "$TOOL" >"$r/recover.log" 2>&1; then
  bad "恶意 work_dir 仍被接受"
elif [ -f "$r/canary/keep" ] && [ -f "$r/.db-upgrade-journal.json" ]; then
  ok "work_dir 越界被拒绝且 canary 保留"
else
  bad "work_dir 越界被拒绝但 canary/journal 状态不对"
fi
rm -rf "$r"

# 2c. 未知 journal state 必须 fail-closed，不能落入回滚分支
r=$(mktemp -d); fixture "$r"
DB_UPGRADE_CRASH=after_journal run_up --data "$r" --tool "$TOOL" --baseline 3.4.6 >"$r/log" 2>&1
python3 - "$r" <<'PY'
import json,sys
p=sys.argv[1]+'/.db-upgrade-journal.json'
j=json.load(open(p)); j['state']='garbage'; json.dump(j,open(p,'w'))
PY
if run_up --recover --data "$r" --tool "$TOOL" >"$r/recover.log" 2>&1; then
  bad "未知 journal state 仍恢复成功"
elif grep -q 'journal state 非法' "$r/recover.log" && [ -f "$r/.db-upgrade-journal.json" ]; then
  ok "未知 journal state 被拒绝且 journal 保留"
else
  bad "未知 journal state 的拒绝信息/状态不对: $(tail -3 "$r/recover.log")"
fi
rm -rf "$r"

# 3. 故障点：恢复后必须回到完整旧库或完整新库，journal 必须清掉
for point in after_migrate after_backup after_journal after_remove_old after_rename after_switch; do
  r=$(mktemp -d); fixture "$r"
  DB_UPGRADE_CRASH=$point run_up --data "$r" --tool "$TOOL" --baseline 3.4.6 >"$r/log" 2>&1
  rc=$?
  if [ "$rc" -ne 137 ]; then
    bad "$point 未触发 SIGKILL (rc=$rc)"
  elif ! run_up --recover --data "$r" --tool "$TOOL" >"$r/recover.log" 2>&1; then
    bad "$point 恢复失败: $(tail -3 "$r/recover.log")"
  elif [ -f "$r/.db-upgrade-journal.json" ] || [ "$(count_sources "$r/mirror-lens.db")" != 1 ]; then
    bad "$point 恢复后 journal/数据不完整"
  elif ! no_staging "$r"; then
    bad "$point 恢复后 staging 残留: $(find "$r" -maxdepth 1 -name '.db-upgrade-*' -print)"
  elif { [ "$point" = after_switch ] || [ "$point" = after_rename ]; } && ! "$TOOL" verify --db "$r/mirror-lens.db" >/dev/null 2>&1; then
    bad "$point 恢复后新库不可用"
  elif [ "$point" != after_switch ] && [ "$point" != after_rename ] && "$TOOL" verify --db "$r/mirror-lens.db" >/dev/null 2>&1; then
    bad "$point 恢复后应为旧库，却被当成 Canonical"
  else
    ok "$point 故障恢复"
  fi
  rm -rf "$r"
done

# 3. 不存在的 journal：recover 幂等
r=$(mktemp -d); fixture "$r"
if run_up --recover --data "$r" --tool "$TOOL" >/dev/null 2>&1; then
  ok "无 journal 时 recover 幂等"
else
  bad "无 journal 时 recover 失败"
fi
rm -rf "$r"

# 4. manifest 保护：升级期间 secret.key 被换掉时拒绝自动回滚
r=$(mktemp -d); fixture "$r"
DB_UPGRADE_CRASH=after_journal run_up --data "$r" --tool "$TOOL" --baseline 3.4.6 >"$r/log" 2>&1
printf 'OTHER-KEY' >"$r/secret.key"
if run_up --recover --data "$r" --tool "$TOOL" >"$r/recover.log" 2>&1; then
  bad "key 与 manifest 不符时仍恢复成功"
elif grep -Eq 'secret\.key .*manifest 不符' "$r/recover.log"; then
  ok "key 与 manifest 不符时拒绝回滚"
else
  bad "key 不符但错误不是 manifest 拒绝: $(tail -3 "$r/recover.log")"
fi
rm -rf "$r"

echo "----"; echo "PASS=$PASS FAIL=$FAIL SKIP=$SKIP"
[ "$FAIL" -eq 0 ]
