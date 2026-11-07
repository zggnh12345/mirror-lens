package store

import (
	"strings"
	"testing"
)

// 勾选信任时自动记下域名，作为后续钉 IP 的键。
func TestSetSourceTrustedRecordsHost(t *testing.T) {
	st := openTest(t)
	id, err := st.AddSource(Source{
		Name: "relay", BaseURL: "https://Relay.Example.COM:8443/base",
		Origin: "manual", Enabled: true, Status: "usable",
	})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := st.SetSourceTrusted(id, true); err != nil || !ok {
		t.Fatalf("勾选信任失败: %v %v", ok, err)
	}
	list, err := st.ListTrustedRelays()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("应有一条信任源，实际 %d", len(list))
	}
	if list[0].PinHost != "relay.example.com:8443" {
		t.Fatalf("pin_host 应规范化为小写 host:port，实际 %q", list[0].PinHost)
	}
	if list[0].PinState != "" {
		t.Fatalf("还没解析前不该有状态，实际 %q", list[0].PinState)
	}
}

// 管理员手填 IP 后直接进入 confirmed，并清掉迁移窗口。
func TestUpdateRelayPinConfirmed(t *testing.T) {
	st := openTest(t)
	id, err := st.AddSource(Source{
		Name: "relay", BaseURL: "https://relay.example", Origin: "manual",
		Enabled: true, Status: "usable",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetSourceTrusted(id, true); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.UpdateRelayPin(id, "1.1.1.1, 8.8.8.8", "2606:4700::1111"); err != nil || !ok {
		t.Fatalf("钉 IP 失败: %v %v", ok, err)
	}
	list, err := st.ListTrustedRelays()
	if err != nil {
		t.Fatal(err)
	}
	got := list[0]
	if got.PinState != "confirmed" || got.PinGraceUntil != 0 || got.PinConfirmedAt == 0 {
		t.Fatalf("钉住后状态不对：%+v", got)
	}
	if v4 := RelayPinIPs(got.PinIPv4); len(v4) != 2 || v4[0] != "1.1.1.1" || v4[1] != "8.8.8.8" {
		t.Fatalf("IPv4 解析不对：%v", v4)
	}
	if v6 := RelayPinIPs(got.PinIPv6); len(v6) != 1 || !strings.Contains(v6[0], ":") {
		t.Fatalf("IPv6 解析不对：%v", v6)
	}
}

func TestRelayPinIPsDropsGarbage(t *testing.T) {
	got := RelayPinIPs("1.1.1.1, not-an-ip, ,::1")
	if len(got) != 2 || got[0] != "1.1.1.1" || got[1] != "::1" {
		t.Fatalf("应只保留合法地址，实际 %v", got)
	}
	if RelayPinIPs("   ") != nil {
		t.Fatal("空白应返回 nil")
	}
}

// 老库升级：pin 列必须被自动补齐，否则信任源读取会整表报错。
func TestPinColumnsMigrated(t *testing.T) {
	st := openTest(t)
	var n int
	err := st.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('sources') WHERE name LIKE 'pin_%'`).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	if n != 7 {
		t.Fatalf("应有 7 个 pin 列，实际 %d", n)
	}
}

// 老库里的信任源没有 pin_host，读取时要按 base_url 补出来，否则面板和
// 钉 IP 逻辑拿不到域名。
func TestListTrustedRelaysBackfillsHost(t *testing.T) {
	st := openTest(t)
	id, err := st.AddSource(Source{
		Name: "legacy", BaseURL: "https://legacy.example:9443",
		Origin: "manual", Enabled: true, Status: "usable",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`UPDATE sources SET trusted=1, pin_host='' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	list, err := st.ListTrustedRelays()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].PinHost != "legacy.example:9443" {
		t.Fatalf("应回填 host，实际 %+v", list)
	}
}
