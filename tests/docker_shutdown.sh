#!/usr/bin/env bash
set -euo pipefail

# Use the PostgreSQL service created for this disposable CI job.
export DATABASE_URL="${TYPHON_TEST_POSTGRES_URL:?dedicated CI database required}"
work="$(mktemp -d)"
name="typhon-drain-${GITHUB_RUN_ID:-$$}"

db() {
  docker run --rm --network host -i --entrypoint psql postgres:16 \
    "$DATABASE_URL" -X --no-password --set ON_ERROR_STOP=1 "$@"
}

cleanup() {
  if docker container inspect "$name" >/dev/null 2>&1; then
    docker rm -f "$name" >/dev/null
  fi
  if [[ -n "${request:-}" ]]; then wait "$request" 2>/dev/null || true; fi
  db -q -c 'DROP TABLE IF EXISTS typhon_docker_stop_fixture' >/dev/null
  rm -rf -- "$work"
}
trap cleanup EXIT

db -q -c 'CREATE TABLE typhon_docker_stop_fixture (id int not null)'
CGO_ENABLED=0 go build -trimpath -o "$work/typhon" ./cmd/typhon
docker run -d --name "$name" --network host -e DATABASE_URL \
  -e TYPHON_LISTEN_ADDRESS=127.0.0.1:18080 \
  -e TYPHON_READINESS_PATH=/health/ready -e TYPHON_HTTP_SHUTDOWN_GRACE=300ms \
  -v "$work/typhon:/usr/local/bin/typhon:ro" \
  -v "$PWD/examples/http_shutdown.ty:/service.ty:ro" \
  --entrypoint /usr/local/bin/typhon postgres:16 run /service.ty >/dev/null

for _ in $(seq 1 100); do
  if curl -sf --max-time 1 http://127.0.0.1:18080/health/ready >/dev/null; then break; fi
  sleep 0.05
done
curl -fsS --max-time 1 http://127.0.0.1:18080/health/ready >/dev/null

curl -sS --max-time 6 -X POST -w '%{http_code}' -o /dev/null \
  http://127.0.0.1:18080/work > "$work/status" &
request=$!
db -q <<'SQL'
DO $$
BEGIN
  FOR attempt IN 1..100 LOOP
    PERFORM pg_stat_clear_snapshot();
    IF EXISTS (SELECT 1 FROM pg_stat_activity
      WHERE pid <> pg_backend_pid() AND state = 'active'
        AND query LIKE '%/* typhon_container_drain */%') THEN
      RETURN;
    END IF;
    PERFORM pg_sleep(0.025);
  END LOOP;
  RAISE EXCEPTION 'transaction did not enter fixture query';
END
$$;
SQL

docker stop --time 4 "$name" >/dev/null
wait "$request"
unset request
test "$(cat "$work/status")" = '503'
test "$(docker inspect -f '{{.State.ExitCode}}' "$name")" = '0'
test "$(db -At -c 'SELECT count(*) FROM typhon_docker_stop_fixture')" = '0'
docker logs "$name"
