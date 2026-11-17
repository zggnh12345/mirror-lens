#!/usr/bin/env bash
# rollback-3.5.15.sh 的本地测试：只操作 mktemp 目录。
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
SCRIPT="$HERE/rollback-3.5.15.sh"
PASS=0; FAIL=0
ok(){ PASS=$((PASS+1)); echo "ok   - $1"; }
bad(){ FAIL=$((FAIL+1)); echo "FAIL - $1"; }

make_data() {
  local d=$1
  mkdir -p "$d"
  printf 'key' >"$d/secret.key"; chmod 600 "$d/secret.key"
  printf 'creds' >"$d/registry_creds.enc"; chmod 600 "$d/registry_creds.enc"
  python3 - "$d/mirror-lens.db" <<'PY'
import sqlite3, sys
c = sqlite3.connect(sys.argv[1])
c.execute("create table cache_entries(key text primary key)")
c.execute("insert into cache_entries(key) values('docker:m:1:x')")
c.commit()
PY
}

r=$(mktemp -d); make_data "$r"
if "$SCRIPT" --data "$r" >"$r/log" 2>&1; then
  bad "有旧缓存时检查模式应拒绝"
elif grep -q '旧 docker' "$r/log" && [ -f "$r/registry_creds.enc" ]; then
  ok "检查模式发现旧缓存且不改文件"
else
  bad "检查模式输出异常: $(tail -2 "$r/log")"
fi
rm -rf "$r"

r=$(mktemp -d); make_data "$r"
if "$SCRIPT" --data "$r" --apply --clean-cache >"$r/log" 2>&1 &&
   [ ! -f "$r/registry_creds.enc" ] &&
   [ "$(python3 - "$r/mirror-lens.db" <<'PY'
import sqlite3, sys
print(sqlite3.connect(sys.argv[1]).execute("select count(*) from cache_entries").fetchone()[0])
PY
)" = 0 ]; then
  ok "执行模式移走凭据并清空旧缓存"
else
  bad "执行模式失败: $(tail -3 "$r/log")"
fi
rm -rf "$r"

r=$(mktemp -d); mkdir -p "$r"
printf 'key' >"$r/secret.key"; chmod 600 "$r/secret.key"
python3 - "$r/mirror-lens.db" <<'PY'
import sqlite3, sys
c=sqlite3.connect(sys.argv[1]); c.execute("create table cache_entries(key text primary key)"); c.commit()
PY
if "$SCRIPT" --data "$r" --apply >"$r/log" 2>&1; then
  ok "没有旧缓存/凭据时可直接执行"
else
  bad "空数据执行失败: $(tail -2 "$r/log")"
fi
rm -rf "$r"

echo "----"
echo "PASS=$PASS FAIL=$FAIL"
[ "$FAIL" -eq 0 ]
