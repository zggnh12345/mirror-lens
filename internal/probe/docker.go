package probe

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// 用最热门、几乎一定被缓存过的镜像做探针。
const (
	probeImage = "library/alpine"
	probeTag   = "latest"
)

var manifestAccept = strings.Join([]string{
	"application/vnd.docker.distribution.manifest.v2+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.oci.image.index.v1+json",
}, ", ")

type challenge struct {
	Realm   string
	Service string
	Scope   string
}

func parseChallenge(h string) challenge {
	var c challenge
	h = strings.TrimSpace(h)
	if h == "" {
		return c
	}
	if i := strings.IndexByte(h, ' '); i > 0 {
		h = h[i+1:]
	}
	for _, part := range strings.Split(h, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		k := strings.ToLower(strings.TrimSpace(kv[0]))
		v := strings.Trim(strings.TrimSpace(kv[1]), `"`)
		switch k {
		case "realm":
			c.Realm = v
		case "service":
			c.Service = v
		case "scope":
			c.Scope = v
		}
	}
	return c
}

// Docker probes one registry mirror. tier 1 = liveness only, 2 = real manifest,
// 3 = manifest plus a blob download for throughput.
func Docker(ctx context.Context, base string, tier int, expectedDigest string) Result {
	base = trimBase(base)
	host := hostOf(base)
	res := Result{DigestOK: -1}

	ok, days, _ := tlsInfo(ctx, host)
	res.TLSOK, res.CertDays = ok, days

	client := newClient(25 * time.Second)
	t0 := time.Now()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v2/", nil)
	if err != nil {
		res.Error = errStr(err)
		return res
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
	if err != nil {
		res.Error = errStr(err)
		res.LatencyMS = time.Since(t0).Milliseconds()
		return res
	}
	body, _ := readCapped(resp.Body, 2048)
	resp.Body.Close()
	res.HTTPCode = resp.StatusCode
	res.LatencyMS = time.Since(t0).Milliseconds()

	ct := resp.Header.Get("Content-Type")
	if resp.StatusCode == 200 && strings.Contains(ct, "text/html") {
		res.Error = "返回 HTML 页面，不是 registry"
		res.Detail = "假站点"
		return res
	}
	if resp.StatusCode != 200 && resp.StatusCode != 401 {
		res.Error = fmt.Sprintf("/v2/ 返回 %d", resp.StatusCode)
		res.Detail = string(body[:min(len(body), 80)])
		return res
	}
	if tier <= 1 {
		res.OK = resp.StatusCode == 401 || resp.StatusCode == 200
		if !res.OK {
			res.Error = fmt.Sprintf("/v2/ 返回 %d", resp.StatusCode)
		}
		return res
	}

	// tier >= 2：完整 token 流程 + 真 manifest
	tok := ""
	if resp.StatusCode == 401 {
		ch := parseChallenge(resp.Header.Get("Www-Authenticate"))
		if ch.Realm == "" {
			res.Error = "401 但未给出认证地址"
			return res
		}
		if ch.Scope == "" {
			ch.Scope = "repository:" + probeImage + ":pull"
		}
		t, code, terr := fetchToken(ctx, client, ch)
		if terr != nil || t == "" {
			res.Error = fmt.Sprintf("token 获取失败 (%d) %s", code, errStr(terr))
			return res
		}
		tok = t
	}

	mf, digest, code, mtype, err := fetchManifest(ctx, client, base, tok, probeImage+"/manifests/"+probeTag)
	if err != nil || code != 200 {
		res.HTTPCode = code
		res.Error = fmt.Sprintf("manifest 获取失败 (%d) %s", code, errStr(err))
		return res
	}
	// 校验基准用索引（multi-arch list）的 digest：官方与各镜像站返回的
	// 都是它。下面拉具体平台 manifest 只用于取 layer，不能覆盖这个值。
	res.Digest = digest
	if expectedDigest != "" && digest != "" {
		if digest == expectedDigest {
			res.DigestOK = 1
		} else {
			res.DigestOK = 0
		}
	}

	// 多架构索引 → 取 linux/amd64 的具体 manifest
	if strings.Contains(mtype, "index") || strings.Contains(mtype, "list") || mf["manifests"] != nil {
		d := pickPlatformDigest(mf)
		if d != "" {
			if mf2, _, code2, _, err2 := fetchManifest(ctx, client, base, tok, probeImage+"/manifests/"+d); err2 == nil && code2 == 200 {
				mf = mf2
			}
		}
	}

	res.OK = true
	res.Detail = fmt.Sprintf("%d 层", len(layerDigests(mf)))
	res.LatencyMS = time.Since(t0).Milliseconds()

	if tier >= 3 {
		if d := firstLayerDigest(mf); d != "" {
			n, err := fetchBlob(ctx, client, base, tok, d)
			if err == nil && n > 0 {
				el := time.Since(t0).Seconds()
				if el > 0 {
					res.SpeedKBps = int64(float64(n) / 1024 / el)
				}
				res.Detail = fmt.Sprintf("%.1f MB 实测", float64(n)/1024/1024)
			}
		}
	}
	return res
}

func fetchToken(ctx context.Context, c *http.Client, ch challenge) (string, int, error) {
	u, err := url.Parse(ch.Realm)
	if err != nil {
		return "", 0, err
	}
	q := u.Query()
	if ch.Service != "" {
		q.Set("service", ch.Service)
	}
	if ch.Scope != "" {
		q.Set("scope", ch.Scope)
	}
	u.RawQuery = q.Encode()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", resp.StatusCode, nil
	}
	var v struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&v); err != nil {
		return "", resp.StatusCode, err
	}
	if v.Token != "" {
		return v.Token, 200, nil
	}
	return v.AccessToken, 200, nil
}

func fetchManifest(ctx context.Context, c *http.Client, base, tok, path string) (map[string]any, string, int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v2/"+path, nil)
	if err != nil {
		return nil, "", 0, "", err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", manifestAccept)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, "", 0, "", err
	}
	defer resp.Body.Close()
	raw, err := readCapped(resp.Body, 4<<20)
	if err != nil {
		return nil, "", resp.StatusCode, "", err
	}
	if resp.StatusCode != 200 {
		return nil, "", resp.StatusCode, resp.Header.Get("Content-Type"), nil
	}
	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "text/html") {
		return nil, "", resp.StatusCode, ct, errors.New("manifest 位置返回 HTML")
	}
	var mf map[string]any
	if err := json.Unmarshal(raw, &mf); err != nil {
		return nil, "", resp.StatusCode, ct, err
	}
	digest := resp.Header.Get("Docker-Content-Digest")
	if digest == "" {
		sum := sha256.Sum256(raw)
		digest = "sha256:" + hex.EncodeToString(sum[:])
	}
	return mf, digest, resp.StatusCode, ct, nil
}

func pickPlatformDigest(mf map[string]any) string {
	arr, _ := mf["manifests"].([]any)
	var fallback string
	for _, it := range arr {
		m, _ := it.(map[string]any)
		if m == nil {
			continue
		}
		d, _ := m["digest"].(string)
		if d == "" {
			continue
		}
		if fallback == "" {
			fallback = d
		}
		p, _ := m["platform"].(map[string]any)
		if p == nil {
			continue
		}
		osv, _ := p["os"].(string)
		arch, _ := p["architecture"].(string)
		if osv == "linux" && arch == "amd64" {
			return d
		}
	}
	return fallback
}

func layerDigests(mf map[string]any) []string {
	arr, _ := mf["layers"].([]any)
	var out []string
	for _, it := range arr {
		m, _ := it.(map[string]any)
		if m == nil {
			continue
		}
		if d, _ := m["digest"].(string); d != "" {
			out = append(out, d)
		}
	}
	return out
}

func firstLayerDigest(mf map[string]any) string {
	l := layerDigests(mf)
	if len(l) == 0 {
		return ""
	}
	return l[0]
}

// fetchBlob 下载最多 4 MB，返回读到的字节数。
func fetchBlob(ctx context.Context, c *http.Client, base, tok, digest string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v2/"+probeImage+"/blobs/"+digest, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", userAgent)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return 0, fmt.Errorf("blob %d", resp.StatusCode)
	}
	n, _ := io.CopyN(io.Discard, resp.Body, 4<<20)
	if n == 0 {
		return 0, errors.New("empty blob")
	}
	return n, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
