-- 3.5.18 阶段10/11 测试夹具：两个可信副本源指向本地自建 registry:2
-- 权威 quay.io/library/alpine -> 副本 registry.gitlab.com(源 reg-a) + registry.k8s.io(源 reg-b)
PRAGMA foreign_keys=ON;
BEGIN;

DELETE FROM trusted_source_mappings;
DELETE FROM source_host_caps;
DELETE FROM registry_host_recovery;
DELETE FROM source_realm_policy;
DELETE FROM trusted_registry_replicas;
DELETE FROM sources WHERE origin='fixture';
DELETE FROM registry_host_gen;

-- 两个 Source，base_url 指向本地真实 registry
INSERT INTO sources(base_url,name,origin,enabled,status,created_at,preferred,trusted)
 VALUES('http://ml318-reg-a:5000','fixture-reg-a','fixture',1,'usable',strftime('%s','now'),1,1);
INSERT INTO sources(base_url,name,origin,enabled,status,created_at,preferred,trusted)
 VALUES('http://ml318-reg-b:5000','fixture-reg-b','fixture',1,'usable',strftime('%s','now'),1,1);

-- host gen（cred_digest 空）
INSERT INTO registry_host_gen(host,gen,cred_digest,updated_at) VALUES('registry.gitlab.com',1,'',strftime('%s','now'));
INSERT INTO registry_host_gen(host,gen,cred_digest,updated_at) VALUES('registry.k8s.io',1,'',strftime('%s','now'));

-- source_realm_policy: 自建 registry 匿名可读，realm 仅为格式占位（不会被实际触发）
INSERT INTO source_realm_policy(source_id,host,primary_realm,extra_realms,policy_gen,updated_at)
 SELECT id,'registry.gitlab.com','https://registry.gitlab.com','[]',1,strftime('%s','now') FROM sources WHERE name='fixture-reg-a';
INSERT INTO source_realm_policy(source_id,host,primary_realm,extra_realms,policy_gen,updated_at)
 SELECT id,'registry.k8s.io','https://registry.k8s.io','[]',1,strftime('%s','now') FROM sources WHERE name='fixture-reg-b';

-- source_host_caps: Supported/native，gen 由 recovery 记录
INSERT INTO source_host_caps(source_id,host,enabled,last_verdict,effective_status,registry_path_mode,registry_prefix,diagnostics)
 SELECT id,'registry.gitlab.com',1,'Supported','Supported','native',0,'fixture' FROM sources WHERE name='fixture-reg-a';
INSERT INTO source_host_caps(source_id,host,enabled,last_verdict,effective_status,registry_path_mode,registry_prefix,diagnostics)
 SELECT id,'registry.k8s.io',1,'Supported','Supported','native',0,'fixture' FROM sources WHERE name='fixture-reg-b';

-- recovery: state=ok, gen=1, last_success_at 新鲜
INSERT INTO registry_host_recovery(source_id,family,scope,state,gen,last_probe_at,last_success_at,next_probe_at,rec_failures,retry_failures,updated_at)
 SELECT id,'registry','registry.gitlab.com','ok',1,strftime('%s','now'),strftime('%s','now'),0,0,0,strftime('%s','now') FROM sources WHERE name='fixture-reg-a';
INSERT INTO registry_host_recovery(source_id,family,scope,state,gen,last_probe_at,last_success_at,next_probe_at,rec_failures,retry_failures,updated_at)
 SELECT id,'registry','registry.k8s.io','ok',1,strftime('%s','now'),strftime('%s','now'),0,0,0,strftime('%s','now') FROM sources WHERE name='fixture-reg-b';

-- trusted_source_mappings: 副本 host -> Source
INSERT INTO trusted_source_mappings(registry_host,source_id,priority,valid_from,valid_until,enabled)
 SELECT 'registry.gitlab.com',id,100,0,0,1 FROM sources WHERE name='fixture-reg-a';
INSERT INTO trusted_source_mappings(registry_host,source_id,priority,valid_from,valid_until,enabled)
 SELECT 'registry.k8s.io',id,100,0,0,1 FROM sources WHERE name='fixture-reg-b';

-- trusted_registry_replicas: 权威 quay.io/library/alpine -> 两个副本
INSERT INTO trusted_registry_replicas(authority_registry,authority_repository,replica_registry,replica_repository,priority,valid_from,valid_until,enabled)
 VALUES('docker.io','library/alpine','registry.gitlab.com','library/alpine',100,0,0,1);
INSERT INTO trusted_registry_replicas(authority_registry,authority_repository,replica_registry,replica_repository,priority,valid_from,valid_until,enabled)
 VALUES('docker.io','library/alpine','registry.k8s.io','library/alpine',200,0,0,1);

-- trusted generation（映射代次，运行时可能要求）
INSERT OR REPLACE INTO trusted_replica_generation(authority_registry,generation) VALUES('docker.io',1);
INSERT OR REPLACE INTO trusted_mapping_generation(registry_host,generation) VALUES('registry.gitlab.com',1);
INSERT OR REPLACE INTO trusted_mapping_generation(registry_host,generation) VALUES('registry.k8s.io',1);

-- B 配置：enabled=true, mode=sequential, sort=fixed
INSERT OR REPLACE INTO settings(key,value) VALUES('cross_registry_enabled','true');
INSERT OR REPLACE INTO settings(key,value) VALUES('cross_registry_mode','sequential');
INSERT OR REPLACE INTO settings(key,value) VALUES('cross_registry_sort_mode','fixed');

COMMIT;
SELECT 'sources',count(*) FROM sources;
SELECT 'caps',count(*) FROM source_host_caps;
SELECT 'recovery',count(*) FROM registry_host_recovery;
SELECT 'replicas',count(*) FROM trusted_registry_replicas;
SELECT 'mappings',count(*) FROM trusted_source_mappings;
