#!/usr/bin/env bash
# 启动门禁测试：已有库缺 key、未完成恢复、正常启动。
# 用法: BIN=/path/to/mirror-lens scripts/startup_test.sh
set -uo pipefail
BIN=${BIN:?需要 BIN=/path/to/mirror-lens}
PASS=0
FAIL=0
ok() { PASS=$((PASS + 1)); echo "ok   - $1"; }
bad() { FAIL=$((FAIL + 1)); echo "FAIL - $1"; }

run() { # run <dir> [timeout]
  local d=$1 t=${2:-10}
  ( cd "$d" && timeout "$t" "$BIN" -config /dev/null -data "$d" -addr 127.0.0.1:0 2>&1 )
}

# 1. 已有 DB、缺 key：拒绝启动，不生成 key
d=$(mktemp -d); printf 'sqlite' >"$d/mirror-lens.db"
out=$(run "$d"); rc=$?
if [ $rc -ne 0 ] && grep -q "secret.key 缺失" <<<"$out" && [ ! -f "$d/secret.key" ] && [ "$(cat "$d/mirror-lens.db")" = sqlite ]; then
  ok "已有 DB 缺 key：拒绝且不改库"
else
  bad "缺 key 用例: rc=$rc out=$(tail -1 <<<"$out")"
fi
rm -rf "$d"

# 2. 未完成恢复 + 缺 key：必须先报恢复切换（优先级高于 key），且不生成 key
d=$(mktemp -d); printf 'sqlite' >"$d/mirror-lens.db"; printf '{"txid":"t"}' >"$d/.restore-journal.json"
out=$(run "$d"); rc=$?
if [ $rc -ne 0 ] && grep -q "未完成的恢复切换" <<<"$out" && [ ! -f "$d/secret.key" ]; then
  ok "journal 优先于密钥检查"
else
  bad "journal 优先级用例: rc=$rc out=$(tail -1 <<<"$out")"
fi
rm -rf "$d"

# 3. 配对完好：能进入正常启动（3 秒内不退出）
d=$(mktemp -d); : >"$d/mirror-lens.db"
head -c 32 /dev/urandom | base64 >"$d/secret.key"
out=$(run "$d" 3); rc=$?
if [ $rc -eq 124 ]; then
  ok "配对完好时正常启动"
else
  bad "正常启动用例: rc=$rc out=$(tail -1 <<<"$out")"
fi
rm -rf "$d"

# 4. 全新安装：库不存在，允许生成 key 并启动
d=$(mktemp -d)
out=$(run "$d" 3); rc=$?
if [ $rc -eq 124 ] && [ -f "$d/secret.key" ]; then
  ok "全新安装生成密钥并启动"
else
  bad "首次安装用例: rc=$rc out=$(tail -1 <<<"$out")"
fi
rm -rf "$d"

# 5. 无法确认 journal 状态（自指符号链接 → ELOOP，非 ENOENT）：拒绝启动
base=$(mktemp -d); mkdir -p "$base/data"
ln -s .restore-journal.json "$base/data/.restore-journal.json"
out=$(timeout 10 "$BIN" -config /dev/null -data "$base/data" -addr 127.0.0.1:0 2>&1); rc=$?
if [ $rc -ne 0 ] && grep -q "无法确认恢复切换日志状态" <<<"$out" && [ ! -f "$base/data/secret.key" ]; then
  ok "journal 状态不可判定时拒绝启动"
else
  bad "Stat 错误用例: rc=$rc out=$(tail -1 <<<"$out")"
fi
rm -rf "$base"

echo "----"
echo "PASS=$PASS FAIL=$FAIL"
[ "$FAIL" -eq 0 ]
