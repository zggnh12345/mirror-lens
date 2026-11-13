#!/usr/bin/env bash
# upgrade-db.sh 的故障注入测试。全部在 mktemp 目录，使用最小旧库，
# 不读取生产数据；需要先构建 db-migrate：
#   go build -o db-migrate ./cmd/db-migrate
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
TOOL=${TOOL:-$HERE/../db-migrate}
UP="$HERE/upgrade-db.sh"
PASS=0; FAIL=0; SKIP=0
ok() { PASS=$((PASS+1)); echo "ok   - $1"; }
bad() { FAIL=$((FAIL+1)); echo "FAIL - $1"; }
skip() { SKIP=$((SKIP+1)); echo "skip - $1"; }

# 在已关闭的 fixture 上开启 WAL，提交一帧，再在连接仍打开时复制出 DB/WAL/SHM；
# 这样升级脚本看到的是“有 sidecar 的旧库”，而不是 checkpoint 后的单文件。
make_wal() {
  python3 - "$1/mirror-lens.db" <<'PY'
import os, shutil, sqlite3, sys
db=sys.argv[1]
c=sqlite3.connect(db)
c.execute('PRAGMA journal_mode=WAL')
c.execute('PRAGMA wal_autocheckpoint=0')
c.execute("INSERT INTO sources(base_url,name,repo_host,docker_cap,github_cap,ghcr_cap,status,enabled,created_at) VALUES('https://b.example','b','docker.io',1,-1,-1,'usable',1,2)")
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

# 假 docker 只提供升级脚本需要的四个 inspect 字段；它不启动/停止任何容器。
FAKEBIN=$(mktemp -d)
trap 'rm -rf "$FAKEBIN"' EXIT
cat >"$FAKEBIN/docker" <<'FAKE'
#!/usr/bin/env bash
[ "${FAKE_MISSING:-}" = "1" ] && exit 1
case "$*" in
  *'{{.Name}}'*)          echo "${FAKE_NAME:-/test}" ;;
  *'{{.Config.Image}}'*)  echo "${FAKE_IMAGE:-test:1}" ;;
  *'{{.State.Running}}'*) echo "${FAKE_RUNNING:-false}" ;;
  *) exit 1 ;;
esac
FAKE
chmod +x "$FAKEBIN/docker"
run_up() { PATH="$FAKEBIN:$PATH" "$UP" --container test --expect-image test:1 "$@"; }

fixture() {
  local d=$1
  mkdir -p "$d"
  python3 - "$d/mirror-lens.db" <<'PY'
import sqlite3,sys
c=sqlite3.connect(sys.argv[1])
c.executescript('''
CREATE TABLE sources(id INTEGER PRIMARY KEY AUTOINCREMENT, base_url TEXT NOT NULL, name TEXT NOT NULL,
  repo_host TEXT NOT NULL DEFAULT '', registry_cap INTEGER NOT NULL DEFAULT -1,
  registry_path_mode TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT 'pending',
  enabled INTEGER NOT NULL DEFAULT 1, created_at INTEGER NOT NULL DEFAULT 0,
  docker_cap INTEGER NOT NULL DEFAULT -1, github_cap INTEGER NOT NULL DEFAULT -1,
  ghcr_cap INTEGER NOT NULL DEFAULT -1);
INSERT INTO sources(base_url,name,repo_host,docker_cap,github_cap,ghcr_cap,status,enabled,created_at)
VALUES('https://a.example','a','docker.io',1,-1,-1,'usable',1,1);
''')
c.commit(); c.close()
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

# 0. 停服/身份门禁：运行中、缺失、身份不符、镜像不符都必须拒绝
for kind in running missing identity image; do
  r=$(mktemp -d); fixture "$r"
  case $kind in
    running)  FAKE_RUNNING=true run_up --data "$r" --tool "$TOOL" --baseline 3.4.6 >"$r/log" 2>&1 ;;
    missing)  FAKE_MISSING=1 run_up --data "$r" --tool "$TOOL" --baseline 3.4.6 >"$r/log" 2>&1 ;;
    identity) FAKE_NAME=/other run_up --data "$r" --tool "$TOOL" --baseline 3.4.6 >"$r/log" 2>&1 ;;
    image)    FAKE_IMAGE=other run_up --data "$r" --tool "$TOOL" --baseline 3.4.6 >"$r/log" 2>&1 ;;
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

# 1. 正常升级
r=$(mktemp -d); fixture "$r"
if run_up --data "$r" --tool "$TOOL" --baseline 3.4.6 >"$r/log" 2>&1 &&
   "$TOOL" verify --db "$r/mirror-lens.db" >/dev/null &&
   [ "$(count_sources "$r/mirror-lens.db")" = 1 ] &&
   [ -f "$r/mirror-lens.db.pre-3.5.5" ] && [ "$(cat "$r/secret.key")" = TEST-KEY ] &&
   [ -f "$r/mirror-lens.db.pre-3.5.5.manifest" ] &&
   grep -q '^manifest_version=2$' "$r/mirror-lens.db.pre-3.5.5.manifest" &&
   grep -q '^db_size=' "$r/mirror-lens.db.pre-3.5.5.manifest" &&
   grep -q '^wal_state=absent$' "$r/mirror-lens.db.pre-3.5.5.manifest" &&
   grep -q '^shm_state=absent$' "$r/mirror-lens.db.pre-3.5.5.manifest" &&
   grep -q '^target_schema_hash=' "$r/mirror-lens.db.pre-3.5.5.manifest" &&
   [ ! -e "$r/mirror-lens.db.pre-3.5.5-wal" ] && [ ! -e "$r/mirror-lens.db.pre-3.5.5-shm" ] &&
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
     grep -q '^wal_state=present$' "$r/mirror-lens.db.pre-3.5.5.manifest" &&
     [ -f "$r/mirror-lens.db.pre-3.5.5-wal" ] &&
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
if [ -f "$r/mirror-lens.db.pre-3.5.5-wal" ]; then
  printf 'X' >>"$r/mirror-lens.db.pre-3.5.5-wal"
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
    db-tamper)       printf 'X' >>"$r/mirror-lens.db.pre-3.5.5" ;;
    db-truncate)     truncate -s 3 "$r/mirror-lens.db.pre-3.5.5" ;;
    manifest-missing) rm -f "$r/mirror-lens.db.pre-3.5.5.manifest" ;;
    db-filename)     sed -i 's/^db_file=.*/db_file=wrong.db/' "$r/mirror-lens.db.pre-3.5.5.manifest" ;;
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

# 2. 故障点：恢复后必须回到完整旧库或完整新库，journal 必须清掉
for point in after_migrate after_backup after_journal after_remove_old after_switch; do
  r=$(mktemp -d); fixture "$r"
  DB_UPGRADE_CRASH=$point run_up --data "$r" --tool "$TOOL" --baseline 3.4.6 >"$r/log" 2>&1
  rc=$?
  if [ "$rc" -ne 137 ]; then
    bad "$point 未触发 SIGKILL (rc=$rc)"
  elif ! run_up --recover --data "$r" --tool "$TOOL" >"$r/recover.log" 2>&1; then
    bad "$point 恢复失败: $(tail -3 "$r/recover.log")"
  elif [ -f "$r/.db-upgrade-journal.json" ] || [ "$(count_sources "$r/mirror-lens.db")" != 1 ]; then
    bad "$point 恢复后 journal/数据不完整"
  elif [ "$point" = after_switch ] && ! "$TOOL" verify --db "$r/mirror-lens.db" >/dev/null 2>&1; then
    bad "$point 恢复后新库不可用"
  elif [ "$point" != after_switch ] && "$TOOL" verify --db "$r/mirror-lens.db" >/dev/null 2>&1; then
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
