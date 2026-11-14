CREATE TABLE audit (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  ts INTEGER NOT NULL,
  ip TEXT NOT NULL DEFAULT '',
  user TEXT NOT NULL DEFAULT '',
  action TEXT NOT NULL,
  target TEXT NOT NULL DEFAULT '',
  detail TEXT NOT NULL DEFAULT '',
  ok INTEGER NOT NULL DEFAULT 1
);
CREATE TABLE blob_integrity_failures(
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			expected_digest TEXT NOT NULL,
			actual_digest TEXT NOT NULL,
			digest_source TEXT NOT NULL,
			protocol TEXT NOT NULL,
			source_id INTEGER,
			source_name TEXT NOT NULL DEFAULT '',
			cache_key TEXT NOT NULL DEFAULT '',
			error_code TEXT NOT NULL,
			created_at INTEGER NOT NULL);
CREATE TABLE blobs(
			sha256 TEXT PRIMARY KEY,
			size INTEGER NOT NULL DEFAULT 0,
			integrity_status TEXT NOT NULL DEFAULT 'unknown',
			expected_digest TEXT NOT NULL DEFAULT '',
			actual_digest TEXT NOT NULL DEFAULT '',
			digest_source TEXT NOT NULL DEFAULT '',
			digest_algorithm TEXT NOT NULL DEFAULT '',
			verified_at INTEGER NOT NULL DEFAULT 0,
			verification_error TEXT NOT NULL DEFAULT '');
CREATE TABLE cache_entries(
			key TEXT PRIMARY KEY,
			sha256 TEXT NOT NULL,
			size INTEGER NOT NULL,
			content_type TEXT NOT NULL DEFAULT '',
			etag TEXT NOT NULL DEFAULT '',
			last_modified TEXT NOT NULL DEFAULT '',
			upstream TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL,
			last_hit INTEGER NOT NULL DEFAULT 0,
			hits INTEGER NOT NULL DEFAULT 0,
			ttl INTEGER NOT NULL DEFAULT 300,
			verify TEXT NOT NULL DEFAULT '');
CREATE TABLE checks (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  source_id INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
  kind TEXT NOT NULL DEFAULT '',
  ts INTEGER NOT NULL,
  tier INTEGER NOT NULL,
  ok INTEGER NOT NULL,
  latency_ms INTEGER NOT NULL DEFAULT 0,
  speed_kbps INTEGER NOT NULL DEFAULT 0,
  digest TEXT NOT NULL DEFAULT '',
  digest_ok INTEGER NOT NULL DEFAULT -1,
  blob_ok INTEGER NOT NULL DEFAULT -1,
  ttfb_ms INTEGER NOT NULL DEFAULT 0,
  tls_ok INTEGER NOT NULL DEFAULT 1,
  cert_days INTEGER NOT NULL DEFAULT 0,
  http_code INTEGER NOT NULL DEFAULT 0,
  error TEXT NOT NULL DEFAULT ''
);
CREATE TABLE live_speed (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	source_id INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
	kind TEXT NOT NULL DEFAULT '',
	ts INTEGER NOT NULL,
	bytes INTEGER NOT NULL,
	window_s INTEGER NOT NULL
);
CREATE TABLE meta(
			k TEXT PRIMARY KEY,
			v TEXT NOT NULL DEFAULT '');
CREATE TABLE pending_blobs(
			sha TEXT PRIMARY KEY,
			queued_at INTEGER NOT NULL);
CREATE TABLE proxy_speed (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  source_id INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
  kind TEXT NOT NULL DEFAULT '',
  ts INTEGER NOT NULL,
  bytes INTEGER NOT NULL,
  ms INTEGER NOT NULL
);
CREATE TABLE sessions (
  token TEXT PRIMARY KEY,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL
);
CREATE TABLE settings (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE source_events(
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			source_id INTEGER NOT NULL,
			kind TEXT NOT NULL DEFAULT '',
			ts INTEGER NOT NULL,
			ok INTEGER NOT NULL,
			ms INTEGER NOT NULL DEFAULT 0,
			code INTEGER NOT NULL DEFAULT 0);
CREATE TABLE source_groups(
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL DEFAULT '',
		rank INTEGER NOT NULL DEFAULT 0
	);
CREATE TABLE source_health (
  source_id INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
  family TEXT NOT NULL,
  fails INTEGER NOT NULL DEFAULT 0,
  ok_at INTEGER NOT NULL DEFAULT 0,
  last_at INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(source_id, family)
);
CREATE TABLE sources (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  base_url TEXT NOT NULL,
  name TEXT NOT NULL,
  site_name TEXT NOT NULL DEFAULT '',
  origin TEXT NOT NULL DEFAULT 'manual',
  enabled INTEGER NOT NULL DEFAULT 1,
  status TEXT NOT NULL DEFAULT 'pending',
  note TEXT NOT NULL DEFAULT '',
  docker_cap INTEGER NOT NULL DEFAULT -1,
  github_cap INTEGER NOT NULL DEFAULT -1,
  git_cap INTEGER NOT NULL DEFAULT 0,
  status_at INTEGER NOT NULL DEFAULT 0,
  status_reason TEXT NOT NULL DEFAULT '',
  probe_at INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL, group_id INTEGER NOT NULL DEFAULT 0, preferred INTEGER NOT NULL DEFAULT 0, ghcr_cap INTEGER NOT NULL DEFAULT -1, ghcr_prefix INTEGER NOT NULL DEFAULT 0, trusted INTEGER NOT NULL DEFAULT 0, range_at INTEGER NOT NULL DEFAULT 0, range_cap INTEGER NOT NULL DEFAULT -1, pin_state TEXT NOT NULL DEFAULT '', pin_ipv4 TEXT NOT NULL DEFAULT '', pin_host TEXT NOT NULL DEFAULT '', pin_confirmed_at INTEGER NOT NULL DEFAULT 0, pin_grace_until INTEGER NOT NULL DEFAULT 0, pin_ipv6 TEXT NOT NULL DEFAULT '', pin_at INTEGER NOT NULL DEFAULT 0,
  UNIQUE(base_url)
);
CREATE TABLE tokens(
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			hash TEXT NOT NULL UNIQUE,
			prefix TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			last_used INTEGER NOT NULL DEFAULT 0,
			revoked INTEGER NOT NULL DEFAULT 0);
CREATE TABLE traffic_stats(
			day TEXT NOT NULL,
			traffic TEXT NOT NULL,
			kind TEXT NOT NULL DEFAULT '',
			n INTEGER NOT NULL DEFAULT 0,
			bytes INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY(day, traffic, kind));
CREATE TABLE trusted_relays (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL DEFAULT '',
  url TEXT NOT NULL,
  enabled INTEGER NOT NULL DEFAULT 1,
  created_at INTEGER NOT NULL,
  UNIQUE(url)
);
CREATE INDEX idx_audit_ts ON audit(ts DESC);
CREATE INDEX idx_blob_integrity_failures_created
			ON blob_integrity_failures(created_at);
CREATE INDEX idx_blobs_integrity ON blobs(integrity_status);
CREATE INDEX idx_cache_hit ON cache_entries(last_hit);
CREATE INDEX idx_cache_sha ON cache_entries(sha256);
CREATE INDEX idx_checks_kind_ts ON checks(kind, ts DESC);
CREATE INDEX idx_checks_src_ts ON checks(source_id, ts DESC);
CREATE INDEX idx_live_speed_src ON live_speed(source_id, ts DESC);
CREATE INDEX idx_proxy_speed_src ON proxy_speed(source_id, ts DESC);
CREATE INDEX idx_sev_src_ts ON source_events(source_id, ts DESC);
