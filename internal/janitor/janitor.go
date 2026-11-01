// Package janitor 定时收拾缓存：容量、过期条目、半截文件、临时文件、
// 索引与磁盘对账。所有删除都走 gc，不在这里直接删文件。
package janitor

import (
	"context"
	"log"
	"sync"

	"github.com/zggnh12345/mirror-lens/internal/metrics"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/cache"
	"github.com/zggnh12345/mirror-lens/internal/gc"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

const (
	metaWarnKey = "cache_warned"
	tmpTTL      = time.Hour // 临时文件固定留 1 小时，不单独做配置项
)

type Options struct {
	Interval      time.Duration
	PartialTTL    time.Duration
	ExpiredGrace  time.Duration
	CacheMaxBytes int64
	WarnAt        float64
	RecoverAt     float64
	// Evict 由 gateway 提供：容量淘汰和写后触发的淘汰必须是同一套逻辑。
	Evict func(limit int64) (int64, error)
}

type Stats struct {
	At       int64 `json:"at"`
	Usage    int64 `json:"usage"`
	Limit    int64 `json:"limit"`
	Evicted  int64 `json:"evicted_bytes"`
	Expired  int   `json:"expired_entries"`
	Missing  int   `json:"missing_entries"`
	Orphans  int   `json:"orphan_blobs"`
	Partials int   `json:"partials"`
	TmpFiles int   `json:"tmp_files"`
	Freed    int64 `json:"freed_bytes"`
	Pending  int   `json:"pending"`
	Warned   bool  `json:"warned"`
}

type Janitor struct {
	gc  *gc.GC
	st  *store.Store
	cs  *cache.Store
	opt Options
	now func() time.Time

	mu      sync.Mutex
	warned  bool
	last    Stats
	lastRun time.Time
}

func New(st *store.Store, cs *cache.Store, g *gc.GC, opt Options) *Janitor {
	j := &Janitor{gc: g, st: st, cs: cs, opt: opt, now: time.Now}
	// 告警状态落库：重启后不该因为"内存里没记住"再刷一条
	if v, err := st.MetaGet(metaWarnKey); err == nil {
		j.warned = v == "1"
	}
	return j
}

func (j *Janitor) Last() Stats {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.last
}

// Run 按间隔巡检，ctx 取消就退出。启动先跑一轮，别等十分钟。
func (j *Janitor) Run(ctx context.Context) {
	j.Once()
	t := time.NewTicker(j.opt.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			j.Once()
		}
	}
}

// Once 跑一轮，返回这轮的数字。失败的项目记日志后继续，不让一处出错停掉整轮。
func (j *Janitor) Once() Stats {
	now := j.now()
	s := Stats{At: now.Unix(), Limit: j.opt.CacheMaxBytes}

	// 1. 临时文件：下载中断留下的，1 小时足够久
	if n, err := j.cs.SweepTemp(now.Add(-tmpTTL).Unix()); err != nil {
		log.Printf("janitor: 清临时文件失败: %v", err)
	} else {
		s.TmpFiles = n
	}

	// 2. 半截文件：超过 partial_ttl 没人续传，且没被任何进程持有
	if j.opt.PartialTTL > 0 {
		n, freed, err := j.cs.SweepPartial(now.Add(-j.opt.PartialTTL).Unix())
		if err != nil {
			log.Printf("janitor: 清半截文件失败: %v", err)
		}
		s.Partials = n
		s.Freed += freed
	}

	// 3. 过期条目：过期且超过宽限期没人访问。只过期不算数——
	//    低频资源删了就得整份重下。
	if j.opt.ExpiredGrace > 0 {
		olds, err := j.st.CacheExpired(now.Unix(), int64(j.opt.ExpiredGrace.Seconds()), 500)
		if err != nil {
			log.Printf("janitor: 查过期条目失败: %v", err)
		} else if len(olds) > 0 {
			res, err := j.gc.DeleteKeys(keysOf(olds))
			if err != nil {
				log.Printf("janitor: 删过期条目失败: %v", err)
			}
			s.Expired = res.Entries
			s.Freed += res.Bytes
			s.Pending += res.Pending
		}
	}

	// 4. 对账：索引在、文件不在 → 删索引（否则用户命中后 502）
	all, err := j.st.CacheAll(0)
	if err != nil {
		log.Printf("janitor: 读索引失败: %v", err)
	} else if miss := j.gc.MissingFiles(all); len(miss) > 0 {
		if res, err := j.gc.DeleteKeys(miss); err == nil {
			s.Missing = res.Entries
			log.Printf("janitor: %d 条索引指向的文件已不存在，已删索引", res.Entries)
		}
	}

	// 5. 对账：文件在、没有索引引用 → 删文件
	if res, err := j.gc.Orphans(); err != nil {
		log.Printf("janitor: 清孤儿文件失败: %v", err)
	} else {
		s.Orphans = res.Blobs
		s.Freed += res.Bytes
		s.Pending += res.Pending
	}

	// 6. 上轮删不掉的文件重试
	if n, freed := j.gc.RetryPending(); n > 0 {
		s.Freed += freed
		log.Printf("janitor: 补删 %d 个文件，释放 %d MB", n, freed>>20)
	}

	// 7. 容量：写后淘汰之外，定时也压一次，防止"没人写就永远不淘汰"
	if j.opt.CacheMaxBytes > 0 && j.opt.Evict != nil {
		if used, err := j.cs.TotalSize(); err == nil && used > j.opt.CacheMaxBytes {
			if freed, err := j.opt.Evict(j.opt.CacheMaxBytes); err != nil {
				log.Printf("janitor: 容量淘汰失败: %v", err)
			} else {
				s.Evicted = freed
				s.Freed += freed
			}
		}
	}

	// 8. 水位：按磁盘实际占用算，不是索引里的 size 之和
	if used, err := j.cs.DiskUsage(); err == nil {
		s.Usage = used
		j.checkWatermark(used, &s)
	}
	s.Pending += j.pendingCount()

	j.mu.Lock()
	j.last = s
	j.lastRun = now
	j.mu.Unlock()

	// 9. 把缓存/源站快照推给 metrics：/metrics 直接读这里，
	// 不每次抓取都递归扫整盘（百万文件时抓取会卡死）。
	if r := metrics.Get(); r != nil {
		blobs := uint64(0)
		if bl, err := j.cs.ListBlobs(); err == nil {
			blobs = uint64(len(bl))
		}
		free := int64(0)
		if f, err := j.cs.FSFree(); err == nil {
			free = f
		}
		metrics.SetCache(uint64(s.Usage), blobs, uint64(free))
		if srcs, err := j.st.ListSources(); err == nil {
			m := make(map[metrics.SourceStat]uint64, len(srcs)*2)
			for _, s2 := range srcs {
				for _, k := range s2.Kinds() {
					m[metrics.SourceStat{Kind: k, Status: s2.Status}]++
				}
			}
			metrics.SetSources(m)
		}
		metrics.SnapshotHitRatio()
	}

	if s.Expired+s.Missing+s.Orphans+s.Partials+s.TmpFiles > 0 || s.Freed > 0 {
		log.Printf("janitor: 过期 %d、缺文件 %d、孤儿 %d、半截 %d、临时 %d，释放 %d MB",
			s.Expired, s.Missing, s.Orphans, s.Partials, s.TmpFiles, s.Freed>>20)
	}
	return s
}

// checkWatermark 只在跨过阈值时动一次：90% 报一次，回落到 85% 以下才重新武装。
// 状态存在 meta 表里，重启后不会重复刷同一条日志。
func (j *Janitor) checkWatermark(used int64, s *Stats) {
	limit := j.opt.CacheMaxBytes
	if limit <= 0 {
		return
	}
	warnAt, recoverAt := j.opt.WarnAt, j.opt.RecoverAt
	if warnAt <= 0 || warnAt >= 1 {
		warnAt = 0.9
	}
	if recoverAt <= 0 || recoverAt >= warnAt {
		recoverAt = 0.85
	}
	ratio := float64(used) / float64(limit)

	j.mu.Lock()
	warned := j.warned
	if !warned && ratio >= warnAt {
		j.warned = true
	} else if warned && ratio <= recoverAt {
		j.warned = false
	}
	changed := j.warned != warned
	j.mu.Unlock()

	if changed {
		v := "0"
		if j.warned {
			v = "1"
			log.Printf("缓存用量 %.0f%%（%d MB / %d MB），超过告警水位 %.0f%%",
				ratio*100, used>>20, limit>>20, warnAt*100)
		}
		_ = j.st.MetaSet(metaWarnKey, v)
	}
	s.Warned = j.warned
}

func (j *Janitor) pendingCount() int {
	shas, err := j.st.PendingBlobs(0)
	if err != nil {
		return 0
	}
	return len(shas)
}

func keysOf(es []store.CacheEntry) []string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.Key)
	}
	return out
}
