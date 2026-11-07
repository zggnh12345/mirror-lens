package config

import (
	"strings"
	"testing"
	"time"
)

func TestRelayDefaults(t *testing.T) {
	c := Default()
	if c.Relay.PinGraceHours != 24 {
		t.Fatalf("pin_grace_hours 默认应为 24，实际 %d", c.Relay.PinGraceHours)
	}
	if c.Relay.ResolverTimeout.Duration != 5*time.Second {
		t.Fatalf("resolver_timeout 默认应为 5s，实际 %s", c.Relay.ResolverTimeout.Duration)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("默认配置应通过校验: %v", err)
	}
}

func TestRelayRejectsNegative(t *testing.T) {
	c := Default()
	c.Relay.PinGraceHours = -1
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "pin_grace_hours") {
		t.Fatalf("负数窗口应被拒绝，实际 %v", err)
	}

	c = Default()
	c.Relay.ResolverTimeout.Duration = -time.Second
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "resolver_timeout") {
		t.Fatalf("负数超时应被拒绝，实际 %v", err)
	}
}
