package store

import (
	"time"
)

// 可信度分级：只看实测记录，不看源站自己怎么宣传。
//
// 一台源的「能不能用」和「敢不敢用」是两件事。能下 200MB/s 但返回过错误
// 字节的源，比一台慢源危险得多——它会把坏内容塞进缓存，再被所有人信任地
// 取走。所以级别由校验结果决定，跟速度、可用率无关。
const (
	TrustPoisoned  = -1 // 7 天内出现过内容不符（manifest 或字节）
	TrustUnknown   = 0  // 7 天内没有一次 tier>=2 的成功检测
	TrustReachable = 1  // 能用，但内容从没被完整校验过
	TrustVerified  = 2  // 有校验通过的记录，且没有不符
	TrustProven    = 3  // 近 7 天 ≥5 次校验通过，且无任何不符
	// 限流（429）不进这一列：它说的是「对方让我们慢点」，不是「内容有问题」。
)

// Trust 是一台源在某类资源上的可信度证据。
type Trust struct {
	Level int `json:"level"`
	// 近 7 天的窗口：成功检测次数、内容校验通过次数、不符次数
	OK7     int `json:"ok7"`
	Digest7 int `json:"digest7"`
	Bad7    int `json:"bad7"`
}

const (
	trustWindow   = 7 * 24 * time.Hour
	trustProvenOK = 5 // 判「长期一致」要的校验通过次数
)

// trustMap 一次算出某个类型下所有源的可信度。
func (s *Store) trustMap(kind string, now time.Time) (map[int64]Trust, error) {
	since := now.Add(-trustWindow).Unix()
	rows, err := s.db.Query(`SELECT source_id,
		SUM(CASE WHEN ok=1 AND tier>=2 THEN 1 ELSE 0 END),
		SUM(CASE WHEN digest_ok=1 THEN 1 ELSE 0 END),
		SUM(CASE WHEN digest_ok=0 OR blob_ok=0 THEN 1 ELSE 0 END)
		FROM checks WHERE ts>=? AND kind=? GROUP BY source_id`, since, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]Trust{}
	for rows.Next() {
		var id int64
		var t Trust
		if err := rows.Scan(&id, &t.OK7, &t.Digest7, &t.Bad7); err != nil {
			return nil, err
		}
		t.Level = trustLevel(t)
		out[id] = t
	}
	return out, rows.Err()
}

func trustLevel(t Trust) int {
	switch {
	case t.Bad7 > 0:
		return TrustPoisoned
	case t.OK7 == 0:
		return TrustUnknown
	case t.Digest7 == 0:
		return TrustReachable
	case t.OK7 >= trustProvenOK && t.Digest7 >= trustProvenOK:
		return TrustProven
	default:
		return TrustVerified
	}
}

// TrustLabel 是给界面看的一句话，避免前端自己拼级别。
func TrustLabel(level int) string {
	switch level {
	case TrustPoisoned:
		return "内容不符"
	case TrustUnknown:
		return "未验证"
	case TrustReachable:
		return "仅可用"
	case TrustVerified:
		return "内容一致"
	case TrustProven:
		return "长期一致"
	}
	return "未知"
}
