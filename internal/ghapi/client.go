// Package ghapi 只做一件事：向 GitHub Release API 查某个资产的摘要。
//
// 约束（来自产品决策）：
//   - 没有 Token 就完全不调用 API：不消耗匿名配额，也不给下载链路加延迟。
//   - API 超时/限流/不可用一律降级，绝不阻塞下载。
//   - digest 为 null 是正常情况（资产没有平台摘要），不是错误；
//     字段缺失才是 api_schema_unexpected，两者不能混。
//   - 只接受 sha256: 摘要；其他算法明确降级，不猜。
//   - 平台摘要缺失时才退到发布者校验文件；校验文件只证明字节与文件里写的
//     一致，不证明发布者身份。
package ghapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/sums"
)

// 结果分类。调用方据此决定降级路径，不要靠 error 字符串判断。
const (
	ResultOK             = "ok"
	ResultNoToken        = "no_token"
	ResultNotFound       = "not_found"
	ResultNoDigest       = "no_digest"
	ResultSchema         = "api_schema_unexpected"
	ResultUnsupportedAlg = "unsupported_algorithm"
	ResultRateLimited    = "rate_limited"
	// ResultAuthFailed 是 Token 失效/被吊销（401）。与限流分开：前者改配置才能恢复，
	// 后者等一会儿就好，混在一起运维看不出该做什么。
	ResultAuthFailed  = "auth_failed"
	ResultUnavailable = "unavailable"
	ResultTimeout     = "timeout"
)

// 摘要来源。
const (
	SourceAPI  = "github_asset_api"
	SourceSUMS = "publisher_checksum_file"
	SourceNone = "none"

	checksumMax = 2 << 20
)

// Digest 是一次查询的结果。Err 非空时 Algorithm/Hex 无意义。
type Digest struct {
	Algorithm string
	Hex       string
	Source    string
	Result    string
	Err       error

	// 身份绑定：校验文件与目标资产必须来自同一份 Release 元数据快照。
	ReleaseID       int64
	AssetID         int64
	AssetUpdatedAt  string
	ChecksumAssetID int64
	ChecksumUpdated string
}

const defaultBase = "https://api.github.com"

type Client struct {
	Token  string
	Base   string
	HTTP   *http.Client
	TTL    time.Duration // 正结果缓存时间
	NegTTL time.Duration // 负结果缓存时间，避免对同一个 404 反复打 API

	mu    sync.Mutex
	cache map[string]cacheEntry
	sf    singleflight

	// 连续 401 到阈值就本进程停用 Release API：Token 失效时不该每 2 分钟
	// 继续打无效请求。改配置或重启后自然恢复。
	authFails atomic.Int32
	disabled  atomic.Bool
}

// authFailLimit 是停用前的连续 401 次数。
const authFailLimit = 5

type cacheEntry struct {
	d   Digest
	exp time.Time
}

func New(token string) *Client {
	return &Client{
		Token:  strings.TrimSpace(token),
		Base:   defaultBase,
		HTTP:   &http.Client{Timeout: 4 * time.Second},
		TTL:    10 * time.Minute,
		NegTTL: 2 * time.Minute,
		cache:  map[string]cacheEntry{},
	}
}

// Enabled 表示是否配置了 Token。未配置时调用方不应调用 AssetDigest。
func (c *Client) Enabled() bool { return c != nil && c.Token != "" }

// AssetDigest 查 release 资产的摘要。owner/repo/tag/file 均为原始大小写。
// tag 为空表示 latest。
func (c *Client) AssetDigest(ctx context.Context, owner, repo, tag, file string) Digest {
	if !c.Enabled() {
		return Digest{Result: ResultNoToken, Source: SourceNone, Err: errors.New("未配置 GitHub Token")}
	}
	if c.disabled.Load() {
		return Digest{Result: ResultAuthFailed, Source: SourceNone,
			Err: errors.New("GitHub Token 鉴权连续失败，已停用 Release API（改配置或重启后恢复）")}
	}
	key := strings.ToLower(owner + "/" + repo + "/" + tag + "/" + file)
	if d, ok := c.cached(key); ok {
		return d
	}
	d := c.sf.Do(key, func() Digest { return c.fetch(ctx, owner, repo, tag, file) })
	if d.Result == ResultAuthFailed {
		if c.authFails.Add(1) >= authFailLimit && c.disabled.CompareAndSwap(false, true) {
			log.Printf("GitHub Token 连续 %d 次鉴权失败（401），已停用 Release 摘要 API；下载不受影响，改配置或重启后恢复", authFailLimit)
		}
	} else {
		c.authFails.Store(0)
	}
	c.store(key, d)
	return d
}

func (c *Client) cached(key string) (Digest, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.cache[key]
	if !ok || time.Now().After(e.exp) {
		return Digest{}, false
	}
	return e.d, true
}

func (c *Client) store(key string, d Digest) {
	ttl := c.TTL
	if d.Result != ResultOK {
		ttl = c.NegTTL
	}
	if ttl <= 0 {
		return
	}
	c.mu.Lock()
	c.cache[key] = cacheEntry{d: d, exp: time.Now().Add(ttl)}
	c.mu.Unlock()
}

type asset struct {
	ID          int64           `json:"id"`
	Name        string          `json:"name"`
	Size        int64           `json:"size"`
	Digest      json.RawMessage `json:"digest"`
	UpdatedAt   string          `json:"updated_at"`
	DownloadURL string          `json:"browser_download_url"`
}

type release struct {
	ID     int64    `json:"id"`
	Assets *[]asset `json:"assets"`
}

func (c *Client) fetch(ctx context.Context, owner, repo, tag, file string) Digest {
	rel, d := c.release(ctx, owner, repo, tag)
	if d.Result != ResultOK {
		return d
	}
	var target *asset
	for i := range *rel.Assets {
		if (*rel.Assets)[i].Name == file {
			target = &(*rel.Assets)[i]
			break
		}
	}
	if target == nil {
		return Digest{Result: ResultNotFound, Source: SourceNone, Err: errors.New("release 中没有该资产")}
	}
	base := Digest{
		ReleaseID: rel.ID, AssetID: target.ID, AssetUpdatedAt: target.UpdatedAt, Source: SourceNone,
	}
	hex, result := parseDigest(target.Digest)
	if result != ResultOK {
		if result != ResultNoDigest {
			base.Result = result
			return base
		}
		// 平台没有摘要：退到发布者校验文件。
		return c.fromChecksum(ctx, rel, target.Name, base)
	}
	base.Algorithm, base.Hex, base.Result, base.Source = "sha256", hex, ResultOK, SourceAPI
	return base
}

// release 取一次 Release 元数据（结果由上层按资产缓存，不在这里另设缓存）。
func (c *Client) release(ctx context.Context, owner, repo, tag string) (*release, Digest) {
	u := fmt.Sprintf("%s/repos/%s/%s/releases/tags/%s", c.Base, owner, repo, tag)
	if tag == "" {
		u = fmt.Sprintf("%s/repos/%s/%s/releases/latest", c.Base, owner, repo)
	}
	body, code, err := c.get(ctx, u, "application/vnd.github+json")
	if err != nil {
		return nil, classifyNetErr(ctx, err)
	}
	switch {
	case code == http.StatusUnauthorized:
		return nil, Digest{Result: ResultAuthFailed, Source: SourceNone, Err: fmt.Errorf("GitHub API %d（Token 无效或已过期）", code)}
	case code == http.StatusForbidden || code == http.StatusTooManyRequests:
		return nil, Digest{Result: ResultRateLimited, Source: SourceNone, Err: fmt.Errorf("GitHub API %d", code)}
	case code == http.StatusNotFound:
		return nil, Digest{Result: ResultNotFound, Source: SourceNone, Err: errors.New("release 不存在")}
	case code >= 500:
		return nil, Digest{Result: ResultUnavailable, Source: SourceNone, Err: fmt.Errorf("GitHub API %d", code)}
	case code != http.StatusOK:
		return nil, Digest{Result: ResultUnavailable, Source: SourceNone, Err: fmt.Errorf("GitHub API %d", code)}
	}
	var rel release
	if err := json.Unmarshal(body, &rel); err != nil {
		return nil, Digest{Result: ResultSchema, Source: SourceNone, Err: err}
	}
	if rel.Assets == nil {
		return nil, Digest{Result: ResultSchema, Source: SourceNone, Err: errors.New("响应缺少 assets")}
	}
	return &rel, Digest{Result: ResultOK, Source: SourceNone}
}

var checksumNames = []string{"SHA256SUMS", "sha256sums.txt", "SHA256SUMS.txt", "checksums.txt", "sha256.txt"}

// fromChecksum 在同一份 Release 快照里找校验文件，下载并解析目标资产。
func (c *Client) fromChecksum(ctx context.Context, rel *release, target string, base Digest) Digest {
	var cs *asset
	for i := range *rel.Assets {
		for _, n := range checksumNames {
			if (*rel.Assets)[i].Name == n {
				cs = &(*rel.Assets)[i]
				break
			}
		}
		if cs != nil {
			break
		}
	}
	if cs == nil || cs.DownloadURL == "" {
		base.Result = ResultNoDigest
		return base
	}
	if cs.Size > checksumMax {
		base.Result = ResultNoDigest
		base.Err = errors.New("校验文件过大")
		return base
	}
	body, code, err := c.get(ctx, cs.DownloadURL, "application/octet-stream")
	if err != nil {
		d := classifyNetErr(ctx, err)
		d.Source = SourceNone
		return d
	}
	if code != http.StatusOK {
		base.Result = ResultUnavailable
		base.Err = fmt.Errorf("校验文件 HTTP %d", code)
		return base
	}
	hex, err := sums.Lookup(body, target)
	if err != nil {
		base.Result = ResultNoDigest
		base.Err = err
		return base
	}
	base.Algorithm, base.Hex, base.Result = "sha256", hex, ResultOK
	base.Source = SourceSUMS
	base.ChecksumAssetID, base.ChecksumUpdated = cs.ID, cs.UpdatedAt
	return base
}

func (c *Client) get(ctx context.Context, u, accept string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return body, resp.StatusCode, err
}

func classifyNetErr(ctx context.Context, err error) Digest {
	if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
		return Digest{Result: ResultTimeout, Source: SourceNone, Err: err}
	}
	return Digest{Result: ResultUnavailable, Source: SourceNone, Err: err}
}

// parseDigest 严格区分三种情况：
//  1. 有 sha256:digest → (hex, OK)
//  2. 显式 null → ("", NoDigest)（正常）
//  3. 字段缺失 / 结构异常 / 非 sha256 → ("", 对应分类)
func parseDigest(raw json.RawMessage) (string, string) {
	if len(raw) == 0 {
		return "", ResultSchema
	}
	if string(raw) == "null" {
		return "", ResultNoDigest
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", ResultSchema
	}
	algo, hex, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok || hex == "" {
		return "", ResultSchema
	}
	if !strings.EqualFold(algo, "sha256") {
		return "", ResultUnsupportedAlg
	}
	return strings.ToLower(hex), ResultOK
}

// singleflight 只合并同一资产的并发查询；API 挂了时不要让每个请求都等 4 秒。
type singleflight struct {
	mu sync.Mutex
	m  map[string]*call
}

type call struct {
	done chan struct{}
	d    Digest
}

func (s *singleflight) Do(key string, fn func() Digest) Digest {
	s.mu.Lock()
	if s.m == nil {
		s.m = map[string]*call{}
	}
	if c, ok := s.m[key]; ok {
		s.mu.Unlock()
		<-c.done
		return c.d
	}
	c := &call{done: make(chan struct{})}
	s.m[key] = c
	s.mu.Unlock()

	c.d = fn()
	close(c.done)
	s.mu.Lock()
	delete(s.m, key)
	s.mu.Unlock()
	return c.d
}
