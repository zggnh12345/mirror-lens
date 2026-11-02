package config

import (
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

type durBox struct {
	D Duration `yaml:"d"`
}

// 300abc 必须报错。以前用 Sscanf("%d")，前缀读完就算成功，
// 写错一个字母配置照样启动，时间和预期差得离谱。
func TestDurationStrict(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
		bad  bool
	}{
		{"300", 300 * time.Second, false},
		{" 300 ", 300 * time.Second, false},
		{"5m", 5 * time.Minute, false},
		{"1h30m", 90 * time.Minute, false},
		{"300abc", 0, true},
		{"-5", 0, true},
		{"5 m", 0, true},
	}
	for _, c := range cases {
		var b durBox
		err := yaml.Unmarshal([]byte("d: "+c.in+"\n"), &b)
		if c.bad {
			if err == nil {
				t.Fatalf("%q 应该报错，实际通过 = %v", c.in, b.D)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%q 不该报错: %v", c.in, err)
		}
		if b.D.Duration != c.want {
			t.Fatalf("%q = %v，期望 %v", c.in, b.D.Duration, c.want)
		}
	}
}
