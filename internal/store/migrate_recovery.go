package store

import (
	"context"
	"database/sql"
	"sort"
	"strings"
)

type rowQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// pkColumns 返回按主键序号排列的列名，例如 "source_id,family,scope,gen"。
func pkColumns(ctx context.Context, q rowQuerier, table string) (map[string]bool, string, error) {
	rows, err := q.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()

	cols := map[string]bool{}
	type pkCol struct {
		seq  int
		name string
	}
	var pks []pkCol
	for rows.Next() {
		var (
			cid     int
			name    string
			typ     string
			notnull int
			dflt    sql.NullString
			pk      int
		)
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return nil, "", err
		}
		cols[name] = true
		if pk > 0 {
			pks = append(pks, pkCol{pk, name})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	sort.Slice(pks, func(i, j int) bool { return pks[i].seq < pks[j].seq })
	names := make([]string, len(pks))
	for i, c := range pks {
		names[i] = c.name
	}
	return cols, strings.Join(names, ","), nil
}
