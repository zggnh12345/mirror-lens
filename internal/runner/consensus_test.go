package runner

import "testing"

// 平票和「只有两票但存在第三种说法」都不能算共识：
// 这种时候宁可不写缓存，也不要钉死一份可能是错的内容。
func TestConsensusRequiresStrictMajority(t *testing.T) {
	items := func(ds ...string) []string { return ds }
	id := func(s string) string { return s }

	cases := []struct {
		in   []string
		want string
	}{
		{items("a", "a", "a", "a"), "a"}, // 4:0
		{items("a", "a", "a", "b"), "a"}, // 3:1
		{items("a", "a", "b"), "a"},      // 2:1 过半
		{items("a", "b"), ""},            // 1:1 平票
		{items("a", "a", "b", "b"), ""},  // 2:2 平票
		{items("a", "a", "b", "c"), ""},  // 2/4 不过半
		{items("a", "", ""), ""},         // 空票不计入分母，但 1 票不够
		{items("a", "a", ""), "a"},       // 2:0
		{items(), ""},                    // 没票
	}
	for _, c := range cases {
		if got := consensus(c.in, id); got != c.want {
			t.Fatalf("%v => %q，期望 %q", c.in, got, c.want)
		}
	}
}
