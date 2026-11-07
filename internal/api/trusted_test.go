package api

import (
	"net/http"
	"testing"

	"github.com/zggnh12345/mirror-lens/internal/store"
)

// 白名单直接从已有源里勾选，不再单独录入地址。
func TestTrustedWhitelistUsesExistingSources(t *testing.T) {
	st, h := newTestServer(t)
	ck := setupPanel(t, h)

	a, err := st.AddSource(store.Source{Name: "relay-a", BaseURL: "http://127.0.0.1:1", Status: "usable", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	b, err := st.AddSource(store.Source{Name: "relay-b", BaseURL: "http://127.0.0.1:2", Status: "usable", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}

	// 列表里应有全部源，且默认都没勾。
	rec := get(h, "/api/trusted", nil, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/trusted = %d %s", rec.Code, rec.Body.String())
	}

	// 勾选两个。
	for _, id := range []int64{a, b} {
		if r := patchJSON(h, "/api/trusted/"+itoa(id), `{"trusted":true}`, ck); r.Code != http.StatusOK {
			t.Fatalf("勾选 %d 失败: %d %s", id, r.Code, r.Body.String())
		}
	}
	list, err := st.ListTrustedRelays()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("白名单应有 2 个，实际 %d", len(list))
	}

	// 取消一个。
	if r := patchJSON(h, "/api/trusted/"+itoa(a), `{"trusted":false}`, ck); r.Code != http.StatusOK {
		t.Fatalf("取消勾选失败: %d %s", r.Code, r.Body.String())
	}
	list, _ = st.ListTrustedRelays()
	if len(list) != 1 || list[0].ID != b {
		t.Fatalf("取消后应只剩 %d，实际 %+v", b, list)
	}

	// 停用源即使勾了也不参与竞速。
	if r := patchJSON(h, "/api/sources/"+itoa(b), `{"enabled":false}`, ck); r.Code != http.StatusOK {
		t.Fatalf("停用源失败: %d %s", r.Code, r.Body.String())
	}
	list, _ = st.ListTrustedRelays()
	if len(list) != 0 {
		t.Fatalf("停用的源不该出现在白名单，实际 %+v", list)
	}

	// 不存在的 id 返回 404。
	if r := patchJSON(h, "/api/trusted/999999", `{"trusted":true}`, ck); r.Code != http.StatusNotFound {
		t.Fatalf("不存在的源应 404，得到 %d", r.Code)
	}
}

// 管理员可以手填钉住地址：直接 confirmed，非法地址整体拒绝。
func TestTrustedPinManualUpdate(t *testing.T) {
	st, h := newTestServer(t)
	ck := setupPanel(t, h)

	id, err := st.AddSource(store.Source{Name: "relay", BaseURL: "https://relay.example", Status: "usable", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if r := patchJSON(h, "/api/trusted/"+itoa(id), `{"trusted":true}`, ck); r.Code != http.StatusOK {
		t.Fatalf("勾选失败: %d %s", r.Code, r.Body.String())
	}

	if r := patchJSON(h, "/api/trusted/"+itoa(id), `{"pin_ipv4":"1.1.1.1,8.8.8.8","pin_ipv6":"2606:4700::1111"}`, ck); r.Code != http.StatusOK {
		t.Fatalf("钉 IP 失败: %d %s", r.Code, r.Body.String())
	}
	list, err := st.ListTrustedRelays()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].PinState != "confirmed" || list[0].PinIPv4 != "1.1.1.1,8.8.8.8" {
		t.Fatalf("钉住结果不对：%+v", list)
	}

	if r := patchJSON(h, "/api/trusted/"+itoa(id), `{"pin_ipv4":"1.1.1.1,bad"}`, ck); r.Code != http.StatusBadRequest {
		t.Fatalf("非法地址应 400，得到 %d %s", r.Code, r.Body.String())
	}

	// 没勾信任的源不能钉。
	other, err := st.AddSource(store.Source{Name: "plain", BaseURL: "https://plain.example", Status: "usable", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if r := patchJSON(h, "/api/trusted/"+itoa(other), `{"pin_ipv4":"1.1.1.1"}`, ck); r.Code != http.StatusNotFound {
		t.Fatalf("未勾信任应 404，得到 %d %s", r.Code, r.Body.String())
	}
}
