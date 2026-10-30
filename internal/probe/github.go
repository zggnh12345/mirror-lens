package probe

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// 固定的校验样本：内容永不变化，便于跨源比对哈希。
const (
	ghSmallURL  = "https://raw.githubusercontent.com/octocat/Hello-World/master/README"
	ghSmallHash = "03ba204e50d126e4674c005e04d82e84c21366780af1f43bd54a37816b6ab340"

	ghBigURL  = "https://github.com/stedolan/jq/releases/download/jq-1.6/jq-linux64"
	ghBigHash = "af986793a515d500ab2d35f8d2aecd656e764504b789b66d7e1a0b727a124c44"
	ghBigSize = 3953824
)

// GitHub probes one GitHub proxy. tier 1 = liveness, 2 = verified small file,
// 3 = real release download with hash check and throughput.
func GitHub(ctx context.Context, base string, tier int, expectedHash string) Result {
	base = trimBase(base)
	host := hostOf(base)
	res := Result{DigestOK: -1}

	ok, days, _ := tlsInfo(ctx, host)
	res.TLSOK, res.CertDays = ok, days

	client := newClient(30 * time.Second)
	t0 := time.Now()

	u := base + "/" + ghSmallURL
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
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
	body, _ := readCapped(resp.Body, 1<<20)
	resp.Body.Close()
	res.HTTPCode = resp.StatusCode
	res.LatencyMS = time.Since(t0).Milliseconds()

	ct := resp.Header.Get("Content-Type")
	if resp.StatusCode != 200 {
		res.Error = fmt.Sprintf("raw 文件返回 %d", resp.StatusCode)
		if len(body) > 0 && len(body) < 200 {
			res.Detail = strings.TrimSpace(string(body))
		}
		return res
	}
	if strings.Contains(ct, "text/html") && !strings.Contains(string(body), "Hello World") {
		res.Error = "返回 HTML 页面，不是代理"
		return res
	}

	sum := sha256.Sum256(body)
	got := hex.EncodeToString(sum[:])
	if tier >= 2 {
		if got == ghSmallHash {
			res.DigestOK = 1
		} else {
			res.DigestOK = 0
			res.Error = "raw 文件哈希不符"
		}
		if res.DigestOK == 0 {
			return res
		}
	}
	res.OK = true
	res.Detail = "raw 通过"

	if tier < 3 {
		return res
	}

	// 真实 release 下载
	t1 := time.Now()
	bu := base + "/" + ghBigURL
	req2, err := http.NewRequestWithContext(ctx, http.MethodGet, bu, nil)
	if err != nil {
		res.Error = errStr(err)
		return res
	}
	req2.Header.Set("User-Agent", userAgent)
	resp2, err := client.Do(req2)
	if err != nil {
		res.Error = "release 下载失败: " + errStr(err)
		return res
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 200 {
		res.Error = fmt.Sprintf("release 返回 %d", resp2.StatusCode)
		return res
	}
	h := sha256.New()
	n, cpErr := io.Copy(io.Discard, io.TeeReader(io.LimitReader(resp2.Body, 16<<20), h))
	el := time.Since(t1).Seconds()
	if el > 0 && n > 0 {
		res.SpeedKBps = int64(float64(n) / 1024 / el)
	}
	if n != ghBigSize {
		if cpErr != nil {
			// 网络中断/超时：是"慢到不可用"，不是投毒。
			res.OK = false
			res.DigestOK = -1
			res.Error = fmt.Sprintf("下载中断 %d/%d 字节", n, ghBigSize)
			return res
		}
		res.OK = false
		res.DigestOK = 0
		res.Error = fmt.Sprintf("内容大小异常 %d/%d，疑似被替换", n, ghBigSize)
		return res
	}
	big := hex.EncodeToString(h.Sum(nil))
	want := ghBigHash
	if expectedHash != "" {
		want = expectedHash
	}
	if big == want {
		res.DigestOK = 1
		res.Detail = fmt.Sprintf("%.1f MB 实测", float64(n)/1024/1024)
	} else {
		res.DigestOK = 0
		res.Error = "release 哈希不符，内容可能被替换"
	}
	return res
}
