package logging

// Code 是稳定的错误代码。日志里只写这些常量，不要在调用点拼字符串：
// 一旦有人写成 "upstream_timeout"，按代码聚合的告警就断了。
type Code string

const (
	UpstreamConnectFailed Code = "UPSTREAM_CONNECT_FAILED"
	UpstreamTimeout       Code = "UPSTREAM_TIMEOUT"
	UpstreamRateLimited   Code = "UPSTREAM_RATE_LIMITED"
	UpstreamHTTPError     Code = "UPSTREAM_HTTP_ERROR"
	ContentDigestMismatch Code = "CONTENT_DIGEST_MISMATCH"
	ContentSizeMismatch   Code = "CONTENT_SIZE_MISMATCH"
	SSRFBlocked           Code = "SSRF_BLOCKED"
	CacheCommitFailed     Code = "CACHE_COMMIT_FAILED"
	CacheReadFailed       Code = "CACHE_READ_FAILED"
	AuthTokenInvalid      Code = "AUTH_TOKEN_INVALID"
	AuthTokenRevoked      Code = "AUTH_TOKEN_REVOKED"
)

// String 让 Code 能直接进 slog 的 key-value，不用每次显式转换。
func (c Code) String() string { return string(c) }
