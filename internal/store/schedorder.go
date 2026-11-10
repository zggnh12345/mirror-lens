package store

import "encoding/json"

const schedOrderKey = "sched_order"

// SchedOrder 是「调度顺序」的用户设置。
// Auto 下完全按评分动态排；Manual 下每种协议的前 3 个源按 Orders 里的顺序固定，
// 其余源仍按评分自动排。
type SchedOrder struct {
	Mode   string             `json:"mode"`   // auto | manual
	Orders map[string][]int64 `json:"orders"` // docker / github 各最多 3 个 id
}

// SchedKind 把协议族归成调度可配置的两类：docker（HUB）与 github（raw/release）。
func SchedKind(kind string) string {
	if kind == "docker" || kind == "ghcr" {
		return "docker"
	}
	return "github"
}

func (s *Store) GetSchedOrder() SchedOrder {
	def := SchedOrder{Mode: "auto", Orders: map[string][]int64{}}
	raw := s.GetSetting(schedOrderKey, "")
	if raw == "" {
		return def
	}
	var o SchedOrder
	if json.Unmarshal([]byte(raw), &o) != nil {
		return def
	}
	if o.Mode != "manual" {
		o.Mode = "auto"
	}
	if o.Orders == nil {
		o.Orders = map[string][]int64{}
	}
	return o
}

func (s *Store) SetSchedOrder(o SchedOrder) error {
	if o.Mode != "manual" {
		o.Mode = "auto"
	}
	if o.Orders == nil {
		o.Orders = map[string][]int64{}
	}
	b, err := json.Marshal(o)
	if err != nil {
		return err
	}
	return s.SetSetting(schedOrderKey, string(b))
}
