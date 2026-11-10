package api

import (
	"testing"

	"github.com/zggnh12345/mirror-lens/internal/store"
)

// 面板列表：★源整体最前，星源内部按综合分降序，其余也降序，没分的沉底。
func TestSortByScore(t *testing.T) {
	list := []store.Source{
		{ID: 1, Name: "a-normal-high", Preferred: false},
		{ID: 2, Name: "b-star-low", Preferred: true},
		{ID: 3, Name: "c-dead", Preferred: false},
		{ID: 4, Name: "d-star-high", Preferred: true},
		{ID: 5, Name: "e-normal-low", Preferred: false},
	}
	scores := map[int64]float64{1: 0.8, 2: 0.3, 4: 0.9, 5: 0.1}

	sortByScore(list, scores)

	want := []int64{4, 2, 1, 5, 3}
	for i := range want {
		if list[i].ID != want[i] {
			got := make([]int64, len(list))
			for j := range list {
				got[j] = list[j].ID
			}
			t.Fatalf("顺序应为 %v，实际 %v", want, got)
		}
	}
}
