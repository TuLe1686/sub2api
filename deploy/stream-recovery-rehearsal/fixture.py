#!/usr/bin/env python3
"""Offline fixture preparation only; SQL is consumed by the isolated PG container."""
import argparse
import datetime as dt
import hashlib
import json
import os
from pathlib import Path
import secrets
import tarfile
import time

CASES = [
    (f"{route}-{proto}-recover", route, proto, "recover")
    for route in ("responses", "passthrough", "rawchat")
    for proto in ("responses", "chat", "messages")
] + [
    ("heartbeat-longwait", "responses", "responses", "longwait"),
    ("responses-tool-half", "passthrough", "responses", "tool_half"),
    ("responses-tool-handoff", "passthrough", "responses", "tool_handoff"),
    ("responses-custom-namespace", "passthrough", "responses", "custom"),
    ("chat-parallel-namespace", "rawchat", "chat", "parallel"),
    ("responses-reasoning", "passthrough", "responses", "reasoning"),
    ("hosted-readonly-pass", "passthrough", "responses", "readonly"),
    ("hosted-sideeffect-pass", "passthrough", "responses", "sideeffect_pass"),
    ("hosted-sideeffect-no-replay", "passthrough", "responses", "sideeffect_fail"),
    ("three-turn-alias", "passthrough", "responses", "alias"),
]


def private_write(path, text):
    fd = os.open(path, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
    with os.fdopen(fd, "w", encoding="utf-8", newline="\n") as f:
        f.write(text)


def sha(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for block in iter(lambda: f.read(1024 * 1024), b""):
            h.update(block)
    return h.hexdigest()


def prepare(a):
    root = Path(a.root)
    for src, expected in [(a.dump, a.dump_sha256), (a.cfg, a.cfg_sha256)]:
        p = Path(src)
        if not p.is_file() or p.is_symlink() or len(expected) != 64 or sha(p) != expected:
            raise ValueError("snapshot missing, symlinked, or SHA256 mismatch")
    age = time.time() - Path(a.dump).stat().st_mtime
    if age < -300 or age > a.max_dump_age:
        raise ValueError("dump timestamp outside allowed freshness window; acquire fresh snapshot")
    with tarfile.open(a.cfg, "r:gz") as t:
        members = t.getmembers()
        if any(m.issym() or m.islnk() or m.name.startswith("/") or ".." in Path(m.name).parts for m in members):
            raise ValueError("unsafe config archive member")
        names = [Path(m.name).name for m in members if m.isfile()]
        if names.count("config.yaml") != 1 or names.count(".installed") != 1:
            raise ValueError("config archive must contain config.yaml and .installed once")
        installed = next(m for m in members if m.isfile() and Path(m.name).name == ".installed")
        installed_data = t.extractfile(installed).read(1024)
    pg = secrets.token_hex(24)
    jwt = secrets.token_hex(32)
    rd = secrets.token_hex(24)
    owner = secrets.token_hex(24)
    stub_name = a.tag + "-stub"
    manifest = {"tag": a.tag, "owner": owner, "created_at": dt.datetime.now(dt.timezone.utc).isoformat(),
                "image": a.image, "dump_sha256": a.dump_sha256, "cfg_sha256": a.cfg_sha256,
                "dump_age_seconds": round(age), "port": a.port, "root": str(root), "cases": []}
    for name, route, proto, kind in CASES:
        manifest["cases"].append({"name": name, "route": route, "protocol": proto, "kind": kind,
                                  "key": "sk-" + secrets.token_hex(24), "upstream_key_a": secrets.token_hex(24),
                                  "upstream_key_b": secrets.token_hex(24)})
    private_write(root / "manifest.json", json.dumps(manifest, indent=2) + "\n")
    private_write(root / "pg.env", f"POSTGRES_USER=sub2api\nPOSTGRES_DB=sub2api\nPOSTGRES_PASSWORD={pg}\nPGDATA=/var/lib/postgresql/data\n")
    private_write(root / "redis.conf", f"bind 0.0.0.0\nrequirepass {rd}\nsave \"\"\nappendonly no\n")
    private_write(root / "redis.env", f"REDIS_PASSWORD={rd}\n")
    private_write(root / "app.env", f"""SERVER_HOST=0.0.0.0
SERVER_PORT=8080
SERVER_MODE=release
RUN_MODE=standard
DATABASE_HOST={a.tag}-pg
DATABASE_PORT=5432
DATABASE_USER=sub2api
DATABASE_DBNAME=sub2api
DATABASE_PASSWORD={pg}
DATABASE_SSLMODE=disable
REDIS_HOST={a.tag}-redis
REDIS_PORT=6379
REDIS_DB=0
REDIS_PASSWORD={rd}
JWT_SECRET={jwt}
TOKEN_REFRESH_ENABLED=false
SCHEDULED_TEST_TIMEOUT_GUARD_ENABLED=false
SECURITY_URL_ALLOWLIST_ENABLED=false
SECURITY_URL_ALLOWLIST_ALLOW_PRIVATE_HOSTS=true
SECURITY_URL_ALLOWLIST_ALLOW_INSECURE_HTTP=true
OPS_ENABLED=false
OPS_CLEANUP_ENABLED=false
OPS_AGGREGATION_ENABLED=false
USAGE_CLEANUP_ENABLED=false
GATEWAY_OPENAI_CODEX_TICKET_ENABLED=false
GATEWAY_OPENAI_CODEX_TICKET_HARVEST_ENABLED=false
IMAGE_STORAGE_ENABLED=false
GOMEMLIMIT=1843MiB
TZ=Asia/Shanghai
""")
    (root / "appdata").mkdir(mode=0o700)
    private_write(root / "appdata" / ".installed", installed_data.decode("utf-8"))
    # Do not import live database URLs, OAuth or storage credentials from config.yaml.
    private_write(root / "appdata" / "config.yaml", """server:
  host: 0.0.0.0
  port: 8080
run_mode: standard
gateway:
  stream_recovery:
    max_recoveries: 3
    wait_budget_seconds: 90
    heartbeat_interval_seconds: 2
  openai_proxy_stream_circuit:
    disabled: true
  stream_data_interval_timeout: 90
  stream_keepalive_interval: 0
token_refresh:
  enabled: false
security:
  url_allowlist:
    enabled: false
    allow_private_hosts: true
    allow_insecure_http: true
ops:
  cleanup:
    enabled: false
  aggregation:
    enabled: false
""")
    stub_config = {"cases": {}}
    for c in manifest["cases"]:
        for lane in ("a", "b"):
            stub_config["cases"][c["upstream_key_" + lane]] = {"name": c["name"], "kind": c["kind"], "lane": lane}
    private_write(root / "stub-config.json", json.dumps(stub_config) + "\n")
    private_write(root / "seed.sql", seed_sql(manifest, stub_name))
    print(json.dumps({"prepared": True, "cases": len(CASES), "dump_sha256": a.dump_sha256, "dump_age_seconds": round(age)}))


def seed_sql(m, stub_name):
    def sql(v):
        return "'" + str(v).replace("'", "''") + "'"
    lines = ["BEGIN;", "UPDATE accounts SET schedulable=false, status='disabled', updated_at=now();",
             "UPDATE api_keys SET status='disabled', updated_at=now();",
             "UPDATE scheduled_test_plans SET enabled=false;",
             "UPDATE channel_monitors SET enabled=false;",
             "DO $$ DECLARE t record; BEGIN FOR t IN SELECT table_name FROM information_schema.columns WHERE table_schema='public' AND column_name='enabled' AND table_name LIKE 'ops_%' LOOP EXECUTE format('UPDATE %I SET enabled=false',t.table_name); END LOOP; END $$;",
             "UPDATE settings SET value=(value::jsonb || '{\"enabled\":false}'::jsonb)::text, updated_at=now() WHERE key='backup_schedule';",
             # Disable every persisted enabled setting object, including webhooks/alerts.
             "UPDATE settings SET value=(value::jsonb || '{\"enabled\":false}'::jsonb)::text, updated_at=now() WHERE key != 'backup_schedule' AND CASE WHEN value IS JSON OBJECT THEN value::jsonb ? 'enabled' ELSE false END;",
             "DO $$ BEGIN IF EXISTS(SELECT 1 FROM accounts WHERE schedulable) OR EXISTS(SELECT 1 FROM scheduled_test_plans WHERE enabled) OR EXISTS(SELECT 1 FROM settings WHERE key='backup_schedule' AND (value::jsonb->>'enabled')::boolean IS DISTINCT FROM false) THEN RAISE EXCEPTION 'isolation jobs gate failed'; END IF; END $$;",
             "CREATE SCHEMA sr9;", "CREATE TABLE sr9.cases(name text primary key,user_id bigint,group_id bigint,key_id bigint,account_a bigint,account_b bigint);",
             "DO $fixture$ DECLARE uid bigint; gid bigint; kid bigint; aid bigint; bid bigint; BEGIN"]
    for c in m["cases"]:
        n = m["tag"] + "-" + c["name"]
        extra = {"openai_passthrough": c["route"] == "passthrough", "openai_responses_mode": "force_chat_completions" if c["route"] == "rawchat" else "force_responses", "enhanced_control": {"stream_recovery": {"enabled": True, "cooldown_seconds": 0 if c["kind"] == "alias" else 600}}}
        lines += [f"INSERT INTO users(email,password_hash,role,balance,concurrency,status,balance_notify_enabled) VALUES ({sql(n+'@example.invalid')},{sql('login-disabled-synthetic-fixture')},'user',100,5,'active',false) RETURNING id INTO uid;",
                  f"INSERT INTO groups(name,platform,subscription_type,status,rate_multiplier,is_exclusive,allow_messages_dispatch,claude_code_only,model_routing_enabled,profit_control_enabled) VALUES ({sql(n)},'openai','standard','active',1,false,true,false,false,false) RETURNING id INTO gid;",
                  f"INSERT INTO api_keys(user_id,key,name,group_id,status,quota) VALUES (uid,{sql(c['key'])},{sql(n)},gid,'active',100) RETURNING id INTO kid;"]
        for lane, var, priority in [("a", "aid", 1), ("b", "bid", 99)]:
            cred = {"base_url": "http://" + stub_name + ":9000", "api_key": c["upstream_key_" + lane], "model_mapping": {"gpt-5.1": "gpt-5.1"}}
            lines += [f"INSERT INTO accounts(name,platform,type,credentials,extra,concurrency,priority,status,schedulable,auto_pause_on_expired,rate_multiplier) VALUES ({sql(n+'-'+lane)},'openai','apikey',{sql(json.dumps(cred))}::jsonb,{sql(json.dumps(extra))}::jsonb,3,{priority},'active',true,false,1) RETURNING id INTO {var};",
                      f"INSERT INTO account_groups(account_id,group_id,priority) VALUES ({var},gid,{priority});"]
        lines.append(f"INSERT INTO sr9.cases VALUES ({sql(c['name'])},uid,gid,kid,aid,bid);")
    lines += ["END $fixture$;", "COMMIT;"]
    return "\n".join(lines) + "\n"


if __name__ == "__main__":
    p = argparse.ArgumentParser()
    p.add_argument("--root", required=True)
    p.add_argument("--tag", required=True)
    p.add_argument("--image", required=True)
    p.add_argument("--dump", required=True)
    p.add_argument("--dump-sha256", required=True)
    p.add_argument("--cfg", required=True)
    p.add_argument("--cfg-sha256", required=True)
    p.add_argument("--port", type=int, default=18087)
    p.add_argument("--max-dump-age", type=int, default=21600)
    prepare(p.parse_args())
