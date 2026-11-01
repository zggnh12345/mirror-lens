package metrics

import "sort"

// MetricsView 是给面板用的聚合快照，JSON 友好。只暴露封闭集合 + 聚合值，
// 不暴露 URL/IP/文件名/request_id，和 /metrics 文本格式遵守同一条约束。
type MetricsView struct {
	Enabled     bool              `json:"enabled"`
	Cache       CacheView        `json:"cache"`
	Sources     []SourceCount    `json:"sources"`
	Ratio       RatioView        `json:"ratio"`
	Heat        map[string]uint64 `json:"heat"`
	Concurrency ConcurrencyView  `json:"concurrency"`
}

// CacheView 缓存盘快照（gauge，janitor 刷新）。
type CacheView struct {
	HasCache bool   `json:"has_cache"`
	Bytes    uint64 `json:"bytes"`
	Blobs    uint64 `json:"blobs"`
	Free     uint64 `json:"free"`
}

// SourceCount 某协议/状态的源站数。
type SourceCount struct {
	Kind   string `json:"kind"`
	Status string `json:"status"`
	Count  uint64 `json:"count"`
}

// RatioView 命中率：各场景最近周期命中率 + 按请求量加权的整体命中率。
type RatioView struct {
	HasRatio bool              `json:"has_ratio"`
	Overall  float64           `json:"overall"`
	Scenes   map[string]float64 `json:"scenes"`
}

// ConcurrencyView 源站并发：当前在途回源占用 + 动态并发上限，以及占用最高的若干源。
type ConcurrencyView struct {
	Inflight uint64         `json:"inflight"`
	Limit    uint64         `json:"limit"`
	Top      []SourceConc   `json:"top"`
}

// SourceConc 单台源的并发占用与上限。
type SourceConc struct {
	ID       int64 `json:"id"`
	Inflight int   `json:"inflight"`
	Limit    int   `json:"limit"`
}

// SnapshotView 返回面板要用的聚合快照。metrics 未启用时 Enabled=false。
func SnapshotView() MetricsView {
	r := Get()
	if r == nil {
		return MetricsView{
			Enabled: false,
			Heat:    map[string]uint64{},
			Ratio:   RatioView{Scenes: map[string]float64{}},
		}
	}
	out := MetricsView{Enabled: true, Heat: map[string]uint64{}, Ratio: RatioView{Scenes: map[string]float64{}}}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.haveCache {
		out.Cache = CacheView{HasCache: true, Bytes: r.cacheBytes, Blobs: r.cacheBlobs, Free: r.cacheFree}
	}
	for k, v := range r.sources {
		out.Sources = append(out.Sources, SourceCount{Kind: k.Kind, Status: k.Status, Count: v})
	}
	sort.Slice(out.Sources, func(i, j int) bool {
		if out.Sources[i].Kind != out.Sources[j].Kind {
			return out.Sources[i].Kind < out.Sources[j].Kind
		}
		return out.Sources[i].Status < out.Sources[j].Status
	})

	if r.haveRatio {
		out.Ratio.HasRatio = true
		var num, den uint64
		for s, ratio := range r.hitRatio {
			out.Ratio.Scenes[s] = ratio
			d := r.sceneDelta[s]
			if d > 0 {
				num += uint64(ratio*float64(d) + 0.5)
				den += d
			}
		}
		if den > 0 {
			out.Ratio.Overall = float64(num) / float64(den)
		}
	}
	for s, v := range r.sceneDelta {
		out.Heat[s] = v
	}

	var inflight, limit uint64
	type kv struct {
		id       int64
		inflight int
		limit    int
	}
	var top []kv
	for id, v := range r.inflight {
		inflight += uint64(v)
		top = append(top, kv{id: id, inflight: v, limit: r.srclimit[id]})
	}
	for _, v := range r.srclimit {
		limit += uint64(v)
	}
	sort.Slice(top, func(i, j int) bool { return top[i].inflight > top[j].inflight })
	if len(top) > 6 {
		top = top[:6]
	}
	for _, t := range top {
		out.Concurrency.Top = append(out.Concurrency.Top, SourceConc{ID: t.id, Inflight: t.inflight, Limit: t.limit})
	}
	out.Concurrency.Inflight = inflight
	out.Concurrency.Limit = limit

	return out
}
