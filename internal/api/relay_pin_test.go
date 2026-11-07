package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/config"
	"github.com/zggnh12345/mirror-lens/internal/gateway"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

func stubLookup(t *testing.T, addrs []string, err error) {
	t.Helper()
	old := relayLookup
	relayLookup = func(ctx context.Context, host string) ([]netip.Addr, error) {
		if err != nil {
			return nil, err
		}
		out := make([]netip.Addr, 0, len(addrs))
		for _, s := range addrs {
			out = append(out, netip.MustParseAddr(s))
		}
		return out, nil
	}
	t.Cleanup(func() { relayLookup = old })
}

func newPinServer(t *testing.T, st *store.Store) (*Server, *gateway.Gateway) {
	t.Helper()
	gw := gateway.New(st, gateway.Options{})
	s := &Server{st: st, gw: gw}
	s.cfg.Store(config.Default())
	return s, gw
}

func addRelay(t *testing.T, st *store.Store, url string) int64 {
	t.Helper()
	id, err := st.AddSource(store.Source{Name: "relay", BaseURL: url, Origin: "manual", Enabled: true, Status: "usable"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetSourceTrusted(id, true); err != nil {
		t.Fatal(err)
	}
	return id
}

func pinsOf(t *testing.T, st *store.Store) store.TrustedRelay {
	t.Helper()
	list, err := st.ListTrustedRelays()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("应有 1 条信任源，实际 %d", len(list))
	}
	return list[0]
}

// 空状态 = 从未解析：解析成功进 pending，并钉住双栈地址。
func TestEnsurePinsEmptyStateToPending(t *testing.T) {
	st, _ := newTestServer(t)
	stubLookup(t, []string{"1.1.1.1", "8.8.8.8", "2606:4700::1111"}, nil)
	s, _ := newPinServer(t, st)
	addRelay(t, st, "https://relay.example:8443")

	if err := s.syncTrusted(); err != nil {
		t.Fatal(err)
	}
	p := pinsOf(t, st)
	if p.PinState != "pending" {
		t.Fatalf("状态应 pending，实际 %q", p.PinState)
	}
	if p.PinIPv4 != "1.1.1.1,8.8.8.8" || p.PinIPv6 != "2606:4700::1111" {
		t.Fatalf("钉住地址不对：%q / %q", p.PinIPv4, p.PinIPv6)
	}
	if p.PinAt == 0 {
		t.Fatal("pin_at 应记录首次钉住时间")
	}
}

// 解析落到内网 → 拒绝，进 unresolved 且带 24h 窗口。
func TestEnsurePinsPrivateResultRejected(t *testing.T) {
	st, _ := newTestServer(t)
	stubLookup(t, []string{"10.0.0.5"}, nil)
	s, _ := newPinServer(t, st)
	addRelay(t, st, "https://relay.example")

	if err := s.syncTrusted(); err != nil {
		t.Fatal(err)
	}
	p := pinsOf(t, st)
	if p.PinState != "unresolved" {
		t.Fatalf("应 unresolved，实际 %q", p.PinState)
	}
	if p.PinIPv4 != "" {
		t.Fatalf("内网地址不该被钉住：%q", p.PinIPv4)
	}
	if d := time.Until(time.Unix(p.PinGraceUntil, 0)); d < 23*time.Hour || d > 25*time.Hour {
		t.Fatalf("窗口应约 24h，实际 %s", d)
	}
}

// 窗口到期仍未解析成功 → expired，网关不再拨这个站。
func TestEnsurePinsExpiresAfterWindow(t *testing.T) {
	st, _ := newTestServer(t)
	s, _ := newPinServer(t, st)
	id := addRelay(t, st, "https://relay.example")
	if err := st.SetRelayPin(id, "unresolved", "", "", time.Now().Add(-time.Minute).Unix()); err != nil {
		t.Fatal(err)
	}
	if err := s.syncTrusted(); err != nil {
		t.Fatal(err)
	}
	if p := pinsOf(t, st); p.PinState != "expired" {
		t.Fatalf("应 expired，实际 %q", p.PinState)
	}
}

// 窗口内反复解析失败不能续期，否则就成了永久豁免。
func TestEnsurePinsDoesNotExtendWindow(t *testing.T) {
	st, _ := newTestServer(t)
	stubLookup(t, nil, errors.New("dns down"))
	s, _ := newPinServer(t, st)
	id := addRelay(t, st, "https://relay.example")
	until := time.Now().Add(3 * time.Hour).Unix()
	if err := st.SetRelayPin(id, "unresolved", "", "", until); err != nil {
		t.Fatal(err)
	}
	if err := s.syncTrusted(); err != nil {
		t.Fatal(err)
	}
	p := pinsOf(t, st)
	if p.PinGraceUntil != until {
		t.Fatalf("窗口不该被续期：%d → %d", until, p.PinGraceUntil)
	}
}

// 已经钉住的站不再做 DNS：解析器被调用就失败。
func TestEnsurePinsSkipsPinned(t *testing.T) {
	st, _ := newTestServer(t)
	called := false
	old := relayLookup
	relayLookup = func(ctx context.Context, host string) ([]netip.Addr, error) {
		called = true
		return nil, errors.New("不该解析")
	}
	t.Cleanup(func() { relayLookup = old })

	s, _ := newPinServer(t, st)
	id := addRelay(t, st, "https://relay.example")
	if err := st.SetRelayPin(id, "confirmed", "1.1.1.1", "2606:4700::1111", 0); err != nil {
		t.Fatal(err)
	}
	if err := s.syncTrusted(); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("confirmed 的站不该再解析")
	}
}

// 窗口到期后即使没重新同步，面板也要显示 expired（网关已按此拒绝拨号）。
func TestTrustedListShowsEffectiveExpired(t *testing.T) {
	st, h := newTestServer(t)
	ck := setupPanel(t, h)
	id := addRelay(t, st, "https://relay.example")
	if err := st.SetRelayPin(id, "unresolved", "", "", time.Now().Add(-time.Minute).Unix()); err != nil {
		t.Fatal(err)
	}
	rec := get(h, "/api/trusted", nil, ck)
	if rec.Code != 200 {
		t.Fatalf("状态码 %d", rec.Code)
	}
	var out struct {
		Sources []struct {
			PinState string `json:"pin_state"`
		} `json:"sources"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Sources) != 1 || out.Sources[0].PinState != "expired" {
		t.Fatalf("应显示 expired，实际 %+v", out.Sources)
	}
}
