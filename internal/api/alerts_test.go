package api

import (
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/zggnh12345/mirror-lens/internal/alert"
)

func TestAlertsListShape(t *testing.T) {
	s := &Server{}
	WithAlerts(alert.New(10))(s)
	s.alerts.Add(slog.LevelError, "数据库快照失败: disk full")

	w := httptest.NewRecorder()
	s.alertsList(w, httptest.NewRequest("GET", "/api/alerts", nil))
	if w.Code != 200 {
		t.Fatalf("状态码 %d", w.Code)
	}
	var got struct {
		Items  []alert.Entry `json:"items"`
		Unread int           `json:"unread"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Unread != 1 || len(got.Items) != 1 || got.Items[0].Level != "ERROR" {
		t.Fatalf("返回不对: %+v", got)
	}
}
