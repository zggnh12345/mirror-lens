package scheduler

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/store"
)

// 跨进程重启验证：真正起两个独立的测试二进制进程，一个写入 429 熔断后退出，
// 另一个从同一个库文件重新构造调度器。这样验证的是「进程重启后从 SQLite 恢复」，
// 而不是在同一个 *store.Store 句柄上再 New 一次（后者连接和缓存都还在）。
type restartState struct {
	ID         int64 `json:"id"`
	Fails      int   `json:"fails"`
	Until      int64 `json:"until"`
	Rev        int64 `json:"rev"`
	Candidates int   `json:"candidates"`
	Present    bool  `json:"present"`
}

// 子进程入口：ML_RESTART_ROLE=write|read 时才执行，正常全量测试里跳过。
func TestBreakerRestartHelperProcess(t *testing.T) {
	role := os.Getenv("ML_RESTART_ROLE")
	if role == "" {
		t.Skip("子进程专用入口")
	}
	dbPath, outPath := os.Getenv("ML_RESTART_DB"), os.Getenv("ML_RESTART_OUT")
	if dbPath == "" || outPath == "" {
		t.Fatal("缺少 ML_RESTART_DB / ML_RESTART_OUT")
	}
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("子进程打开库: %v", err)
	}
	defer st.Close()

	cfg := Config{Enabled: true, FailCooldown: 5 * time.Minute, MaxCooldown: time.Hour}
	var res restartState

	switch role {
	case "write":
		id, err := st.AddSource(store.Source{
			BaseURL: "https://a.example", Name: "a", GithubCap: 1, DockerCap: 1,
			Status: "usable", Enabled: true,
		})
		if err != nil {
			t.Fatalf("加源: %v", err)
		}
		s := New(st, cfg)
		c := candOf(id, "a")
		s.ReportOutcome("docker", c, OutcomeFail, 20*time.Millisecond, Hint{Code: 429})
		// 重启前再来一条中性事件：404 既不能洗掉 429 的冷却，也不能产生新失败。
		s.ReportOutcome("docker", c, OutcomeMiss, 5*time.Millisecond, Hint{Code: 404})

		row := waitBreakerRow(t, st, id, "docker")
		res = restartState{ID: id, Fails: row.Fails, Until: row.Until, Rev: row.Rev}

	case "read":
		s := New(st, cfg)
		rows, err := st.LoadBreakerStates()
		if err != nil {
			t.Fatalf("读熔断状态: %v", err)
		}
		var row store.BreakerState
		for _, r := range rows {
			if r.Kind == "docker" {
				row = r
			}
		}
		if row.SourceID == 0 {
			t.Fatal("重启后库里没有熔断行")
		}
		res = restartState{ID: row.SourceID, Fails: row.Fails, Until: row.Until, Rev: row.Rev}

		// 调度器必须按库恢复出同样的内存状态，而且冷却期内不作为正常候选。
		b := brkOf(s, "docker", row.SourceID)
		if b == nil {
			t.Fatal("重启后调度器内存里没有熔断状态")
		}
		res.Fails, res.Until = b.fails, b.until.Unix()
		list := s.CandidatesN("docker", 10)
		res.Candidates = len(list)
		for _, c := range list {
			if c.ID == row.SourceID && !c.Probe {
				res.Present = true
			}
		}

	default:
		t.Fatalf("未知角色 %q", role)
	}

	buf, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("序列化结果: %v", err)
	}
	if err := os.WriteFile(outPath, buf, 0o600); err != nil {
		t.Fatalf("写结果: %v", err)
	}
}

func waitBreakerRow(t *testing.T, st *store.Store, id int64, kind string) store.BreakerState {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rows, err := st.LoadBreakerStates()
		if err != nil {
			t.Fatalf("读熔断状态: %v", err)
		}
		for _, r := range rows {
			if r.SourceID == id && r.Kind == kind {
				return r
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("熔断行没有落库: %+v", rows)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func runRestartHelper(t *testing.T, role, dbPath, outPath string) restartState {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestBreakerRestartHelperProcess$", "-test.timeout=90s")
	cmd.Env = append(os.Environ(),
		"ML_RESTART_ROLE="+role,
		"ML_RESTART_DB="+dbPath,
		"ML_RESTART_OUT="+outPath,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("子进程(%s)失败: %v\n%s", role, err, out)
	}
	buf, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("读子进程结果: %v\n%s", err, out)
	}
	var res restartState
	if err := json.Unmarshal(buf, &res); err != nil {
		t.Fatalf("解析子进程结果: %v\n%s", err, buf)
	}
	return res
}

// 429 之后进程退出，新进程从同一库恢复：fails 和绝对截止时间都要原样保留，
// 冷却期内该源不能作为正常候选回来（否则就是「429 变 404 白等一轮」的那个 bug）。
func TestBreakerSurvivesRealProcessRestart(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ml.db")

	w := runRestartHelper(t, "write", dbPath, filepath.Join(dir, "write.json"))
	if w.ID == 0 || w.Fails != 1 {
		t.Fatalf("写入进程结果不对: %+v", w)
	}
	if w.Until <= time.Now().Unix() {
		t.Fatalf("写入进程的冷却截止时间不在将来: %d", w.Until)
	}

	r := runRestartHelper(t, "read", dbPath, filepath.Join(dir, "read.json"))
	if r.Fails != w.Fails {
		t.Fatalf("重启后 fails 变了: want %d got %d", w.Fails, r.Fails)
	}
	if d := r.Until - w.Until; d < -1 || d > 1 {
		t.Fatalf("重启后截止时间没原样恢复: want %d got %d（差 %ds）", w.Until, r.Until, d)
	}
	if r.Rev != w.Rev {
		t.Fatalf("重启后 rev 变了: want %d got %d", w.Rev, r.Rev)
	}
	if r.Present {
		t.Fatal("冷却期内该源又作为正常候选出现")
	}
}
