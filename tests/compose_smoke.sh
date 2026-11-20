#!/usr/bin/env bash
set -euo pipefail

export POSTGRES_PASSWORD="$(openssl rand -hex 24)"
export TYPHON_PORT=18081
project="typhon-compose-ci-${GITHUB_RUN_ID:-$$}"
compose=(docker compose --project-name "$project")
url="http://127.0.0.1:$TYPHON_PORT"

cleanup() {
  local status=$?
  if [[ "$status" != 0 ]]; then
    "${compose[@]}" ps >&2
    "${compose[@]}" logs --tail 60 >&2
  fi
  "${compose[@]}" down --volumes --remove-orphans
}
trap cleanup EXIT

wait_ready() {
  for _ in $(seq 1 60); do
    if curl -sf --max-time 2 "$url/health/ready" >/dev/null; then return; fi
    sleep 0.5
  done
  echo 'API readiness did not recover' >&2
  return 1
}

"${compose[@]}" config --quiet
"${compose[@]}" up --build -d
wait_ready

test "$(curl -s -o /dev/null -w '%{http_code}' "$url/health/live")" = 204
curl -fsS "$url/cache" | grep -F 'PONG'
test "$("${compose[@]}" exec -T api id -u)" = 10001
"${compose[@]}" exec -T api sh -c '! command -v go && test -s /etc/ssl/certs/ca-certificates.crt'
docker image inspect -f 'Runtime {{.Os}}/{{.Architecture}} ({{.Size}} bytes)' typhon-local:dev
history="$(docker history --no-trunc typhon-local:dev)"
if [[ "$history" == *"$POSTGRES_PASSWORD"* ]]; then
  echo 'database password present in image history' >&2
  exit 1
fi

curl -fsS -X POST -H 'Content-Type: application/json' \
  --data '{"name":"created"}' "$url/records/compose" | grep -F 'created'
curl -fsS "$url/records/compose" | grep -F 'created'
curl -fsS -X PUT -H 'Content-Type: application/json' \
  --data '{"name":"updated"}' "$url/records/compose"
curl -fsS "$url/records/compose" | grep -F 'updated'

"${compose[@]}" stop --timeout 15 api
container="$("${compose[@]}" ps --all --quiet api)"
test "$(docker inspect -f '{{.State.ExitCode}}' "$container")" = 0
"${compose[@]}" up -d --no-deps --force-recreate db
"${compose[@]}" start api
wait_ready
curl -fsS "$url/records/compose" | grep -F 'updated'
curl -fsS -X DELETE "$url/records/compose"
test "$(curl -s -o /dev/null -w '%{http_code}' "$url/records/compose")" = 404
