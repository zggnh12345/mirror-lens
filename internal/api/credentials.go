package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/zggnh12345/mirror-lens/internal/auth"
	"github.com/zggnh12345/mirror-lens/internal/secret"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

// 3.5.15-A.3 凭据管理 API。
//
// 硬约束：
//   - 写操作必须 guard(csrf(...))，只允许管理员会话；
//   - 必须 HTTPS（或可信反代 X-Forwarded-Proto=https）；
//   - 响应 no-store；请求体 64 KiB；密码/Token 不出现在 URL/日志/错误体；
//   - PUT 更新必须带 expected_gen，冲突 409。

const credBodyLimit = 64 << 10

// credWriteAllowed 判定这次写请求是否满足 HTTPS 要求。
// 纯 HTTP 默认拒绝，除非显式本地调试开关（不随生产配置发布）。
func credWriteAllowed(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	if strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		return true
	}
	return os.Getenv("ML_ALLOW_HTTP_CREDENTIALS") == "1"
}

func (s *Server) credentialsList(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		writeJSON(w, 405, map[string]string{"error": "method"})
		return
	}
	if s.creds == nil {
		writeJSON(w, 503, map[string]string{"error": "凭据存储不可用"})
		return
	}
	list, err := s.creds.ListStatus()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "读取凭据状态失败"})
		return
	}
	// UI 只展示 credentialCapableHosts()：ANONYMOUS_ONLY / NOT_SUPPORTED
	// 的 Host 不出现"添加凭据"入口。
	capable := map[string]bool{}
	for _, h := range secret.CredentialCapableHosts() {
		capable[h] = true
	}
	out := make([]secret.Status, 0, len(list))
	for _, st := range list {
		if capable[st.Host] {
			out = append(out, st)
		}
	}
	writeJSON(w, 200, map[string]any{
		"hosts":        out,
		"capable":      secret.CredentialCapableHosts(),
		"private_auth": s.privateAuthEnabled(),
	})
}

func (s *Server) credentialItem(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.creds == nil {
		writeJSON(w, 503, map[string]string{"error": "凭据存储不可用"})
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/credentials/")
	if rest == "" || rest == r.URL.Path {
		writeJSON(w, 404, map[string]string{"error": "缺少 Host"})
		return
	}
	action := ""
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest, action = rest[:i], rest[i+1:]
	}
	host := strings.TrimSpace(rest)
	if host == "" || strings.ContainsAny(host, "/\\") {
		writeJSON(w, 400, map[string]string{"error": "Host 不合法"})
		return
	}

	switch {
	case action == "" && r.Method == http.MethodGet:
		st, err := s.creds.Status(host)
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": "Host 不合法"})
			return
		}
		writeJSON(w, 200, st)
	case action == "" && r.Method == http.MethodPut:
		s.putCredential(w, r, host)
	case action == "" && r.Method == http.MethodDelete:
		s.deleteCredential(w, r, host)
	case action == "verify" && r.Method == http.MethodPost:
		s.verifyCredential(w, r, host)
	case action == "reconcile" && r.Method == http.MethodPost:
		st, err := s.creds.ReconcileHost(host)
		ok := err == nil && st.State != secret.StateQuarantined
		s.auditAs(r, s.currentUser(), "credential_reconcile", host,
			fmt.Sprintf("state=%s reason=%s", st.State, st.Reason), ok)
		if err != nil && st.State == secret.StateQuarantined {
			writeJSON(w, 409, map[string]any{"error": "该 Host 记录损坏，需重新写入", "status": st})
			return
		}
		if err != nil {
			writeJSON(w, 500, map[string]any{"error": "对账失败", "status": st})
			return
		}
		writeJSON(w, 200, st)
	default:
		writeJSON(w, 405, map[string]string{"error": "method"})
	}
}

func (s *Server) putCredential(w http.ResponseWriter, r *http.Request, host string) {
	if !credWriteAllowed(r) {
		writeJSON(w, 403, map[string]any{"error": "凭据接口必须通过 HTTPS", "code": "https_required"})
		return
	}
	if !secret.CredentialHostAllowed(host) {
		writeJSON(w, 400, map[string]any{
			"error": "该 Host 不在私有凭据 allowlist 内",
			"code":  "host_not_allowed",
		})
		return
	}
	var in struct {
		Mode        string  `json:"mode"`
		Username    string  `json:"username"`
		Password    *string `json:"password"`
		ExpectedGen *int64  `json:"expected_gen"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, credBodyLimit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "请求格式不对"})
		return
	}
	mode := secret.AuthMode(in.Mode)
	if !mode.Valid() {
		writeJSON(w, 400, map[string]string{"error": "mode 只支持 basic / bearer_token_basic / bearer_token_none"})
		return
	}
	// 更新必须带 expected_gen：没带就按「创建」处理，已存在则拒绝。
	cur, _ := s.creds.Status(host)
	exists := cur.State == secret.StateReady || cur.State == secret.StatePending
	if exists && in.ExpectedGen == nil {
		writeJSON(w, 409, map[string]any{"error": "更新必须带 expected_gen", "current_gen": cur.Gen})
		return
	}

	cred := secret.Credential{Mode: string(mode), Username: in.Username}
	password := ""
	if in.Password != nil {
		password = *in.Password
	} else if exists {
		// password 省略表示保留现有密码（只允许更新场景）。
		if c, ok, err := s.creds.Get(host); err == nil && ok {
			password = c.Password
			if cred.Username == "" {
				cred.Username = c.Username
			}
		}
	}
	switch mode {
	case secret.ModeBasic, secret.ModeBearerTokenBasic:
		if password == "" {
			writeJSON(w, 400, map[string]string{"error": "该模式必须提供密码"})
			return
		}
		cred.Password = password
		if mode == secret.ModeBearerTokenBasic {
			cred.TokenAuth = "basic"
		}
	case secret.ModeBearerTokenNone:
		cred.TokenAuth = "none"
		if in.Password != nil && *in.Password == "" {
			writeJSON(w, 400, map[string]string{"error": "显式空密码不合法；删除请用 DELETE"})
			return
		}
		cred.Password = password
	}

	res, err := s.creds.PutCredential(host, cred, in.ExpectedGen,
		secret.WriteActor{User: s.currentUser(), IP: auth.ClientIP(r)})
	if err != nil {
		s.auditAs(r, s.currentUser(), "credential_put", host,
			fmt.Sprintf("mode=%s gen=%d changed=%v", mode, res.Gen, res.Changed), false)
	}
	switch {
	case errors.Is(err, store.ErrCredGenConflict):
		writeJSON(w, 409, map[string]any{"error": "凭据已被其他操作修改", "current_gen": res.Gen})
		return
	case errors.Is(err, secret.ErrInvalidCredential):
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	case err != nil:
		writeJSON(w, 500, map[string]any{"error": "写入失败", "state": res.State})
		return
	}
	// 凭据变更必须立刻生效：清掉该 Host 的认证缓存与负缓存。
	if s.gw != nil && res.Changed {
		s.gw.InvalidateCredentialCaches(host)
	}
	if res.State == secret.StatePending {
		writeJSON(w, 202, map[string]any{"state": res.State, "gen": res.Gen})
		return
	}
	writeJSON(w, 200, map[string]any{"state": res.State, "gen": res.Gen, "changed": res.Changed})
}

func (s *Server) deleteCredential(w http.ResponseWriter, r *http.Request, host string) {
	if !credWriteAllowed(r) {
		writeJSON(w, 403, map[string]any{"error": "凭据接口必须通过 HTTPS", "code": "https_required"})
		return
	}
	var in struct {
		ExpectedGen *int64 `json:"expected_gen"`
	}
	if r.ContentLength > 0 {
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, credBodyLimit)).Decode(&in)
	}
	res, err := s.creds.DeleteCredential(host, in.ExpectedGen,
		secret.WriteActor{User: s.currentUser(), IP: auth.ClientIP(r)})
	if err != nil {
		s.auditAs(r, s.currentUser(), "credential_delete", host,
			fmt.Sprintf("gen=%d changed=%v", res.Gen, res.Changed), false)
	}
	switch {
	case errors.Is(err, store.ErrCredGenConflict):
		writeJSON(w, 409, map[string]any{"error": "凭据已被其他操作修改", "current_gen": res.Gen})
		return
	case errors.Is(err, secret.ErrRecordMissing):
		writeJSON(w, 409, map[string]any{"error": "数据库有记录但文件缺失，需先修复", "state": res.State})
		return
	case err != nil:
		writeJSON(w, 500, map[string]any{"error": "删除失败", "state": res.State})
		return
	}
	if s.gw != nil {
		s.gw.InvalidateCredentialCaches(host)
	}
	if res.State == secret.StatePending {
		writeJSON(w, 202, map[string]any{"state": res.State})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) verifyCredential(w http.ResponseWriter, r *http.Request, host string) {
	if !credWriteAllowed(r) {
		writeJSON(w, 403, map[string]any{"error": "凭据接口必须通过 HTTPS", "code": "https_required"})
		return
	}
	if s.gw == nil {
		writeJSON(w, 503, map[string]string{"error": "Gateway 不可用"})
		return
	}
	st, err := s.creds.Status(host)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "Host 不合法"})
		return
	}
	if st.State != secret.StateReady {
		writeJSON(w, 409, map[string]any{"error": "凭据状态不是 Ready", "status": st})
		return
	}
	res := s.gw.VerifyCredential(r.Context(), host)
	ok := res.Status == http.StatusOK
	s.auditAs(r, s.currentUser(), "credential_verify", host,
		fmt.Sprintf("status=%d code=%s", res.Status, res.Code), ok)
	// 只回状态与稳定错误码，不回上游正文、Token 或 Authorization。
	writeJSON(w, 200, map[string]any{"ok": ok, "status": res.Status, "code": res.Code})
}

func (s *Server) currentUser() string {
	return s.st.GetSetting(auth.KeyUser, auth.DefaultUser)
}

// privateAuthEnabled 报告私有认证是否开启（用于 UI 提示 blast radius）。
func (s *Server) privateAuthEnabled() bool {
	c := s.cfg.Load()
	if c == nil {
		return false
	}
	return c.Registry.PrivateAuthEnabled
}
