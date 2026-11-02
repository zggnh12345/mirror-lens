package discover

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/store"
)

// Scan 抓取全部发现源，把库里没有的主机写进 pending 池，返回新增数量。
// 发现的只是候选：能力留空，由探测决定去留。
func Scan(ctx context.Context, st *store.Store) (int, error) {
	srcs, err := st.ListSources()
	if err != nil {
		return 0, err
	}
	known := make(map[string]bool, len(srcs))
	for _, s := range srcs {
		if h := hostOf(s.BaseURL); h != "" {
			known[h] = true
		}
	}

	// 同一域名可能同时出现在列表仓库和状态页：状态页在前，优先保留在线标记。
	var order []string
	best := map[string]candidate{}
	for _, c := range fetchAll(ctx) {
		if known[c.host] {
			continue
		}
		old, ok := best[c.host]
		if !ok {
			order = append(order, c.host)
			best[c.host] = c
			continue
		}
		if !old.online && c.online {
			best[c.host] = c
		}
	}
	sort.Strings(order)

	added := 0
	for _, h := range order {
		c := best[h]
		note := "自动发现 · " + c.origin
		if c.online {
			note += "（在线）"
		}
		_, err := st.AddSource(store.Source{
			BaseURL:   "https://" + h,
			Name:      h,
			Origin:    "discovered",
			Enabled:   true,
			Status:    "pending",
			Note:      note,
			DockerCap: store.CapUnknown,
			GithubCap: store.CapUnknown,
		})
		if err == nil {
			added++
		}
	}
	if added > 0 {
		log.Printf("discover: 新增 %d 个候选源", added)
	}
	return added, nil
}

// fetchAll 并发抓取，单个源失败只影响自己。
func fetchAll(ctx context.Context) []candidate {
	client := &http.Client{Timeout: 25 * time.Second}
	perFeed := make([][]candidate, len(feeds))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, f := range feeds {
		wg.Add(1)
		go func(i int, f feed) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			body, err := fetch(ctx, client, f)
			if err != nil {
				log.Printf("discover: %s 抓取失败: %v", f.name, err)
				return
			}
			perFeed[i] = parse(f, body)
			log.Printf("discover: %s %d 条候选", f.name, len(perFeed[i]))
		}(i, f)
	}
	wg.Wait()

	var all []candidate
	for _, c := range perFeed {
		all = append(all, c...)
	}
	return all
}

func fetch(ctx context.Context, client *http.Client, f feed) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "mirror-lens/1.0")
	for k, v := range f.header {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 2<<20))
}
