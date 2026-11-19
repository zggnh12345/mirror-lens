package unified

import (
	"context"
	"net/http"
)

type scopeKey struct{}

// WithScope 把 RequestScope 绑到请求 Context。所有 Registry 上游请求
// （含 Token 获取与认证重发）必须携带它，P 才能被完整计量。
func WithScope(ctx context.Context, s *Scope) context.Context {
	if s == nil {
		return ctx
	}
	return context.WithValue(ctx, scopeKey{}, s)
}

// FromContext 取出 RequestScope；没有时返回 nil（非 Registry 路径）。
func FromContext(ctx context.Context) *Scope {
	if ctx == nil {
		return nil
	}
	s, _ := ctx.Value(scopeKey{}).(*Scope)
	return s
}

// PTransport 是应用层受控派发的计量点。它必须包裹在底层 http.Transport
// 之上、认证重试层之下：只有这样 Token 请求（t.base 直发）才会被计入，
// 而 Transport 内部的自动重试不会被多算（design §3.2）。
type PTransport struct {
	Base http.RoundTripper
	// OnCommit 在正式提交派发前回调（指标/日志），不阻塞。
	OnCommit func()
	// OnExhaust 在因预算耗尽拒绝派发时回调一次。
	OnExhaust func()
}

// RoundTrip 实现 http.RoundTripper。
func (t *PTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	sc := FromContext(req.Context())
	if sc == nil {
		return base.RoundTrip(req)
	}
	res, err := sc.ReserveP()
	if err != nil {
		if IsBudgetExhausted(err) {
			// 冻结：耗尽即终止本 Scope 的剩余派发（取消在途 Attempt、
			// 关闭未完成 Body），不再启动任何新请求。
			sc.markExhausted("P")
			if t.OnExhaust != nil {
				t.OnExhaust()
			}
		}
		return nil, err
	}
	// 预留后、提交前再检查一次取消：未提交的预留必须归还，
	// 否则客户端取消会永久吃掉 P。
	if cerr := req.Context().Err(); cerr != nil {
		res.Release()
		return nil, cerr
	}
	if !res.Commit() {
		return nil, ErrBudgetExhausted
	}
	if t.OnCommit != nil {
		t.OnCommit()
	}
	return base.RoundTrip(req)
}

// 预算耗尽对应的 HTTP 语义（design §3.2 冻结）。
const (
	// CodeBudgetExhausted 是内部错误码，写入 X-Ml-Error 供客户端与日志识别。
	CodeBudgetExhausted = "REQUEST_BUDGET_EXHAUSTED"
	// HeaderError 是网关写内部错误码的响应头。
	HeaderError = "X-Ml-Error"
)

// WriteBudgetExhausted 写 503 + no-store。它不冷却 Source、不回退请求，
// 也不污染任何源的成功率样本。
func WriteBudgetExhausted(w http.ResponseWriter) {
	if w == nil {
		return
	}
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Retry-After", "0")
	h.Set(HeaderError, CodeBudgetExhausted)
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte("503 request budget exhausted\n"))
}

// BudgetDecision 把账本仲裁结论映射成「是否预算耗尽」。
func (d Decision) BudgetExhausted() bool {
	return d.Outcome == OutcomeBudget
}

// CodeScopeCapacity 表示全局 RequestScope 容量（G）已满，请求未被准入。
const CodeScopeCapacity = "REQUEST_SCOPE_CAPACITY"

// WriteScopeCapacity 写 503 + no-store。容量满不是上游故障，不冷却源。
func WriteScopeCapacity(w http.ResponseWriter) {
	if w == nil {
		return
	}
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Retry-After", "1")
	h.Set(HeaderError, CodeScopeCapacity)
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte("503 request scope capacity exhausted\n"))
}
