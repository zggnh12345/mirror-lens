//go:build s1inv

// S1 只读盘点工具。不是业务代码，不进入正常构建（默认无 s1inv tag）。
// 通过 go test -overlay 映射为 internal/store 的临时测试文件，以便直接调用
// 未导出的 Store 与 SourceCandidates，而不修改 internal/。
package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"sort"
	"testing"
	"time"
)

func s1Hash(t *testing.T, p string) string {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatalf("open %s: %v", p, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatalf("hash %s: %v", p, err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

type s1Cand struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	BaseURL string `json:"base_url"`
	Status  string `json:"status"`
}

type s1Row struct {
	ID              int64  `json:"id"`
	Name            string `json:"name"`
	BaseURL         string `json:"base_url"`
	RepoHost        string `json:"repo_host"`
	Origin          string `json:"origin"`
	Status          string `json:"status"`
	Enabled         bool   `json:"enabled"`
	DockerCap       int    `json:"docker_cap"`
	GhcrCap         int    `json:"ghcr_cap"`
	GithubCap       int    `json:"github_cap"`
	GitCap          int    `json:"git_cap"`
	PlannedIdentity string `json:"planned_identity"`
}

type s1Out struct {
	QueryTimeUTC string         `json:"query_time_utc"`
	DBPath       string         `json:"db_path"`
	DBSize       int64          `json:"db_size"`
	DBSHA256     string         `json:"db_sha256_before"`
	DBSHA256End  string         `json:"db_sha256_after"`
	Total        int            `json:"total_sources"`
	EmptyHost    int            `json:"empty_host"`
	Docker       []s1Cand       `json:"docker_candidates"`
	DockerCount  int            `json:"docker_count"`
	Ghcr         []s1Cand       `json:"ghcr_candidates"`
	GhcrCount    int            `json:"ghcr_count"`
	StatusCount  map[string]int `json:"status_count"`
	EnabledCount map[string]int `json:"enabled_count"`
	GroupStatus  map[string]int `json:"group_status"`
	Rows         []s1Row        `json:"rows"`
}

func TestS1ReadOnlyBaseline(t *testing.T) {
	path := os.Getenv("S1_DB")
	if path == "" {
		t.Fatal("S1_DB 未设置")
	}
	before := s1Hash(t, path)
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()

	s := &Store{db: db, clock: time.Now}

	toCands := func(stats []SourceStat) []s1Cand {
		out := make([]s1Cand, 0, len(stats))
		for _, x := range stats {
			out = append(out, s1Cand{ID: x.ID, Name: x.Name, BaseURL: x.BaseURL, Status: x.Status})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
		return out
	}

	docker, err := s.SourceCandidates("docker")
	if err != nil {
		t.Fatalf("SourceCandidates(docker): %v", err)
	}
	ghcr, err := s.SourceCandidates("ghcr")
	if err != nil {
		t.Fatalf("SourceCandidates(ghcr): %v", err)
	}

	rows, err := db.Query(`SELECT id, name, base_url, repo_host, origin, status, enabled,
		docker_cap, ghcr_cap, github_cap, git_cap,
		CASE WHEN repo_host<>'' THEN 'identified'
		     WHEN docker_cap=1 THEN 'identified'
		     WHEN github_cap=1 THEN 'not_applicable'
		     ELSE 'unclassified' END
		FROM sources ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	out := s1Out{
		QueryTimeUTC: time.Now().UTC().Format(time.RFC3339),
		DBPath:       path,
		DBSize:       st.Size(),
		DBSHA256:     before,
		Docker:       toCands(docker),
		DockerCount:  len(docker),
		Ghcr:         toCands(ghcr),
		GhcrCount:    len(ghcr),
		StatusCount:  map[string]int{},
		EnabledCount: map[string]int{},
		GroupStatus:  map[string]int{},
	}
	for rows.Next() {
		var r s1Row
		var enabled int
		if err := rows.Scan(&r.ID, &r.Name, &r.BaseURL, &r.RepoHost, &r.Origin, &r.Status, &enabled,
			&r.DockerCap, &r.GhcrCap, &r.GithubCap, &r.GitCap, &r.PlannedIdentity); err != nil {
			t.Fatal(err)
		}
		r.Enabled = enabled == 1
		if r.RepoHost == "" {
			out.EmptyHost++
			out.StatusCount[r.Status]++
			if r.Enabled {
				out.EnabledCount["enabled"]++
			} else {
				out.EnabledCount["disabled"]++
			}
			out.GroupStatus[r.PlannedIdentity+"/"+r.Status]++
		}
		out.Rows = append(out.Rows, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	out.Total = len(out.Rows)

	after := s1Hash(t, path)
	out.DBSHA256End = after
	if before != after {
		t.Fatalf("数据库在盘点期间被修改: %s -> %s", before, after)
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if outPath := os.Getenv("S1_OUT"); outPath != "" {
		f, err := os.Create(outPath)
		if err != nil {
			t.Fatalf("创建 %s: %v", outPath, err)
		}
		enc = json.NewEncoder(f)
		enc.SetIndent("", "  ")
		defer f.Close()
	}
	if err := enc.Encode(out); err != nil {
		t.Fatal(err)
	}
}
