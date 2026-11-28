# G0 重取证证据：5 守卫 UPDATE 语义实测（F9）

- 取证基线：`feat/3.5.26-maintenance-fix` HEAD `4e40a3ebbbdd81356c2d00b007a6fa54dfd503bd`（`docs/3.5.26-unattended-delivery.md` L506 G0 重取证项）
- 取证目的：核实实际运行的 SQLite 版本；对新的「带 5 守卫 UPDATE」重做六类语义实测；纳入证据包。
- 取证日期：2026-11-28

## 1. 实际运行的 SQLite 版本（关键结论）

- 生产应用使用 `modernc.org/sqlite v1.61.0`（纯 Go 实现），由 `go.mod` / `go.sum` 锁定；生产容器 `mirror-lens` 内无 sqlite3 CLI，故版本以**同款引擎运行时** `SELECT sqlite_version()` 为准。
- 本次实测在 `golang:1.27.3` 容器内用 `go get modernc.org/sqlite@v1.61.0`，与生产 go.mod 同版本。
- **实测运行时版本：`sqlite_version() = 3.54.1`**（modernc v1.61.0 内核版本）。
- 旧证据声称的「SQLite 3.53.1 双分支 UPSERT 实测」随 R10 结构撤销整体作废，不再作为既成事实引用；本机 Python 自带 C SQLite 3.53.1 亦不代表生产引擎，本次结论以 modernc 运行时为准。

实测依赖锁定（容器内 `go.mod` 关键行）：

```
require modernc.org/sqlite v1.61.0
```

## 2. 执行命令

```bash
# 拉取 golang 镜像并起临时容器（完成后已删除）
docker pull -q golang:1.27.3
docker run -d --name ml-g0-evidence --network=host golang:1.27.3 sleep 3600
docker exec ml-g0-evidence mkdir -p /work
docker cp <main.go> ml-g0-evidence:/work/main.go      # main.go 全文见文末
docker exec -w /work ml-g0-evidence sh -c 'go mod init g0evidence && go get modernc.org/sqlite@v1.61.0 && go mod tidy'
docker exec -w /work ml-g0-evidence go run .
```

## 3. 5 守卫 UPDATE SQL（设计稿 L482-497 逐字沿用）

```sql
UPDATE source_host_caps
SET last_verdict=?, effective_status=?, registry_path_mode=?, registry_prefix=?
WHERE source_id=? AND host=?
  AND enabled=1
  AND effective_status <> 'Mismatch'
  AND NOT EXISTS (
      SELECT 1 FROM registry_host_recovery r
      WHERE r.source_id = source_host_caps.source_id
        AND r.family = 'registry'
        AND r.scope = source_host_caps.host
        AND r.state = 'hard-excluded'
  )
  AND COALESCE((SELECT policy_gen FROM source_realm_policy p
                WHERE p.source_id=source_host_caps.source_id
                  AND p.host=source_host_caps.host), 0) = ?;
```

实测程序额外用 `RETURNING source_id, host` 统计影响行数，并与 `RowsAffected()` 交叉验证，两法结果一致。

守卫清单（5 项全部必须）：① caps 行存在（影响行数 0 即 skip）；② enabled=1；③ effective_status<>'Mismatch'；④ 无 hard-excluded recovery（关联键 `source_id + family='registry' + scope=host`）；⑤ policy_gen 快照匹配（无策略行按 COALESCE=0 处理）。

## 4. 实测结果（原始输出）

六类为任务书 L506 点名要求；另加两类 COALESCE(policy_gen)=0 边界补充验证。

```
=== G0 重取证：modernc.org/sqlite v1.61.0 ===
[1] 实际运行的 SQLite 版本（运行时 API）:
    sqlite_version() = 3.54.1

[2] 建生产 schema（canonical.sql 相关表 DDL 逐字沿用）:
    OK: sources / source_realm_policy / source_host_caps / registry_host_recovery

[3] 5 守卫 UPDATE SQL（设计稿 L482-497，逐字沿用；末尾加 RETURNING 取影响行数）:

UPDATE source_host_caps
SET last_verdict=?, effective_status=?, registry_path_mode=?, registry_prefix=?
WHERE source_id=? AND host=?
  AND enabled=1
  AND effective_status <> 'Mismatch'
  AND NOT EXISTS (
      SELECT 1 FROM registry_host_recovery r
      WHERE r.source_id = source_host_caps.source_id
        AND r.family = 'registry'
        AND r.scope = source_host_caps.host
        AND r.state = 'hard-excluded'
  )
  AND COALESCE((SELECT policy_gen FROM source_realm_policy p
                WHERE p.source_id=source_host_caps.source_id
                  AND p.host=source_host_caps.host), 0) = ?
RETURNING source_id, host;

[4] 六类语义实测（每类独立内存库，避免相互污染）：
    [OK  ] caps 行不存在              期望影响行数=0  RETURNING行数=0  RowsAffected=0  写后effective_status=""
    [OK  ] enabled=0              期望影响行数=0  RETURNING行数=0  RowsAffected=0  写后effective_status="Unknown"
    [OK  ] Mismatch               期望影响行数=0  RETURNING行数=0  RowsAffected=0  写后effective_status="Mismatch"
    [OK  ] hard-excluded recovery 期望影响行数=0  RETURNING行数=0  RowsAffected=0  写后effective_status="Unknown"
    [OK  ] policy_gen 不匹配         期望影响行数=0  RETURNING行数=0  RowsAffected=0  写后effective_status="Unknown"
    [OK  ] 无策略行按 policy_gen=0（快照 0 应写入） 期望影响行数=1  RETURNING行数=1  RowsAffected=1  写后effective_status="Supported"
    [OK  ] 无策略行快照非0（skip）         期望影响行数=0  RETURNING行数=0  RowsAffected=0  写后effective_status="Unknown"
    [OK  ] 正常 Supported           期望影响行数=1  RETURNING行数=1  RowsAffected=1  写后effective_status="Supported"

RESULT: PASS — 六类场景全部符合 5 守卫 UPDATE 冻结语义（skip=0 / 写入=1），RETURNING 与 RowsAffected 两法一致。
```

## 5. 结论

- 实际运行 SQLite 版本：**3.54.1**（modernc.org/sqlite v1.61.0 内核），非本机 C SQLite 3.53.1，旧 3.53.1 证据作废。
- 六类必测场景全部符合冻结语义：五类守卫不满足时影响行数 0（skip，不写任何字段）；正常 Supported 影响行数 1（实际写入）。
- 两个补充边界确认：`COALESCE(policy_gen,0)` 语义正确——无策略行时快照 0 写入、非 0 skip。
- `RETURNING` 计数与 `RowsAffected()` 两法一致。
- 本证据满足 L506 三项要求（版本核实 / 六类实测 / 证据归档），可将 G0 重取证项标记为完成。

## 附：可复现源码 main.go

```go
package main

// G0 重取证：在与应用生产同款引擎 modernc.org/sqlite v1.61.0 上，
// 核实实际运行的 SQLite 版本，并对设计稿 §3 F9 冻结的 5 守卫 UPDATE
// 重做六类语义实测（caps 行不存在 / enabled=0 / Mismatch /
// hard-excluded / policy_gen 不匹配 / 正常 Supported）。
//
// 证据要求（任务书 L506）：记录命令、SQL 与结果。本程序的输出即为证据。

import (
	"database/sql"
	"fmt"
	"os"

	_ "modernc.org/sqlite"
)

// 生产 schema（取自 internal/store/schema/canonical.sql，逐字沿用相关表 DDL）
const schema = `
CREATE TABLE sources (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  base_url TEXT NOT NULL UNIQUE,
  name TEXT NOT NULL DEFAULT '',
  enabled INTEGER NOT NULL DEFAULT 1,
  status TEXT NOT NULL DEFAULT 'pending',
  created_at INTEGER NOT NULL
);
CREATE TABLE source_realm_policy (
  source_id INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
  host TEXT NOT NULL,
  primary_realm TEXT NOT NULL,
  extra_realms TEXT NOT NULL DEFAULT '[]',
  policy_gen INTEGER NOT NULL DEFAULT 1 CHECK(policy_gen > 0),
  updated_at INTEGER NOT NULL,
  PRIMARY KEY(source_id, host)
);
CREATE TABLE source_host_caps (
  source_id INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
  host TEXT NOT NULL,
  enabled INTEGER NOT NULL DEFAULT 1 CHECK(enabled IN (0,1)),
  last_verdict TEXT NOT NULL CHECK(last_verdict IN ('Supported','Unsupported','Inconclusive','Mismatch')),
  effective_status TEXT NOT NULL CHECK(effective_status IN ('Unknown','Supported','Unsupported','Mismatch')),
  registry_path_mode TEXT NOT NULL DEFAULT '' CHECK(registry_path_mode IN ('','native','host_prefix')),
  registry_prefix INTEGER NOT NULL DEFAULT 0 CHECK(registry_prefix IN (0,1)),
  diagnostics TEXT NOT NULL DEFAULT '',
  PRIMARY KEY(source_id, host),
  CHECK(effective_status <> 'Supported' OR registry_path_mode IN ('native','host_prefix'))
);
CREATE TABLE registry_host_recovery (
  source_id INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
  family TEXT NOT NULL DEFAULT 'registry',
  scope TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL DEFAULT 'initial',
  gen INTEGER NOT NULL DEFAULT 0,
  last_probe_at INTEGER NOT NULL DEFAULT 0,
  last_success_at INTEGER NOT NULL DEFAULT 0,
  next_probe_at INTEGER NOT NULL DEFAULT 0,
  rec_failures INTEGER NOT NULL DEFAULT 0,
  retry_failures INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  updated_at INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(source_id, family, scope)
);
`

// 设计稿 §3 F9 L482-497 冻结的 5 守卫 UPDATE，逐字沿用；
// 唯一差异：末尾 RETURNING 用于取影响行数，modernc 支持；
// 同时用 RowsAffected() 交叉验证（两种取数法结果必须一致）。
const guardUpdate = `
UPDATE source_host_caps
SET last_verdict=?, effective_status=?, registry_path_mode=?, registry_prefix=?
WHERE source_id=? AND host=?
  AND enabled=1
  AND effective_status <> 'Mismatch'
  AND NOT EXISTS (
      SELECT 1 FROM registry_host_recovery r
      WHERE r.source_id = source_host_caps.source_id
        AND r.family = 'registry'
        AND r.scope = source_host_caps.host
        AND r.state = 'hard-excluded'
  )
  AND COALESCE((SELECT policy_gen FROM source_realm_policy p
                WHERE p.source_id=source_host_caps.source_id
                  AND p.host=source_host_caps.host), 0) = ?
RETURNING source_id, host;`

type tc struct {
	name    string // 场景名
	setup   string // 造数 SQL（分号分隔，逐条执行）；caps/recovery/policy 初始状态
	gen     int    // 传给 UPDATE 的 policy_gen 快照参数
	wantN   int64  // 期望影响行数：0=skip，1=写入
	wantVer string // 期望写入后的 effective_status
}

func mustExec(db *sql.DB, q string, args ...any) {
	if _, err := db.Exec(q, args...); err != nil {
		fmt.Printf("FATAL exec %q: %v\n", q, err)
		os.Exit(1)
	}
}

func main() {
	fmt.Println("=== G0 重取证：modernc.org/sqlite v1.61.0 ===")
	fmt.Println("[1] 实际运行的 SQLite 版本（运行时 API）:")
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil { panic(err) }
	defer db.Close()
	var ver string
	if err := db.QueryRow("SELECT sqlite_version()").Scan(&ver); err != nil { panic(err) }
	fmt.Printf("    sqlite_version() = %s\n", ver)

	fmt.Println("\n[2] 建生产 schema（canonical.sql 相关表 DDL 逐字沿用）:")
	mustExec(db, schema)
	fmt.Println("    OK: sources / source_realm_policy / source_host_caps / registry_host_recovery")

	fmt.Println("\n[3] 5 守卫 UPDATE SQL（设计稿 L482-497，逐字沿用；末尾加 RETURNING 取影响行数）:")
	fmt.Println(guardUpdate)

	// 写入目标值固定：Supported / native / prefix=1
	last, eff, mode, prefix := "Supported", "Supported", "native", 1

	cases := []tc{
		{
			name:  "caps 行不存在",
			setup: "INSERT INTO sources(id,base_url,name,created_at) VALUES(1,'u1','s1',1);",
			gen:   0,
			wantN: 0, // 行不存在 → skip
		},
		{
			name: "enabled=0",
			setup: `INSERT INTO sources(id,base_url,name,created_at) VALUES(1,'u1','s1',1);
INSERT INTO source_host_caps(source_id,host,enabled,last_verdict,effective_status) VALUES(1,'h',0,'Inconclusive','Unknown');`,
			gen: 0, wantN: 0,
		},
		{
			name: "Mismatch",
			setup: `INSERT INTO sources(id,base_url,name,created_at) VALUES(1,'u1','s1',1);
INSERT INTO source_host_caps(source_id,host,enabled,last_verdict,effective_status) VALUES(1,'h',1,'Mismatch','Mismatch');`,
			gen: 0, wantN: 0,
		},
		{
			name: "hard-excluded recovery",
			setup: `INSERT INTO sources(id,base_url,name,created_at) VALUES(1,'u1','s1',1);
INSERT INTO source_host_caps(source_id,host,enabled,last_verdict,effective_status) VALUES(1,'h',1,'Inconclusive','Unknown');
INSERT INTO registry_host_recovery(source_id,family,scope,state) VALUES(1,'registry','h','hard-excluded');`,
			gen: 0, wantN: 0,
		},
		{
			name: "policy_gen 不匹配",
			setup: `INSERT INTO sources(id,base_url,name,created_at) VALUES(1,'u1','s1',1);
INSERT INTO source_host_caps(source_id,host,enabled,last_verdict,effective_status) VALUES(1,'h',1,'Inconclusive','Unknown');
INSERT INTO source_realm_policy(source_id,host,primary_realm,policy_gen,updated_at) VALUES(1,'h','official',2,1);`,
			gen: 1, wantN: 0, // 快照 1 != 实际 2 → skip
		},
		{
			name: "无策略行按 policy_gen=0（快照 0 应写入）",
			setup: `INSERT INTO sources(id,base_url,name,created_at) VALUES(1,'u1','s1',1);
INSERT INTO source_host_caps(source_id,host,enabled,last_verdict,effective_status) VALUES(1,'h',1,'Inconclusive','Unknown');`,
			gen: 0, wantN: 1, wantVer: "Supported",
		},
		{
			name: "无策略行快照非0（skip）",
			setup: `INSERT INTO sources(id,base_url,name,created_at) VALUES(1,'u1','s1',1);
INSERT INTO source_host_caps(source_id,host,enabled,last_verdict,effective_status) VALUES(1,'h',1,'Inconclusive','Unknown');`,
			gen: 1, wantN: 0,
		},
		{
			name: "正常 Supported",
			setup: `INSERT INTO sources(id,base_url,name,created_at) VALUES(1,'u1','s1',1);
INSERT INTO source_host_caps(source_id,host,enabled,last_verdict,effective_status) VALUES(1,'h',1,'Inconclusive','Unknown');
INSERT INTO source_realm_policy(source_id,host,primary_realm,policy_gen,updated_at) VALUES(1,'h','official',3,1);`,
			gen: 3, wantN: 1, wantVer: "Supported",
		},
	}

	fmt.Println("\n[4] 六类语义实测（每类独立内存库，避免相互污染）：")
	allOK := true
	for _, c := range cases {
		// 每类独立连接
		cdb, e := sql.Open("sqlite", ":memory:")
		if e != nil { panic(e) }
		if _, e = cdb.Exec(schema); e != nil { panic(e) }
		for _, s := range splitSemi(c.setup) {
			if s == "" { continue }
			mustExec(cdb, s)
		}
		// RETURNING 行数法
		rows, e := cdb.Query(guardUpdate, last, eff, mode, prefix, 1, "h", c.gen)
		if e != nil {
			fmt.Printf("    [%-22s] FATAL query: %v\n", c.name, e); allOK = false; cdb.Close(); continue
		}
		nRet := int64(0)
		for rows.Next() { nRet++ }
		if e := rows.Err(); e != nil { fmt.Printf("    [%-22s] FATAL rows: %v\n", c.name, e); allOK = false }
		rows.Close()

		// RowsAffected 法（不带 RETURNING，逐字用设计稿原 SQL）
		origSQL := guardUpdate[:len(guardUpdate)-len("\nRETURNING source_id, host;")]
		res, e := cdb.Exec(origSQL, last, eff, mode, prefix, 1, "h", c.gen)
		if e != nil { fmt.Printf("    [%-22s] FATAL exec: %v\n", c.name, e); allOK = false; cdb.Close(); continue }
		nAff, e := res.RowsAffected()
		if e != nil { fmt.Printf("    [%-22s] FATAL rowsAffected: %v\n", c.name, e); allOK = false }

		var gotVer string
		_ = cdb.QueryRow(`SELECT effective_status FROM source_host_caps WHERE source_id=1 AND host='h'`).Scan(&gotVer)

		ok := nRet == c.wantN && nAff == c.wantN && (c.wantVer == "" || gotVer == c.wantVer)
		if !ok { allOK = false }
		mark := "FAIL"; if ok { mark = "OK  " }
		fmt.Printf("    [%s] %-22s 期望影响行数=%d  RETURNING行数=%d  RowsAffected=%d  写后effective_status=%q\n",
			mark, c.name, c.wantN, nRet, nAff, gotVer)
		cdb.Close()
	}
	fmt.Println()
	if allOK {
		fmt.Println("RESULT: PASS — 六类场景全部符合 5 守卫 UPDATE 冻结语义（skip=0 / 写入=1），RETURNING 与 RowsAffected 两法一致。")
	} else {
		fmt.Println("RESULT: FAIL — 存在不符合预期的场景。")
		os.Exit(2)
	}
}

// 简单分号切分（setup SQL 中无分号字符串字面量）
func splitSemi(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == ';' { out = append(out, cur); cur = "" } else { cur += string(r) }
	}
	out = append(out, cur)
	return out
}
```
