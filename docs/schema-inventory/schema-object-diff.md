# Schema 对象差异（3.4.6 → 3.5.4）

3.4.6 表数: 19；3.5.4 表数: 25

## 表
- 新增 `breaker_state`（3.5.4，2 行）
- 新增 `registry_host_gen`（3.5.4，0 行）
- 新增 `registry_host_recovery`（3.5.4，0 行）
- 新增 `schema_migrations`（3.5.4，3 行）
- 新增 `source_health_archive`（3.5.4，0 行）
- 新增 `source_health_scope`（3.5.4，0 行）
- `source_events`: 新增列 ['neutral']；删除列 无
- `sources`: 新增列 ['registry_cap', 'registry_identity_state', 'registry_path_mode', 'registry_probe_at', 'registry_probe_error', 'repo_host']；删除列 无

## 行数
| 表 | 3.4.6 | 3.5.4 |
|---|---:|---:|
| _writable | 0 | 0 |
| audit | 167 | 167 |
| blob_integrity_failures | 0 | 0 |
| blobs | 392 | 476 |
| breaker_state | - | 2 |
| cache_entries | 183 | 346 |
| checks | 164369 | 190670 |
| live_speed | 30 | 30 |
| meta | 1 | 2 |
| pending_blobs | 0 | 0 |
| proxy_speed | 160 | 181 |
| registry_host_gen | - | 0 |
| registry_host_recovery | - | 0 |
| schema_migrations | - | 3 |
| sessions | 4 | 2 |
| settings | 10 | 10 |
| source_events | 2976 | 3169 |
| source_groups | 2 | 2 |
| source_health | 255 | 255 |
| source_health_archive | - | 0 |
| source_health_scope | - | 0 |
| sources | 91 | 91 |
| tokens | 1 | 1 |
| traffic_stats | 54 | 65 |
| trusted_relays | 0 | 0 |
