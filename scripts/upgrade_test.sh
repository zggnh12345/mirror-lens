#!/usr/bin/env bash
# upgrade-db.sh 的故障注入测试。全部在 mktemp 目录，使用最小旧库，
# 不读取生产数据；需要先构建 db-migrate：
#   go build -o db-migrate ./cmd/db-migrate
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
TOOL=${TOOL:-$HERE/../db-migrate}
UP="$HERE/upgrade-db.sh"
PASS=0; FAIL=0
ok() { PASS=$((PASS+1)); echo "ok   - $1"; }
bad() { FAIL=$((FAIL+1)); echo "FAIL - $1"; }

[ -x "$TOOL" ] || { echo "缺少 db-migrate: $TOOL" >&2; exit 2; }

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

# 1. 正常升级
r=$(mktemp -d); fixture "$r"
if "$UP" --data "$r" --tool "$TOOL" --baseline 3.4.6 >"$r/log" 2>&1 &&
   "$TOOL" verify --db "$r/mirror-lens.db" >/dev/null &&
   [ "$(count_sources "$r/mirror-lens.db")" = 1 ] &&
   [ -f "$r/mirror-lens.db.pre-3.5.5" ] && [ "$(cat "$r/secret.key")" = TEST-KEY ] &&
   [ ! -f "$r/.db-upgrade-journal.json" ]; then
  ok "正常升级 + 回滚副本 + key 不动"
else
  bad "正常升级失败: $(tail -5 "$r/log")"
fi
rm -rf "$r"

# 2. 故障点：恢复后必须回到完整旧库或完整新库，journal 必须清掉
for point in after_backup after_journal after_remove_old after_switch; do
  r=$(mktemp -d); fixture "$r"
  DB_UPGRADE_CRASH=$point "$UP" --data "$r" --tool "$TOOL" --baseline 3.4.6 >"$r/log" 2>&1
  rc=$?
  if [ "$rc" -ne 137 ]; then
    bad "$point 未触发 SIGKILL (rc=$rc)"
  elif ! "$UP" --recover --data "$r" --tool "$TOOL" >"$r/recover.log" 2>&1; then
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
if "$UP" --recover --data "$r" --tool "$TOOL" >/dev/null 2>&1; then
  ok "无 journal 时 recover 幂等"
else
  bad "无 journal 时 recover 失败"
fi
rm -rf "$r"

echo "----"; echo "PASS=$PASS FAIL=$FAIL"
[ "$FAIL" -eq 0 ]
