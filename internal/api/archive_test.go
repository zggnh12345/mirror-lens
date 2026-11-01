package api

import (
	"net/http"
	"testing"

	"github.com/zggnh12345/mirror-lens/internal/store"
)

// 归档是判断过的状态：普通启用不能顶回调度，需显式给 pending 恢复。
func TestArchivedSourceCannotBeEnabledDirectly(t *testing.T) {
	st, h := newTestServer(t)
	ck := setupPanel(t, h)

	id, err := st.AddSource(store.Source{
		Name: "old", BaseURL: "http://127.0.0.1:1", Status: "archived",
		Enabled: true, DockerCap: store.CapYes, GithubCap: store.CapNo,
	})
	if err != nil {
		t.Fatal(err)
	}

	rec := patchJSON(h, "/api/sources/"+itoa(id), `{"enabled":true}`, ck)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("归档源直接启用应 400，得到 %d %s", rec.Code, rec.Body.String())
	}
	got, _ := st.GetSource(id)
	if got.Status != "archived" {
		t.Fatalf("状态被改成了 %s", got.Status)
	}

	// 显式恢复：同请求给 pending 才允许重新启用。
	rec = patchJSON(h, "/api/sources/"+itoa(id), `{"enabled":true,"status":"pending"}`, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("显式恢复应 200，得到 %d %s", rec.Code, rec.Body.String())
	}
	got, _ = st.GetSource(id)
	if got.Status != "pending" {
		t.Fatalf("恢复后应为 pending（等复检），得到 %s", got.Status)
	}
	if !got.Enabled {
		t.Fatal("恢复后应已启用")
	}
}

// 归档的源不出现在调度候选里。
func TestArchivedSourceNotInCandidates(t *testing.T) {
	st, h := newTestServer(t)
	setupPanel(t, h)

	id, err := st.AddSource(store.Source{
		Name: "old", BaseURL: "http://127.0.0.1:1", Status: "archived",
		Enabled: true, DockerCap: store.CapYes, GithubCap: store.CapNo,
	})
	if err != nil {
		t.Fatal(err)
	}
	srcs, err := st.EnabledSources()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range srcs {
		if s.ID == id {
			t.Fatal("归档源不该出现在 EnabledSources 里")
		}
	}
}
