package api

import (
	"net/http"
	"testing"

	"github.com/zggnh12345/mirror-lens/internal/store"
)

// ☆ 优先开关走 PATCH /api/sources/:id，preferred 要落库且不动其它字段。
func TestPatchPreferredSource(t *testing.T) {
	st, h := newTestServer(t)
	ck := setupPanel(t, h)

	id, err := st.AddSource(store.Source{
		Name: "m", BaseURL: "http://127.0.0.1:1", Status: "usable",
		Enabled: true, DockerCap: store.CapYes, GithubCap: store.CapYes,
	})
	if err != nil {
		t.Fatal(err)
	}

	rec := patchJSON(h, "/api/sources/"+itoa(id), `{"preferred":true}`, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("设为优先应 200，得到 %d %s", rec.Code, rec.Body.String())
	}
	got, err := st.GetSource(id)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Preferred {
		t.Fatal("preferred 未写入")
	}
	if !got.Enabled || got.Name != "m" {
		t.Fatalf("其它字段被改动：%+v", got)
	}

	rec = patchJSON(h, "/api/sources/"+itoa(id), `{"preferred":false}`, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("取消优先应 200，得到 %d %s", rec.Code, rec.Body.String())
	}
	got, err = st.GetSource(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Preferred {
		t.Fatal("preferred 未取消")
	}
}
