// Package schema 是 Mirror Lens 的唯一 Canonical Schema 入口。
//
// 运行时只负责校验数据库是否已经是 Canonical Schema；新建空库和旧库搬迁
// 分别由 EnsureFresh 和离线迁移工具调用。这里不包含任何历史 ALTER/回填逻辑。
package schema

import (
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Version 是当前 Canonical Schema 对应的应用版本。
const Version = "3.5.7"

//go:embed canonical.sql
var ddl string

// Meta 是 schema_meta 单行内容。
type Meta struct {
	SchemaVersion   string
	SchemaHash      string
	BaselineVersion string
	AppliedAt       int64
}

// DDL 返回 Canonical DDL 的副本。
func DDL() string { return ddl }

// IsEmpty 判断数据库里是否没有任何用户表。
func IsEmpty(db *sql.DB) (bool, error) {
	var n int
	err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`).Scan(&n)
	if err != nil {
		return false, err
	}
	return n == 0, nil
}

// EnsureFresh 只对空库执行 Canonical DDL，并写入 schema_meta。
// baseline 是 "fresh"、"3.4.6"、"3.5.4" 等来源标识。
//
// DDL 和 schema_meta 必须处于同一个事务：初始化中途失败时不能留下
// “有表但没有元数据”的半成品库。schema hash 也在事务内计算，保证它
// 描述的就是即将提交的最终 Canonical Schema。
func EnsureFresh(db *sql.DB, baseline string) error {
	empty, err := IsEmpty(db)
	if err != nil {
		return err
	}
	if !empty {
		return fmt.Errorf("数据库不是空库，拒绝执行 Canonical DDL")
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(ddl); err != nil {
		return fmt.Errorf("执行 Canonical DDL: %w", err)
	}
	h, err := hashQuery(tx)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO schema_meta(schema_version,schema_hash,baseline_version,applied_at)
		VALUES(?,?,?,strftime('%s','now'))`, Version, h, baseline); err != nil {
		return fmt.Errorf("写入 schema_meta: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交 Canonical 初始化: %w", err)
	}
	return nil
}

// ReadMeta 读取并校验 schema_meta 存在且为单行。单行是 Canonical 的硬约束，
// 不能只取第一行而忽略第二行。
func ReadMeta(db *sql.DB) (Meta, error) {
	var m Meta
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM schema_meta`).Scan(&n); err != nil {
		return m, fmt.Errorf("数据库缺少 schema_meta（旧库必须先用离线工具升级）: %w", err)
	}
	if n != 1 {
		return m, fmt.Errorf("schema_meta 必须恰好一行，实际 %d 行；拒绝启动", n)
	}
	err := db.QueryRow(`SELECT schema_version,schema_hash,baseline_version,applied_at FROM schema_meta`).
		Scan(&m.SchemaVersion, &m.SchemaHash, &m.BaselineVersion, &m.AppliedAt)
	if err != nil {
		return m, fmt.Errorf("读取 schema_meta: %w", err)
	}
	return m, nil
}

// Verify 校验 schema_meta 的版本和实际结构 hash。
func Verify(db *sql.DB) (Meta, error) {
	m, err := ReadMeta(db)
	if err != nil {
		return m, err
	}
	if m.SchemaVersion != Version {
		return m, fmt.Errorf("Schema 版本为 %q，运行时不支持（需要 %q）；请先用对应版本的离线工具升级", m.SchemaVersion, Version)
	}
	h, err := Hash(db)
	if err != nil {
		return m, err
	}
	if h != m.SchemaHash {
		return m, fmt.Errorf("Schema hash 不匹配: 数据库=%s，schema_meta=%s", h, m.SchemaHash)
	}
	return m, nil
}

// VerifyStructure 只校验实际结构 hash，不校验版本字符串。供离线工具使用。
func VerifyStructure(db *sql.DB, want string) error {
	h, err := Hash(db)
	if err != nil {
		return err
	}
	if h != want {
		return fmt.Errorf("Schema hash 不匹配: 实际=%s，期望=%s", h, want)
	}
	return nil
}

type column struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	NotNull bool   `json:"notnull"`
	Default string `json:"default"`
	PK      int    `json:"pk"`
}

type foreignKey struct {
	ID       int    `json:"id"`
	Seq      int    `json:"seq"`
	Table    string `json:"table"`
	From     string `json:"from"`
	To       string `json:"to"`
	OnUpdate string `json:"on_update"`
	OnDelete string `json:"on_delete"`
	Match    string `json:"match"`
}

type index struct {
	Name    string   `json:"name"`
	Unique  bool     `json:"unique"`
	Origin  string   `json:"origin"`
	Partial bool     `json:"partial"`
	Columns []string `json:"columns"`
}

type table struct {
	Name        string       `json:"name"`
	SQL         string       `json:"sql"`
	Columns     []column     `json:"columns"`
	ForeignKeys []foreignKey `json:"foreign_keys"`
	Indexes     []index      `json:"indexes"`
}

type object struct {
	Type string `json:"type"`
	Name string `json:"name"`
	SQL  string `json:"sql"`
}

func normSQL(s string) string { return strings.Join(strings.Fields(s), " ") }

// Hash 对实际数据库结构做稳定 hash。hash 不包含任何业务行，也不包含
// schema_meta 里的值和 sqlite_sequence 的当前值。
func Hash(db *sql.DB) (string, error) { return hashQuery(db) }

// queryer 让 *sql.DB 和 *sql.Tx 都能参与结构 hash。初始化事务需要在提交前
// 计算 hash，不能用另一个连接读取尚未提交的 DDL。
type queryer interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

func hashQuery(q queryer) (string, error) {
	rows, err := q.Query(`SELECT type,name,sql FROM sqlite_master
		WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite_%' AND name <> '_writable'
		ORDER BY type,name`)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	var tables []table
	var others []object
	type masterRow struct{ typ, name, sqlText string }
	var masters []masterRow
	for rows.Next() {
		var typ, name, sqlText string
		if err := rows.Scan(&typ, &name, &sqlText); err != nil {
			return "", err
		}
		masters = append(masters, masterRow{typ, name, sqlText})
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	rows.Close()
	// 先把 sqlite_master 读完并关闭游标，再逐表 PRAGMA。调用方可能把连接池
	// 限制为 1（离线迁移工具就是这样），在游标未关闭时再开查询会死锁。
	for _, m := range masters {
		switch m.typ {
		case "table":
			t, err := inspectTable(q, m.name, m.sqlText)
			if err != nil {
				return "", err
			}
			tables = append(tables, t)
		case "index", "trigger", "view":
			others = append(others, object{Type: m.typ, Name: m.name, SQL: normSQL(m.sqlText)})
		}
	}
	sort.Slice(tables, func(i, j int) bool { return tables[i].Name < tables[j].Name })
	sort.Slice(others, func(i, j int) bool {
		if others[i].Type != others[j].Type {
			return others[i].Type < others[j].Type
		}
		return others[i].Name < others[j].Name
	})
	payload := struct {
		Tables []table  `json:"tables"`
		Others []object `json:"others"`
	}{tables, others}
	b, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func inspectTable(q queryer, name, sqlText string) (table, error) {
	t := table{Name: name, SQL: normSQL(sqlText)}
	cr, err := q.Query(`PRAGMA table_info(` + quoteIdent(name) + `)`)
	if err != nil {
		return t, err
	}
	for cr.Next() {
		var cid, notnull, pk int
		var cname, ctype string
		var dflt sql.NullString
		if err := cr.Scan(&cid, &cname, &ctype, &notnull, &dflt, &pk); err != nil {
			cr.Close()
			return t, err
		}
		t.Columns = append(t.Columns, column{Name: cname, Type: strings.ToUpper(ctype), NotNull: notnull != 0, Default: dflt.String, PK: pk})
	}
	if err := cr.Err(); err != nil {
		cr.Close()
		return t, err
	}
	cr.Close()

	fr, err := q.Query(`PRAGMA foreign_key_list(` + quoteIdent(name) + `)`)
	if err != nil {
		return t, err
	}
	for fr.Next() {
		var id, seq int
		var parent, from, to, onUpdate, onDelete, match string
		if err := fr.Scan(&id, &seq, &parent, &from, &to, &onUpdate, &onDelete, &match); err != nil {
			fr.Close()
			return t, err
		}
		t.ForeignKeys = append(t.ForeignKeys, foreignKey{ID: id, Seq: seq, Table: parent, From: from, To: to, OnUpdate: onUpdate, OnDelete: onDelete, Match: match})
	}
	if err := fr.Err(); err != nil {
		fr.Close()
		return t, err
	}
	fr.Close()

	ir, err := q.Query(`PRAGMA index_list(` + quoteIdent(name) + `)`)
	if err != nil {
		return t, err
	}
	type idxRow struct {
		name            string
		unique, partial int
		origin          string
	}
	var idxs []idxRow
	for ir.Next() {
		var seq, unique, partial int
		var iname, origin string
		if err := ir.Scan(&seq, &iname, &unique, &origin, &partial); err != nil {
			ir.Close()
			return t, err
		}
		idxs = append(idxs, idxRow{iname, unique, partial, origin})
	}
	if err := ir.Err(); err != nil {
		ir.Close()
		return t, err
	}
	ir.Close()
	for _, r := range idxs {
		idx := index{Name: r.name, Unique: r.unique != 0, Origin: r.origin, Partial: r.partial != 0}
		xr, err := q.Query(`PRAGMA index_info(` + quoteIdent(r.name) + `)`)
		if err != nil {
			return t, err
		}
		for xr.Next() {
			var seqno, cid int
			var cname sql.NullString
			if err := xr.Scan(&seqno, &cid, &cname); err != nil {
				xr.Close()
				return t, err
			}
			idx.Columns = append(idx.Columns, cname.String)
		}
		if err := xr.Err(); err != nil {
			xr.Close()
			return t, err
		}
		xr.Close()
		t.Indexes = append(t.Indexes, idx)
	}
	sort.Slice(t.Indexes, func(i, j int) bool { return t.Indexes[i].Name < t.Indexes[j].Name })
	return t, nil
}

func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}
