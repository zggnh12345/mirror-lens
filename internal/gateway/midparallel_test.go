package gateway

import "testing"

func TestMidChunks(t *testing.T) {
	cases := []struct {
		rest, chunk int64
		want        int
	}{
		{0, 1 << 20, 0},
		{1, 1 << 20, 1},
		{1 << 20, 1 << 20, 1},
		{1<<20 + 1, 1 << 20, 2},
		{86 << 20, 4 << 20, 22},
		{112 << 20, 1 << 20, 112},
	}
	for _, c := range cases {
		if got := midChunks(c.rest, c.chunk); got != c.want {
			t.Errorf("midChunks(%d,%d)=%d want %d", c.rest, c.chunk, got, c.want)
		}
	}
}
