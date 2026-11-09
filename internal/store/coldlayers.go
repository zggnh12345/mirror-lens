package store

// 冷层统计：只回答「有多少字节是长期没人再取用的 layer」，
// 不做引用图，也不参与淘汰决策。用来判断「旧 latest 的差异层」
// 是否已经占到大头，再决定要不要上 manifest→layer 引用表。

// ColdLayerStats 统计 docker:blob:* 条目里 last_hit 早于 cutoff 的部分。
// 从未命中的条目 last_hit=0，同样算冷——它们本来就该被优先淘汰。
// 返回：冷字节、总字节、冷条目数、总条目数。
func (s *Store) ColdLayerStats(cutoff int64) (coldBytes, totalBytes, coldCount, totalCount int64, err error) {
	err = s.db.QueryRow(`
		SELECT
			COALESCE(SUM(CASE WHEN last_hit < ? THEN size ELSE 0 END), 0),
			COALESCE(SUM(size), 0),
			COALESCE(SUM(CASE WHEN last_hit < ? THEN 1 ELSE 0 END), 0),
			COUNT(*)
		FROM cache_entries
		WHERE key LIKE 'docker:blob:%'`, cutoff, cutoff).
		Scan(&coldBytes, &totalBytes, &coldCount, &totalCount)
	return
}
