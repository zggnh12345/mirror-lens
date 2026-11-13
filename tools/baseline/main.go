// Command baseline 在生产库的只读副本上重放 3.5.4 迁移并对比候选池。
//
// 用法：baseline -db /path/to/copy.db > baseline.json
// 只应指向副本；程序会执行迁移（写库），不会连接生产。
package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"log"
	"os"
	"sort"

	_ "modernc.org/sqlite"

	"github.com/zggnh12345/mirror-lens/internal/store"
)

type result struct {
	Pre        snapshot            `json:"pre"`
	Post       snapshot            `json:"post"`
	Candidates map[string][]int64  `json:"candidates_post"`
	PreCand    map[string][]int64  `json:"candidates_pre"`
	Diff       map[string]diffInfo `json:"candidate_diff"`
	Overlap    map[string]int      `json:"classification_overlap"`
}

type snapshot struct {
	Total      int              `json:"total"`
	Status     map[string]int   `json:"status"`
	Hosts      map[string]int   `json:"hosts"`
	Identity   map[string]int   `json:"identity"`
	DeadIDs    []int64          `json:"dead_ids"`
	PendingIDs []int64          `json:"pending_ids"`
	UsableIDs  []int64          `json:"usable_ids"`
	DockerCap  []int64          `json:"docker_cap_ids"`
	GHCRCap    []int64          `json:"ghcr_cap_ids"`
	GitHubCap  []int64          `json:"github_cap_ids"`
	Registry   []int64          `json:"registry_cap_ids"`
	Special    []map[string]any `json:"special_114_115"`
}

type diffInfo struct {
	Added   []int64 `json:"added"`
	Removed []int64 `json:"removed"`
}

func main() {
	path := flag.String("db", "", "SQLite 副本路径")
	flag.Parse()
	if *path == "" {
		log.Fatal("-db 必填")
	}
	pre, preCand, err := readRaw(*path)
	if err != nil {
		log.Fatal(err)
	}
	st, err := store.Open(*path)
	if err != nil {
		log.Fatalf("迁移副本失败: %v", err)
	}
	defer st.Close()

	post, err := readStore(*path)
	if err != nil {
		log.Fatal(err)
	}
	postCand := map[string][]int64{}
	for _, kind := range []string{"docker", "ghcr"} {
		rows, err := st.SourceCandidates(kind)
		if err != nil {
			log.Fatal(err)
		}
		postCand[kind] = ids(rows)
	}
	hosts := map[string]bool{}
	for h := range post.Hosts {
		if h != "" && h != "docker.io" && h != "ghcr.io" {
			hosts[h] = true
		}
	}
	for h := range hosts {
		rows, err := st.SourceCandidates("registry:" + h)
		if err != nil {
			log.Fatal(err)
		}
		postCand["registry:"+h] = ids(rows)
	}
	for _, h := range []string{"registry:docker.io", "registry:ghcr.io"} {
		rows, err := st.SourceCandidates(h)
		if err != nil {
			log.Fatal(err)
		}
		postCand[h] = ids(rows)
	}

	res := result{Pre: pre, Post: post, Candidates: postCand, PreCand: preCand,
		Diff: map[string]diffInfo{}, Overlap: overlap(pre)}
	for k, postIDs := range postCand {
		res.Diff[k] = diffInfo{
			Added:   minus(postIDs, preCand[k]),
			Removed: minus(preCand[k], postIDs),
		}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(res); err != nil {
		log.Fatal(err)
	}
}

// readRaw 在迁移前按 3.5.3 的查询形状取候选。身份列此时可能还不存在。
func readRaw(path string) (snapshot, map[string][]int64, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return snapshot{}, nil, err
	}
	defer db.Close()
	s := snapshot{Status: map[string]int{}, Hosts: map[string]int{}, Identity: map[string]int{}}
	rows, err := db.Query(`SELECT id,status,enabled,repo_host,docker_cap,ghcr_cap,github_cap,registry_cap,ghcr_prefix,COALESCE(registry_identity_state,'') FROM sources ORDER BY id`)
	if err != nil {
		// 老库无身份列，退回不带该列的查询。
		rows, err = db.Query(`SELECT id,status,enabled,repo_host,docker_cap,ghcr_cap,github_cap,registry_cap,ghcr_prefix,'' FROM sources ORDER BY id`)
	}
	if err != nil {
		return s, nil, err
	}
	for rows.Next() {
		var id int64
		var status, host, identity string
		var enabled, docker, ghcr, github, registry, prefix int
		if err := rows.Scan(&id, &status, &enabled, &host, &docker, &ghcr, &github, &registry, &prefix, &identity); err != nil {
			return s, nil, err
		}
		s.Total++
		s.Status[status]++
		s.Hosts[host]++
		if identity != "" {
			s.Identity[identity]++
		}
		switch status {
		case "dead":
			s.DeadIDs = append(s.DeadIDs, id)
		case "pending":
			s.PendingIDs = append(s.PendingIDs, id)
		case "usable":
			s.UsableIDs = append(s.UsableIDs, id)
		}
		if docker == 1 {
			s.DockerCap = append(s.DockerCap, id)
		}
		if ghcr == 1 {
			s.GHCRCap = append(s.GHCRCap, id)
		}
		if github == 1 {
			s.GitHubCap = append(s.GitHubCap, id)
		}
		if registry == 1 {
			s.Registry = append(s.Registry, id)
		}
		if id == 114 || id == 115 {
			s.Special = append(s.Special, map[string]any{"id": id, "host": host, "docker_cap": docker,
				"ghcr_cap": ghcr, "ghcr_prefix": prefix, "registry_cap": registry, "identity": identity, "status": status})
		}
	}
	rows.Close()

	cand := map[string][]int64{}
	for _, kind := range []string{"docker", "ghcr"} {
		col := kind + "_cap"
		q := `SELECT s.id FROM sources s
			WHERE s.enabled=1 AND s.status IN ('usable','degraded') AND s.` + col + `=1
			  AND s.id NOT IN (SELECT source_id FROM source_health WHERE family=? AND fails>=?)
			ORDER BY s.id`
		rr, err := db.Query(q, kind, 3)
		if err != nil {
			return s, nil, err
		}
		for rr.Next() {
			var id int64
			if err := rr.Scan(&id); err != nil {
				return s, nil, err
			}
			cand[kind] = append(cand[kind], id)
		}
		rr.Close()
	}
	// 迁移前没有身份语义，foreign Registry 候选按设计为 0。
	cand["registry:docker.io"] = []int64{}
	cand["registry:ghcr.io"] = []int64{}
	return s, cand, nil
}

func readStore(path string) (snapshot, error) {
	var s snapshot
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return s, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT id,status,repo_host,docker_cap,ghcr_cap,github_cap,ghcr_prefix,registry_cap,COALESCE(registry_identity_state,'') FROM sources ORDER BY id`)
	if err != nil {
		return s, err
	}
	defer rows.Close()
	s.Status = map[string]int{}
	s.Hosts = map[string]int{}
	s.Identity = map[string]int{}
	for rows.Next() {
		var id int64
		var status, host, identity string
		var docker, ghcr, github, prefix, registry int
		if err := rows.Scan(&id, &status, &host, &docker, &ghcr, &github, &prefix, &registry, &identity); err != nil {
			return s, err
		}
		s.Total++
		s.Status[status]++
		s.Hosts[host]++
		s.Identity[identity]++
		switch status {
		case "dead":
			s.DeadIDs = append(s.DeadIDs, id)
		case "pending":
			s.PendingIDs = append(s.PendingIDs, id)
		case "usable":
			s.UsableIDs = append(s.UsableIDs, id)
		}
		if docker == 1 {
			s.DockerCap = append(s.DockerCap, id)
		}
		if ghcr == 1 {
			s.GHCRCap = append(s.GHCRCap, id)
		}
		if github == 1 {
			s.GitHubCap = append(s.GitHubCap, id)
		}
		if registry == 1 {
			s.Registry = append(s.Registry, id)
		}
		if id == 114 || id == 115 {
			s.Special = append(s.Special, map[string]any{"id": id, "host": host, "docker_cap": docker,
				"ghcr_cap": ghcr, "ghcr_prefix": prefix, "registry_cap": registry, "identity": identity, "status": status})
		}
	}
	return s, rows.Err()
}

func ids(rows []store.SourceStat) []int64 {
	out := make([]int64, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ID)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func minus(a, b []int64) []int64 {
	set := map[int64]bool{}
	for _, v := range b {
		set[v] = true
	}
	var out []int64
	for _, v := range a {
		if !set[v] {
			out = append(out, v)
		}
	}
	if out == nil {
		out = []int64{}
	}
	return out
}

func overlap(s snapshot) map[string]int {
	in := func(set []int64, id int64) bool {
		for _, v := range set {
			if v == id {
				return true
			}
		}
		return false
	}
	var deadDocker, deadGHCR, deadGitHub, deadPending, pendingDocker int
	for _, id := range s.DeadIDs {
		if in(s.DockerCap, id) {
			deadDocker++
		}
		if in(s.GHCRCap, id) {
			deadGHCR++
		}
		if in(s.GitHubCap, id) {
			deadGitHub++
		}
		if in(s.PendingIDs, id) {
			deadPending++
		}
	}
	for _, id := range s.PendingIDs {
		if in(s.DockerCap, id) {
			pendingDocker++
		}
	}
	return map[string]int{
		"dead": len(s.DeadIDs), "pending": len(s.PendingIDs), "usable": len(s.UsableIDs),
		"docker_cap": len(s.DockerCap), "ghcr_cap": len(s.GHCRCap), "github_cap": len(s.GitHubCap),
		"dead_and_docker": deadDocker, "dead_and_ghcr": deadGHCR, "dead_and_github": deadGitHub,
		"dead_and_pending": deadPending, "pending_and_docker": pendingDocker,
	}
}
