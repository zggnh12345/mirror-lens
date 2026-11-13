// 3.5.4 S2 迁移演练工具：在数据库副本上执行真实迁移（store.Open），
// 输出身份分布、空 Host 分类与两个旧候选池的完整 ID。
// 只允许对副本运行；工具不接触生产库。
package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"

	"github.com/zggnh12345/mirror-lens/internal/store"
	_ "modernc.org/sqlite"
)

type report struct {
	DBPath    string  `json:"db_path"`
	DockerIDs []int64 `json:"docker_ids"`
	GhcrIDs   []int64 `json:"ghcr_ids"`
	// 铁律：Hub 不得因迁移变成 foreign Registry 候选。
	DockerIOHostCands int            `json:"registry_candidates_docker_io"`
	KindDockerIOCands int            `json:"source_candidates_registry_docker_io"`
	IdentityAll       map[string]int `json:"identity_all"`
	IdentityEmpty     map[string]int `json:"identity_empty_host"`
	Rows              []row          `json:"rows"`
}

type row struct {
	ID       int64  `json:"id"`
	Host     string `json:"repo_host"`
	Identity string `json:"registry_identity_state"`
	Docker   int    `json:"docker_cap"`
	Ghcr     int    `json:"ghcr_cap"`
	Github   int    `json:"github_cap"`
	Status   string `json:"status"`
	Enabled  int    `json:"enabled"`
}

func main() {
	dbPath := flag.String("db", "", "数据库副本路径")
	outPath := flag.String("out", "", "输出 JSON 路径（空则写 stdout）")
	flag.Parse()
	if *dbPath == "" {
		fmt.Fprintln(os.Stderr, "必须指定 -db")
		os.Exit(2)
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "打开/迁移失败:", err)
		os.Exit(1)
	}
	defer st.Close()

	rep := report{DBPath: *dbPath,
		IdentityAll:   map[string]int{},
		IdentityEmpty: map[string]int{}}

	hubCands, err := st.RegistryCandidatesRaw("docker.io")
	if err != nil {
		fmt.Fprintln(os.Stderr, "docker.io 候选:", err)
		os.Exit(1)
	}
	rep.DockerIOHostCands = len(hubCands)
	kindCands, err := st.SourceCandidates("registry:docker.io")
	if err != nil {
		fmt.Fprintln(os.Stderr, "registry:docker.io 候选:", err)
		os.Exit(1)
	}
	rep.KindDockerIOCands = len(kindCands)

	for _, fam := range []string{"docker", "ghcr"} {
		cands, err := st.SourceCandidates(fam)
		if err != nil {
			fmt.Fprintf(os.Stderr, "候选 %s: %v\n", fam, err)
			os.Exit(1)
		}
		ids := make([]int64, 0, len(cands))
		for _, c := range cands {
			ids = append(ids, c.ID)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		if fam == "docker" {
			rep.DockerIDs = ids
		} else {
			rep.GhcrIDs = ids
		}
	}

	raw, err := sql.Open("sqlite", *dbPath+"?mode=ro")
	if err != nil {
		fmt.Fprintln(os.Stderr, "只读连接:", err)
		os.Exit(1)
	}
	defer raw.Close()
	rows, err := raw.Query(`SELECT id, repo_host, registry_identity_state,
		docker_cap, ghcr_cap, github_cap, status, enabled FROM sources ORDER BY id`)
	if err != nil {
		fmt.Fprintln(os.Stderr, "查询:", err)
		os.Exit(1)
	}
	defer rows.Close()
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.ID, &r.Host, &r.Identity, &r.Docker, &r.Ghcr, &r.Github, &r.Status, &r.Enabled); err != nil {
			fmt.Fprintln(os.Stderr, "扫描:", err)
			os.Exit(1)
		}
		rep.IdentityAll[r.Identity]++
		if r.Host == "" {
			rep.IdentityEmpty[r.Identity]++
		}
		rep.Rows = append(rep.Rows, r)
	}
	if err := rows.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "遍历:", err)
		os.Exit(1)
	}

	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "编码:", err)
		os.Exit(1)
	}
	if *outPath == "" {
		fmt.Println(string(data))
		return
	}
	if err := os.WriteFile(*outPath, append(data, '\n'), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "写文件:", err)
		os.Exit(1)
	}
	fmt.Printf("已写入 %s：docker=%d ghcr=%d identity=%v empty_host=%v\n",
		*outPath, len(rep.DockerIDs), len(rep.GhcrIDs), rep.IdentityAll, rep.IdentityEmpty)
}
