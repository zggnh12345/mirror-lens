-- A10 竞速配对夹具：quay.io 两个星标源（快=0ms 慢=80ms），真实 HTTP 源返回同一 digest
PRAGMA foreign_keys=ON;
BEGIN;
DELETE FROM trusted_source_mappings;
DELETE FROM source_host_caps;
DELETE FROM registry_host_recovery;
DELETE FROM source_realm_policy;
DELETE FROM trusted_registry_replicas;
DELETE FROM sources WHERE origin='fixture';
DELETE FROM registry_host_gen;

INSERT INTO sources(base_url,name,origin,enabled,status,created_at,preferred,trusted)
 VALUES('http://ml318-src-fast:19001','race-fast','fixture',1,'usable',strftime('%s','now'),1,1);
INSERT INTO sources(base_url,name,origin,enabled,status,created_at,preferred,trusted)
 VALUES('http://ml318-src-slow:19001','race-slow','fixture',1,'usable',strftime('%s','now'),1,1);

INSERT INTO registry_host_gen(host,gen,cred_digest,updated_at) VALUES('quay.io',1,'',strftime('%s','now'));
INSERT INTO source_realm_policy(source_id,host,primary_realm,extra_realms,policy_gen,updated_at)
 SELECT id,'quay.io','https://quay.io','[]',1,strftime('%s','now') FROM sources WHERE name='race-fast';
INSERT INTO source_realm_policy(source_id,host,primary_realm,extra_realms,policy_gen,updated_at)
 SELECT id,'quay.io','https://quay.io','[]',1,strftime('%s','now') FROM sources WHERE name='race-slow';

INSERT INTO source_host_caps(source_id,host,enabled,last_verdict,effective_status,registry_path_mode,registry_prefix,diagnostics)
 SELECT id,'quay.io',1,'Supported','Supported','native',0,'{"version":2,"repo":"library/alpine","tag":"latest"}' FROM sources WHERE name='race-fast';
INSERT INTO source_host_caps(source_id,host,enabled,last_verdict,effective_status,registry_path_mode,registry_prefix,diagnostics)
 SELECT id,'quay.io',1,'Supported','Supported','native',0,'{"version":2,"repo":"library/alpine","tag":"latest"}' FROM sources WHERE name='race-slow';

INSERT INTO registry_host_recovery(source_id,family,scope,state,gen,last_probe_at,last_success_at,next_probe_at,rec_failures,retry_failures,updated_at)
 SELECT id,'registry','quay.io','ok',1,strftime('%s','now'),strftime('%s','now'),0,0,0,strftime('%s','now') FROM sources WHERE name='race-fast';
INSERT INTO registry_host_recovery(source_id,family,scope,state,gen,last_probe_at,last_success_at,next_probe_at,rec_failures,retry_failures,updated_at)
 SELECT id,'registry','quay.io','ok',1,strftime('%s','now'),strftime('%s','now'),0,0,0,strftime('%s','now') FROM sources WHERE name='race-slow';

INSERT INTO trusted_source_mappings(registry_host,source_id,priority,valid_from,valid_until,enabled)
 SELECT 'quay.io',id,100,0,0,1 FROM sources WHERE name='race-fast';
INSERT INTO trusted_source_mappings(registry_host,source_id,priority,valid_from,valid_until,enabled)
 SELECT 'quay.io',id,200,0,0,1 FROM sources WHERE name='race-slow';

COMMIT;
SELECT 'sources',count(*) FROM sources;
