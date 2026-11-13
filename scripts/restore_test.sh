#!/usr/bin/env bash
# restore.sh 的故障注入与拒绝用例。只操作 mktemp 目录，不碰真实数据。
set -uo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
RESTORE="$HERE/restore.sh"
PASS=0
FAIL=0

ok() { PASS=$((PASS + 1)); echo "ok   - $1"; }
bad() { FAIL=$((FAIL + 1)); echo "FAIL - $1"; }
sha() { sha256sum "$1" | awk '{print $1}'; }

make_fixture() { # $1=root; 生成 data 旧配对 + unit 新配对
  local root=$1
  mkdir -p "$root/data" "$root/unit"
  printf 'OLD-DB-CONTENT' >"$root/data/mirror-lens.db"
  printf 'OLD-KEY-CONTENT' >"$root/data/secret.key"
  chmod 600 "$root/data/"*
  printf 'NEW-DB-CONTENT' >"$root/unit/mirror-lens.db"
  printf 'NEW-KEY-CONTENT' >"$root/unit/secret.key"
  chmod 600 "$root/unit/"*
  local ds ks dn kn
  ds=$(sha "$root/unit/mirror-lens.db"); ks=$(sha "$root/unit/secret.key")
  dn=$(stat -c %s "$root/unit/mirror-lens.db"); kn=$(stat -c %s "$root/unit/secret.key")
  cat >"$root/unit/manifest.json" <<EOF
{
  "schema": 1,
  "app_version": "test",
  "created_at": "2026-11-13T00:00:00+08:00",
  "created_at_unix": 1,
  "db": {
    "file": "mirror-lens.db",
    "sha256": "$ds",
    "size": $dn
  },
  "key": {
    "file": "secret.key",
    "sha256": "$ks",
    "size": $kn
  }
}
EOF
}

pair_is() { # $1=data $2=dbcontent $3=keycontent
  [ "$(cat "$1/mirror-lens.db" 2>/dev/null)" = "$2" ] && [ "$(cat "$1/secret.key" 2>/dev/null)" = "$3" ]
}

run_recover() { "$RESTORE" --recover --data "$1/data" >"$1/recover.log" 2>&1; }

# 1. 正常恢复
r=$(mktemp -d); make_fixture "$r"
if "$RESTORE" --backup "$r/unit" --data "$r/data" >"$r/log" 2>&1 && pair_is "$r/data" NEW-DB-CONTENT NEW-KEY-CONTENT &&
   [ ! -f "$r/data/.restore-journal.json" ] && ls "$r/data"/.restore-old-* >/dev/null 2>&1; then
  ok "正常恢复切换并保留现场"
else
  bad "正常恢复失败: $(tail -3 "$r/log")"
fi
rm -rf "$r"

# 2..6 拒绝用例：manifest 缺失、DB 篡改、key 篡改、DB 截断、manifest 哈希错误
for kind in no-manifest db-tamper key-tamper db-truncate bad-sha; do
  r=$(mktemp -d); make_fixture "$r"
  case $kind in
    no-manifest) rm "$r/unit/manifest.json" ;;
    db-tamper) printf 'X' >>"$r/unit/mirror-lens.db" ;;
    key-tamper) printf 'X' >>"$r/unit/secret.key" ;;
    db-truncate) truncate -s 3 "$r/unit/mirror-lens.db" ;;
    bad-sha) sed -i 's/"sha256": "./"sha256": "0/' "$r/unit/manifest.json" ;;
  esac
  if "$RESTORE" --backup "$r/unit" --data "$r/data" >"$r/log" 2>&1; then
    bad "$kind 应被拒绝却成功"
  elif pair_is "$r/data" OLD-DB-CONTENT OLD-KEY-CONTENT && [ ! -f "$r/data/.restore-journal.json" ]; then
    ok "$kind 被拒绝且原配对可用"
  else
    bad "$kind 被拒绝但原配对受损"
  fi
  rm -rf "$r"
done

# 7. 切换各阶段 SIGKILL 后 --recover
for point in after_journal after_stage after_save after_db after_key after_verify; do
  r=$(mktemp -d); make_fixture "$r"
  RESTORE_TEST_CRASH=$point "$RESTORE" --backup "$r/unit" --data "$r/data" >"$r/log" 2>&1
  if [ ! -f "$r/data/.restore-journal.json" ]; then
    bad "$point 未留下切换日志"
    rm -rf "$r"; continue
  fi
  if ! run_recover "$r"; then
    bad "$point 后 --recover 失败: $(tail -2 "$r/recover.log")"
  elif [ -f "$r/data/.restore-journal.json" ]; then
    bad "$point 后 --recover 未清理日志"
  elif pair_is "$r/data" NEW-DB-CONTENT NEW-KEY-CONTENT || pair_is "$r/data" OLD-DB-CONTENT OLD-KEY-CONTENT; then
    ok "$point 后 --recover 收敛到完整配对"
  else
    bad "$point 后 DB/key 混合"
  fi
  rm -rf "$r"
done

# 9. 旧 WAL/SHM 必须随旧配对移走，不能挂到新库上
r=$(mktemp -d); make_fixture "$r"
printf 'OLD-WAL' >"$r/data/mirror-lens.db-wal"
printf 'OLD-SHM' >"$r/data/mirror-lens.db-shm"
if "$RESTORE" --backup "$r/unit" --data "$r/data" >"$r/log" 2>&1 &&
   pair_is "$r/data" NEW-DB-CONTENT NEW-KEY-CONTENT &&
   [ ! -f "$r/data/mirror-lens.db-wal" ] && [ ! -f "$r/data/mirror-lens.db-shm" ] &&
   [ "$(cat "$r/data"/.restore-old-*/mirror-lens.db-wal 2>/dev/null)" = OLD-WAL ] &&
   [ "$(cat "$r/data"/.restore-old-*/mirror-lens.db-shm 2>/dev/null)" = OLD-SHM ]; then
  ok "旧 WAL/SHM 随旧配对归档"
else
  bad "WAL/SHM 处理失败: $(tail -3 "$r/log")"
fi
rm -rf "$r"

# 10. 已完成恢复后重复 --recover 不得改变结果
r=$(mktemp -d); make_fixture "$r"
"$RESTORE" --backup "$r/unit" --data "$r/data" >"$r/log" 2>&1
h1=$(sha "$r/data/mirror-lens.db"):$(sha "$r/data/secret.key")
if run_recover "$r" && run_recover "$r"; then
  h2=$(sha "$r/data/mirror-lens.db"):$(sha "$r/data/secret.key")
  if [ "$h1" = "$h2" ] && pair_is "$r/data" NEW-DB-CONTENT NEW-KEY-CONTENT; then
    ok "重复 --recover 幂等"
  else
    bad "重复 --recover 改变了结果"
  fi
else
  bad "重复 --recover 报错: $(tail -2 "$r/recover.log")"
fi
rm -rf "$r"

# 11. --recover 自身失败时，journal 必须保留（服务仍被拦）
r=$(mktemp -d); make_fixture "$r"
RESTORE_TEST_CRASH=after_save "$RESTORE" --backup "$r/unit" --data "$r/data" >"$r/log" 2>&1
rm -rf "$r/data"/.restore-old-*
if run_recover "$r"; then
  bad "--recover 现场缺失时不应成功"
elif [ ! -f "$r/data/.restore-journal.json" ]; then
  bad "--recover 失败后 journal 被删，服务会绕过拦截"
else
  ok "--recover 失败保留 journal"
fi
rm -rf "$r"

# 8. 非 root 服务用户恢复
r=$(mktemp -d); make_fixture "$r"
if id -u 1000 >/dev/null 2>&1; then
  chown -R 1000:1000 "$r"
  if setpriv --reuid=1000 --regid=1000 --clear-groups "$RESTORE" --backup "$r/unit" --data "$r/data" >"$r/log" 2>&1 &&
     pair_is "$r/data" NEW-DB-CONTENT NEW-KEY-CONTENT; then
    ok "uid 1000 完成恢复"
  else
    bad "uid 1000 恢复失败: $(tail -3 "$r/log")"
  fi
else
  ok "无 uid 1000，跳过（环境不支持）"
fi
rm -rf "$r"

echo "----"
echo "PASS=$PASS FAIL=$FAIL"
[ "$FAIL" -eq 0 ]
