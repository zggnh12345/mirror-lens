package runner

import (
	"context"
	"testing"
)

// 11a 统一语义：Probe 的默认 Host 策略也必须允许同 Host、同 scheme 的 Realm。
func TestHostProbeDefaultPolicyAllowsSameHost(t *testing.T) {
	r := NewHostProbeRunner(HostProbeOptions{})
	p, err := r.o.Policies.Resolve(context.Background(), 0, "registry.example")
	if err != nil {
		t.Fatal(err)
	}
	if !p.AllowSameHost {
		t.Fatalf("Probe 默认策略未启用 AllowSameHost: %+v", p)
	}
}
