#!/usr/bin/env bash
# Run only on the authorised rehearsal host. No SSH, live paths, or implicit cleanup.
# TAG=sr9-$(date -u +%Y%m%dT%H%M%SZ) IMAGE=sha256:<64hex> \
# DUMP=<fresh.dump> DUMP_SHA256=<verified hash> CFG=<snapshot.tgz> CFG_SHA256=<hash> \
# bash rehearse-stream-recovery.sh run
# TAG=<same> bash rehearse-stream-recovery.sh verify|status|cleanup
set -Eeuo pipefail
umask 077
export MSYS_NO_PATHCONV=1
HERE="$(cd -- "$(dirname -- "$0")" && pwd -P)"
ACTION="${1:-help}"
TAG="${TAG:-}"
BASE=/www/rehearsals/sub2api
PORT="${PORT:-18087}"
PG_IMAGE=docker.io/library/postgres@sha256:d3e1620b530c944afa6e887d22eb899824da68e19c52024bf98f5220c88a65b2
REDIS_IMAGE=docker.io/library/redis@sha256:becdda6c7f4b3fb42e42fd7f120bbf5c54c4caaaf16f26da24e4563d2c1f0576
GO_IMAGE=docker.io/library/golang:1.27.2-alpine@sha256:738d1cf061836894ff6bb8c33881080ac66de8cf0586615012a0c8f592649cfa
fail() { printf '%s\n' "$1" >&2; exit 1; }
if [[ "$ACTION" == help ]]; then
  printf '%s\n' 'Actions: run (new TAG + fixed IMAGE + fresh DUMP/CFG and SHA256s), verify, status, cleanup.' 'All evidence is retained. cleanup removes only matching resources in this run ledger.'
  exit 0
fi
[[ "$ACTION" =~ ^(run|verify|status|cleanup)$ ]] || fail 'Unknown action'
[[ "$TAG" =~ ^sr9-[0-9]{8}T[0-9]{6}Z$ ]] || fail 'TAG must be sr9-YYYYMMDDTHHMMSSZ'
[[ "$PORT" =~ ^[0-9]+$ ]] && (( PORT >= 1024 && PORT <= 65535 )) || fail 'Invalid loopback port'
ROOT="$BASE/$TAG"
NET="$TAG-net" PG="$TAG-pg" RD="$TAG-redis" STUB="$TAG-stub" APP="$TAG-app"
PGV="$TAG-pgdata" APPV="$TAG-appdata" BUILD="$TAG-build" COPY="$TAG-copy"
command -v docker >/dev/null && command -v python3 >/dev/null || fail 'docker and host python3 are required'

# Ledger stores immutable IDs, not a prefix-based search. Ownership mismatch is fatal.
owned() {
  local kind="$1" name="$2" actual
  actual="$(docker "$kind" inspect -f '{{index .Config.Labels "sr9.owner"}}' "$name" 2>/dev/null || true)"
  if [[ "$kind" != container ]]; then
    actual="$(docker "$kind" inspect -f '{{index .Labels "sr9.owner"}}' "$name" 2>/dev/null || true)"
  fi
  [[ "$actual" == "$OWNER" ]]
}
record() {
  local kind="$1" name="$2" id
  owned "$kind" "$name" || fail 'Created resource has wrong ownership'
  if [[ "$kind" == volume ]]; then id="$name"; else id="$(docker "$kind" inspect -f '{{.Id}}' "$name")"; fi
  printf '%s\t%s\t%s\n' "$kind" "$name" "$id" >> "$ROOT/resources.tsv"
}
need_root() {
  [[ -d "$ROOT" && ! -L "$ROOT" && -f "$ROOT/manifest.json" && ! -L "$ROOT/manifest.json" ]] || fail 'Run manifest missing or unsafe'
  OWNER="$(python3 - "$ROOT/manifest.json" "$TAG" <<'PY'
import json,sys
m=json.load(open(sys.argv[1])); assert m['tag']==sys.argv[2]; assert len(m['owner'])==48
print(m['owner'])
PY
)"
}
evidence() {
  local rc=$?
  trap - EXIT INT TERM
  for name in "$APP" "$STUB" "$PG" "$RD"; do
    if owned container "$name"; then
      docker logs "$name" > "$ROOT/$name.log" 2>&1 || true
      docker inspect -f '{{json .State}}' "$name" > "$ROOT/$name.state.json" 2>/dev/null || true
    fi
  done
  printf '{"shell_exit_code":%d,"remote_validation_ran":true,"evidence_retained":true}\n' "$rc" > "$ROOT/run-exit.json"
  printf 'exit=%s evidence=%s\n' "$rc" "$ROOT"
  exit "$rc"
}
q() {
  owned container "$PG" || fail 'SQL target ownership mismatch'
  docker exec -i "$PG" psql -X -U sub2api -d sub2api -v ON_ERROR_STOP=1 -Atq
}

if [[ "$ACTION" == status ]]; then
  need_root
  python3 - "$ROOT" <<'PY'
import json,sys
from pathlib import Path
r=Path(sys.argv[1]); m=json.load(open(r/'manifest.json'))
result=json.load(open(r/'report.json')) if (r/'report.json').exists() else {'status':'not_verified'}
print(json.dumps({'tag':m['tag'],'cases':len(m['cases']),'report_status':result.get('status'),'passed':result.get('passed'),'failed':result.get('failed'),'evidence':str(r)}))
PY
  exit 0
fi
if [[ "$ACTION" == cleanup ]]; then
  need_root
  [[ -f "$ROOT/resources.tsv" && ! -L "$ROOT/resources.tsv" ]] || fail 'Resource ledger missing'
  # Containers first, network/volumes last; each removal validates the original ID + owner.
  python3 - "$ROOT/resources.tsv" "$OWNER" "$TAG" <<'PY'
import subprocess,json,sys
rows=[x.split('\t') for x in open(sys.argv[1]).read().splitlines() if x]
rows.sort(key=lambda x: {'container':0,'network':1,'volume':2}[x[0]])
for kind,name,rid in rows:
    assert kind in ('container','network','volume') and name.startswith(sys.argv[3]+'-')
    p=subprocess.run(['docker',kind,'inspect',name],capture_output=True,text=True)
    if p.returncode:
        if 'No such' in p.stderr or 'not found' in p.stderr: continue
        raise RuntimeError('resource inspection failed; cleanup stopped')
    obj=json.loads(p.stdout)[0]
    labels=obj.get('Config',{}).get('Labels') if kind=='container' else obj.get('Labels')
    assert (labels or {}).get('sr9.owner')==sys.argv[2], 'ownership mismatch; cleanup stopped'
    assert (obj['Name'] if kind=='volume' else obj['Id'])==rid, 'ID changed; cleanup stopped'
    subprocess.run(['docker',kind,'rm']+(['-f'] if kind=='container' else [])+[name],check=True,stdout=subprocess.DEVNULL)
print(json.dumps({'cleanup':'owned_resources_removed','evidence_retained':True}))
PY
  exit 0
fi
if [[ "$ACTION" == verify ]]; then
  need_root
  owned container "$APP" && owned container "$PG" && owned container "$RD" || fail 'Run resources missing or wrong owner'
  python3 "$HERE/verify.py" --root "$ROOT"
  exit $?
fi

[[ "${IMAGE:-}" =~ ^sha256:[0-9a-f]{64}$ || "${IMAGE:-}" =~ ^[^[:space:]]+@sha256:[0-9a-f]{64}$ ]] || fail 'IMAGE must be fixed local image ID or repository digest'
: "${DUMP:?fresh DUMP required}" "${CFG:?CFG required}" "${DUMP_SHA256:?verified DUMP_SHA256 required}" "${CFG_SHA256:?verified CFG_SHA256 required}"
for image in "$IMAGE" "$PG_IMAGE" "$REDIS_IMAGE" "$GO_IMAGE"; do docker image inspect "$image" >/dev/null 2>&1 || fail 'Required fixed image is not cached; no pull permitted'; done
[[ "$(docker image inspect -f '{{.Id}}' "$IMAGE")" == sha256:* ]] || fail 'Candidate image inspection failed'
[[ ! -e "$ROOT" && ! -L "$ROOT" ]] || fail 'Run directory already exists; choose a new UTC TAG'
for path in /www /www/rehearsals "$BASE"; do [[ ! -L "$path" ]] || fail 'Rehearsal parent may not be symlink'; done
for name in "$PG" "$RD" "$STUB" "$APP" "$BUILD" "$COPY"; do
  if docker container inspect "$name" >/dev/null 2>&1; then fail 'Container name exists; choose new TAG'; fi
done
for name in "$PGV" "$APPV"; do if docker volume inspect "$name" >/dev/null 2>&1; then fail 'Volume name exists; choose new TAG'; fi; done
if docker network inspect "$NET" >/dev/null 2>&1; then fail 'Network name exists; choose new TAG'; fi
# Check host bind before creating anything; do not evict another listener.
python3 - "$PORT" <<'PY'
import socket,sys
s=socket.socket();s.bind(('127.0.0.1',int(sys.argv[1])));s.close()
PY
mkdir -p -- "$BASE"
mkdir -m 700 -- "$ROOT"
python3 "$HERE/fixture.py" --root "$ROOT" --tag "$TAG" --image "$IMAGE" --dump "$DUMP" --dump-sha256 "$DUMP_SHA256" --cfg "$CFG" --cfg-sha256 "$CFG_SHA256" --port "$PORT" --max-dump-age "${MAX_DUMP_AGE_SECONDS:-21600}" > "$ROOT/snapshots.json"
need_root
trap evidence EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
: > "$ROOT/resources.tsv"
LABELS=(--label "sr9.owner=$OWNER" --label "sr9.tag=$TAG")
printf 'preparing=%s\n' "$ROOT"
python3 - "$ROOT/external-containers.json" <<'PY'
import subprocess,json,sys,os
names=subprocess.check_output(['docker','container','ls','-a','--format','{{.Names}}'],text=True).splitlines()
state={}
for name in names:
    o=json.loads(subprocess.check_output(['docker','container','inspect',name],text=True))[0]
    state[name]={'id':o['Id'],'image':o['Image'],'running':o['State']['Running'],'restart_policy':o['HostConfig']['RestartPolicy']}
with open(sys.argv[1],'w') as f: json.dump(state,f)
os.chmod(sys.argv[1],0o600)
PY
mkdir -m 700 "$ROOT/src" "$ROOT/stub-out"
cp -- "$HERE/stub.go" "$ROOT/src/main.go"
# Compile with stdlib only, without network or module downloads. Retain the build container.
docker create --pull never --name "$BUILD" "${LABELS[@]}" --network none -e CGO_ENABLED=0 -e GOPROXY=off -e GOSUMDB=off -e GOTOOLCHAIN=local -e GOCACHE=/tmp/go-cache -e GO111MODULE=off -v "$ROOT/src:/src" -w /src "$GO_IMAGE" go build -trimpath -o /src/stub /src/main.go >/dev/null
record container "$BUILD"
docker start -a "$BUILD" > "$ROOT/build.log" 2>&1
[[ "$(docker inspect -f '{{.State.ExitCode}}' "$BUILD")" == 0 && -x "$ROOT/src/stub" ]] || fail 'Stub compilation failed; inspect retained build.log'
docker network create --internal "${LABELS[@]}" "$NET" >/dev/null; record network "$NET"
[[ "$(docker network inspect -f '{{.Internal}}' "$NET")" == true ]] || fail 'Network is not internal'
for v in "$PGV" "$APPV"; do docker volume create "${LABELS[@]}" "$v" >/dev/null; record volume "$v"; done
# Copy sanitised fixture config into only this run's app volume.
docker create --pull never --name "$COPY" "${LABELS[@]}" --network none -v "$APPV:/data" -v "$ROOT/appdata:/fixture:ro" --entrypoint /bin/sh "$PG_IMAGE" -c 'cp /fixture/config.yaml /fixture/.installed /data/ && chmod 600 /data/config.yaml /data/.installed' >/dev/null
record container "$COPY"; docker start -a "$COPY" > "$ROOT/config-copy.log" 2>&1
[[ "$(docker inspect -f '{{.State.ExitCode}}' "$COPY")" == 0 ]] || fail 'Config copy failed'
docker create --pull never --name "$PG" "${LABELS[@]}" --network "$NET" --env-file "$ROOT/pg.env" -v "$PGV:/var/lib/postgresql/data" "$PG_IMAGE" >/dev/null
record container "$PG"; docker start "$PG" >/dev/null
docker create --pull never --name "$RD" "${LABELS[@]}" --network "$NET" --env-file "$ROOT/redis.env" -v "$ROOT/redis.conf:/run/redis-fixture.conf:ro" --user 0:0 --entrypoint redis-server "$REDIS_IMAGE" /run/redis-fixture.conf >/dev/null
record container "$RD"; docker start "$RD" >/dev/null
# Readiness checks are bounded; no grep-based success detector.
# PG's first-boot initdb performs a fast shutdown and restart of a temporary
# instance; a socket-only pg_isready succeeds against that temp server and the
# following pg_restore then hits "database system is shutting down". Require a
# real TCP query to succeed twice in a row before touching the database.
python3 - "$PG" "$RD" <<'PY'
import subprocess,time,sys
streak=0
for _ in range(180):
    pg=subprocess.run(['docker','exec',sys.argv[1],'pg_isready','-h','127.0.0.1','-U','sub2api','-d','sub2api'],capture_output=True)
    rd=subprocess.run(['docker','exec',sys.argv[2],'/bin/sh','-c','REDISCLI_AUTH="$REDIS_PASSWORD" redis-cli ping'],capture_output=True)
    if pg.returncode==0 and rd.returncode==0 and rd.stdout.strip()==b'PONG':
        probe=subprocess.run(['docker','exec',sys.argv[1],'psql','-h','127.0.0.1','-U','sub2api','-d','sub2api','-Atq','-c','SELECT 1'],capture_output=True)
        streak=streak+1 if probe.returncode==0 and probe.stdout.strip()==b'1' else 0
        if streak>=2: break
    else:
        streak=0
    time.sleep(1)
else: raise RuntimeError('isolated database readiness failed')
PY
# Refuse a non-PG18 runtime and inspect dump TOC before isolated restore.
docker exec -i "$PG" pg_restore --list < "$DUMP" > "$ROOT/dump-toc.txt" 2> "$ROOT/dump-toc.err"
printf 'SELECT current_setting('\''server_version_num'\'')::int / 10000 = 18;\n' | q > "$ROOT/pg-version.txt"
python3 - "$ROOT/pg-version.txt" <<'PY'
import sys
assert open(sys.argv[1]).read().strip()=='t', 'PG runtime must be 18'
PY
docker exec -i "$PG" pg_restore --exit-on-error --no-owner --no-privileges -U sub2api -d sub2api < "$DUMP" > "$ROOT/restore.log" 2>&1
q < "$ROOT/seed.sql" > "$ROOT/seed.log" 2>&1
printf 'SELECT count(*) FROM sr9.cases;\n' | q > "$ROOT/seed-count.txt"
python3 - "$ROOT/seed-count.txt" "$ROOT/manifest.json" <<'PY'
import json,sys
assert int(open(sys.argv[1]).read())==len(json.load(open(sys.argv[2]))['cases'])
PY
# Stub shares only the internal network; no externally callable test-control port.
docker create --pull never --name "$STUB" "${LABELS[@]}" --network "$NET" --read-only --user 0:0 --memory 256m --entrypoint /src/stub -e STUB_CONFIG=/run/stub-config.json -e STUB_EVIDENCE=/out/requests.jsonl -v "$ROOT/src:/src:ro" -v "$ROOT/stub-config.json:/run/stub-config.json:ro" -v "$ROOT/stub-out:/out" "$PG_IMAGE" >/dev/null
record container "$STUB"; docker start "$STUB" >/dev/null
docker create --pull never --name "$APP" "${LABELS[@]}" --network "$NET" --memory 2g --memory-swap 2g --read-only --tmpfs /tmp:rw,noexec,nosuid,size=512m --env-file "$ROOT/app.env" -v "$APPV:/app/data" -p "127.0.0.1:$PORT:8080" "$IMAGE" >/dev/null
record container "$APP"; docker start "$APP" >/dev/null
python3 "$HERE/verify.py" --root "$ROOT"
