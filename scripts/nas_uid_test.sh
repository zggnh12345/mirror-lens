#!/bin/sh
# C5：在真实 /vol1 数据目录（由生产容器挂载）上，以生产 UID/GID 1000:1000
# 跑文件系统、flock、SQLite checkpoint、升级 journal 恢复和回滚测试。
# 只使用独立临时目录；不读取/不修改生产 mirror-lens.db。
#
# 在容器内运行（容器 Config.User=1000:1000）：
#   docker exec -i mirror-lens sh -s < scripts/nas_uid_test.sh
# 需要先把 db-migrate、upgrade-db.sh 放到 TEST_ROOT，旧库副本放到 OLD_DB。
set -u

ROOT=${TEST_ROOT:-/data/.uidtest-c5}
TOOL=${TOOL:-$ROOT/db-migrate}
UPGRADE=${UPGRADE:-$ROOT/upgrade-db.sh}
OLD_DB=${OLD_DB:-/data/mirror-lens.db.bak-3.4.6}
OLD_DB_354=${OLD_DB_354:-}
PASS=0; FAIL=0; SKIP=0
ok() { PASS=$((PASS+1)); echo "ok   - $1"; }
bad() { FAIL=$((FAIL+1)); echo "FAIL - $1"; }
skip() { SKIP=$((SKIP+1)); echo "skip - $1"; }

[ "$(id -u):$(id -g)" = "1000:1000" ] && ok "运行身份 1000:1000" || bad "运行身份 $(id -u):$(id -g)，期望 1000:1000"
[ -x "$TOOL" ] || { echo "缺少可执行 db-migrate: $TOOL" >&2; exit 2; }
[ -f "$UPGRADE" ] || { echo "缺少 upgrade-db.sh: $UPGRADE" >&2; exit 2; }
[ -f "$OLD_DB" ] || { echo "缺少 3.4.6 备份副本: $OLD_DB" >&2; exit 2; }
case "$ROOT" in /data/*) : ;; *) echo "TEST_ROOT 必须在 /data 下: $ROOT" >&2; exit 2 ;; esac
mkdir -p "$ROOT" || exit 2

d=$(mktemp -d "$ROOT/fs.XXXXXX") || exit 2
trap 'rm -rf "$d" "$ROOT/bin"' EXIT
mkdir -p "$ROOT/bin"
cat >"$ROOT/bin/docker" <<'FAKE'
#!/bin/sh
case "$3" in
  *Name*) echo /test ;;
  *Image*) echo test:1 ;;
  *Running*) echo false ;;
  *) echo unknown ;;
esac
FAKE
chmod 700 "$ROOT/bin/docker"

# 1. 文件创建/写入/属主/权限/fsync/rename
printf 'c5-payload' >"$d/f" && ok "在 /vol1 真实文件系统创建文件" || bad "创建文件失败"
[ "$(stat -c '%u:%g' "$d/f")" = "1000:1000" ] && ok "新文件属主 1000:1000" || bad "新文件属主 $(stat -c '%u:%g' "$d/f")"
chmod 600 "$d/f"
[ "$(stat -c '%a' "$d/f")" = "600" ] && ok "chmod 600 生效" || bad "chmod 未生效"
dd if=/dev/zero of="$d/fsync.bin" bs=4096 count=1 conv=fsync 2>/dev/null &&
  [ "$(stat -c %s "$d/fsync.bin")" = 4096 ] && ok "dd conv=fsync 写入 4096 字节" || bad "fsync 写入失败"
mv "$d/fsync.bin" "$d/fsync.renamed" && sync &&
  [ "$(stat -c %s "$d/fsync.renamed")" = 4096 ] && ok "rename 后 sync 可读且长度正确" || bad "rename/sync 失败"

# 2. flock 竞争：持锁进程存在时第二个必须失败，释放后必须成功
flock -n "$d/flock" -c 'sleep 2' & lp=$!
sleep 0.3
if flock -n "$d/flock" -c true; then bad "持锁期间第二个 flock 未失败"; else ok "flock 竞争被拒绝"; fi
wait "$lp" 2>/dev/null
if flock -n "$d/flock" -c true; then ok "释放后 flock 可获取"; else bad "释放后 flock 仍失败"; fi

# 3. SQLite：init / checkpoint / 重开 verify
if "$TOOL" init --out "$d/canonical.db" --baseline fresh >"$d/init.log" 2>&1 &&
   "$TOOL" checkpoint --db "$d/canonical.db" >"$d/ckpt.log" 2>&1 &&
   "$TOOL" verify --db "$d/canonical.db" >"$d/verify.log" 2>&1 &&
   [ ! -e "$d/canonical.db-wal" ] && [ ! -e "$d/canonical.db-shm" ]; then
  ok "SQLite init+checkpoint+重开 verify，无 WAL/SHM 残留"
else
  bad "SQLite checkpoint/verify 失败: $(tail -2 "$d/init.log" "$d/ckpt.log" "$d/verify.log" 2>/dev/null | tr '\n' ' ')"
fi

# 4. journal 恢复：after_migrate / after_journal 崩溃后必须回到完整旧库
# 使用 3.4.6 生产库的独立副本，不碰生产路径。
run_up() {
  PATH="$ROOT/bin:$PATH" sh "$UPGRADE" --container test --expect-image test:1 \
    --data "$d" --tool "$TOOL" "$@"
}
for point in after_migrate after_journal; do
  rm -rf "$d"; mkdir -p "$d"
  cp "$OLD_DB" "$d/mirror-lens.db"; chmod 600 "$d/mirror-lens.db"
  printf 'TEST-KEY' >"$d/secret.key"; chmod 600 "$d/secret.key"
  old_sha=$(sha256sum "$d/mirror-lens.db" | awk '{print $1}')
  DB_UPGRADE_CRASH=$point run_up --baseline 3.4.6 >"$d/up.log" 2>&1
  rc=$?
  if [ "$rc" -ne 137 ]; then
    bad "$point 未触发 SIGKILL (rc=$rc)"
    continue
  fi
  if run_up --recover >"$d/recover.log" 2>&1 &&
     [ "$(sha256sum "$d/mirror-lens.db" | awk '{print $1}')" = "$old_sha" ] &&
     [ ! -f "$d/.db-upgrade-journal.json" ]; then
    ok "$point 崩溃后 recover 回到完整旧库并清 journal"
  else
    bad "$point recover 后状态不正确: $(tail -3 "$d/recover.log")"
  fi
done

# 5. 正常 3.4.6 真实副本升级（独立目录）
rm -rf "$d"; mkdir -p "$d"
cp "$OLD_DB" "$d/mirror-lens.db"; chmod 600 "$d/mirror-lens.db"
printf 'TEST-KEY' >"$d/secret.key"; chmod 600 "$d/secret.key"
if run_up --baseline 3.4.6 >"$d/up.log" 2>&1 &&
   "$TOOL" verify --db "$d/mirror-lens.db" >/dev/null 2>&1 &&
   [ ! -f "$d/.db-upgrade-journal.json" ]; then
  ok "真实 3.4.6 副本正常升级 + verify + journal 清理"
else
  bad "真实 3.4.6 副本升级失败: $(tail -4 "$d/up.log")"
fi
# 同一库再次 checkpoint 再 verify，确认可重开
if "$TOOL" checkpoint --db "$d/mirror-lens.db" >/dev/null 2>&1 &&
   "$TOOL" verify --db "$d/mirror-lens.db" >/dev/null 2>&1; then
  ok "升级后 checkpoint + 重开 verify"
else
  bad "升级后 checkpoint/重开 verify 失败"
fi

# 6. 真实 3.5.4 副本升级；没有独立副本时明确记为 SKIP，不计 PASS。
if [ -n "$OLD_DB_354" ] && [ -f "$OLD_DB_354" ]; then
  rm -rf "$d"; mkdir -p "$d"
  cp "$OLD_DB_354" "$d/mirror-lens.db"; chmod 600 "$d/mirror-lens.db"
  printf 'TEST-KEY' >"$d/secret.key"; chmod 600 "$d/secret.key"
  if run_up --baseline 3.5.4 >"$d/up354.log" 2>&1 &&
     "$TOOL" verify --db "$d/mirror-lens.db" >/dev/null 2>&1 &&
     [ ! -f "$d/.db-upgrade-journal.json" ]; then
    ok "真实 3.5.4 副本正常升级 + verify + journal 清理"
  else
    bad "真实 3.5.4 副本升级失败: $(tail -4 "$d/up354.log")"
  fi
else
  skip "缺少独立 3.5.4 副本（OLD_DB_354 未设置/不存在）"
fi

echo "----"
echo "PASS=$PASS FAIL=$FAIL SKIP=$SKIP"
[ "$FAIL" -eq 0 ]
