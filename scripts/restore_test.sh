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

# 11a. 切换前中断（暂存区完整）且旧现场丢失：向前补完是安全的
r=$(mktemp -d); make_fixture "$r"
RESTORE_TEST_CRASH=after_save "$RESTORE" --backup "$r/unit" --data "$r/data" >"$r/log" 2>&1
rm -rf "$r/data"/.restore-old-*
if run_recover "$r" && pair_is "$r/data" NEW-DB-CONTENT NEW-KEY-CONTENT && [ ! -f "$r/data/.restore-journal.json" ]; then
  ok "旧现场缺失但暂存区完整：向前补完"
else
  bad "向前补完用例: $(tail -2 "$r/recover.log")"
fi
rm -rf "$r"

# 11b. 新配对与旧现场都不可用：必须失败并保留 journal
r=$(mktemp -d); make_fixture "$r"
RESTORE_TEST_CRASH=after_save "$RESTORE" --backup "$r/unit" --data "$r/data" >"$r/log" 2>&1
rm -rf "$r/data"/.restore-old-* "$r"/.ml-restore-stage-*
if run_recover "$r"; then
  bad "无可恢复材料时 --recover 不应成功"
elif [ ! -f "$r/data/.restore-journal.json" ]; then
  bad "无可恢复材料时 journal 被删，服务会绕过拦截"
else
  ok "无可恢复材料：失败且保留 journal"
fi
rm -rf "$r"

# 12-14. 回滚前必须能证明旧配对完整；不完整时不得破坏正式目录
rollback_guard() { # $1=db|key|hash
  local mode=$1 r h0 c0
  r=$(mktemp -d); make_fixture "$r"
  RESTORE_TEST_CRASH=after_db "$RESTORE" --backup "$r/unit" --data "$r/data" >/dev/null 2>&1
  rm -f "$r"/.ml-restore-stage-*/secret.key   # 断掉「向前补完」路径，强制回滚
  case $mode in
    db)   rm -f "$r/data"/.restore-old-*/mirror-lens.db ;;
    key)  rm -f "$r/data"/.restore-old-*/secret.key ;;
    hash) printf 'TAMPERED' >"$r/data"/.restore-old-*/mirror-lens.db ;;
  esac
  h0=$(sha "$r/data/mirror-lens.db"); c0=$(cat "$r/data/mirror-lens.db")
  if run_recover "$r"; then
    bad "旧配对 $mode 不完整时 --recover 不应成功"
  elif [ "$(sha "$r/data/mirror-lens.db")" != "$h0" ] || [ "$(cat "$r/data/mirror-lens.db")" != "$c0" ]; then
    bad "旧配对 $mode 不完整时正式目录被改动"
  elif [ ! -f "$r/data/.restore-journal.json" ]; then
    bad "旧配对 $mode 不完整时 journal 被清除"
  else
    ok "旧配对 $mode 不完整：拒绝回滚且未破坏正式目录"
  fi
  rm -rf "$r"
}
rollback_guard db
rollback_guard key
rollback_guard hash

# 15. 向前补完失败时回滚：旧配对恢复，新配对移入失败现场（不删除）
r=$(mktemp -d); make_fixture "$r"
RESTORE_TEST_CRASH=after_db "$RESTORE" --backup "$r/unit" --data "$r/data" >/dev/null 2>&1
printf 'BROKEN-KEY' >"$r"/.ml-restore-stage-*/secret.key
run_recover "$r"; rc=$?
if [ $rc -ne 0 ] && pair_is "$r/data" OLD-DB-CONTENT OLD-KEY-CONTENT && [ ! -f "$r/data/.restore-journal.json" ] &&
   [ "$(cat "$r"/data/.restore-failed-*/mirror-lens.db)" = NEW-DB-CONTENT ]; then
  ok "补完失败回滚：旧配对恢复且新配对留档"
else
  bad "回滚成功路径: rc=$rc $(tail -3 "$r/recover.log")"
fi
rm -rf "$r"

# 16. 回滚中途 SIGKILL：已有失败现场不得被覆盖，二次 --recover 必须收敛
r=$(mktemp -d); make_fixture "$r"
RESTORE_TEST_CRASH=after_db "$RESTORE" --backup "$r/unit" --data "$r/data" >/dev/null 2>&1
tx=$(sed -n 's/.*"txid":"\([^"]*\)".*/\1/p' "$r/data/.restore-journal.json")
mkdir -p "$r/data/.restore-failed-$tx"
printf 'SENTINEL' >"$r/data/.restore-failed-$tx/keep"
printf 'BROKEN-KEY' >"$r"/.ml-restore-stage-*/secret.key
RESTORE_TEST_PAUSE_AFTER_FAILED=1 "$RESTORE" --recover --data "$r/data" >"$r/recover1.log" 2>&1 &
pid=$!
for _ in $(seq 1 200); do [ -f "$r/data/.restore-test-paused" ] && break; sleep 0.05; done
if [ ! -f "$r/data/.restore-test-paused" ]; then
  bad "回滚故障注入未到达暂停点: $(tail -2 "$r/recover1.log")"
  kill -9 "$pid" 2>/dev/null; wait "$pid" 2>/dev/null
else
  kill -9 "$pid" 2>/dev/null; wait "$pid" 2>/dev/null
  rm -f "$r/data/.restore-test-paused"
  if run_recover "$r" && pair_is "$r/data" OLD-DB-CONTENT OLD-KEY-CONTENT &&
     [ ! -f "$r/data/.restore-journal.json" ] &&
     [ "$(cat "$r/data/.restore-failed-$tx/keep" 2>/dev/null)" = SENTINEL ] &&
     [ -n "$(grep -l NEW-DB-CONTENT "$r/data"/.restore-failed-*/mirror-lens.db 2>/dev/null)" ]; then
    ok "回滚 SIGKILL 后二次恢复：旧现场保留且配对完整"
  else
    bad "回滚 SIGKILL 重入: $(tail -3 "$r/recover.log")"
  fi
fi
rm -rf "$r"

# 8. 非 root 服务用户恢复
r=$(mktemp -d); make_fixture "$r"
if command -v setpriv >/dev/null 2>&1; then
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
