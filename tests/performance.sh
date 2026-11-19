#!/usr/bin/env bash
set -euo pipefail

mode="${1:-smoke}"
soak_minutes="${TYPHON_SOAK_MINUTES:-30}"
if [[ "$mode" != smoke && "$mode" != soak ]]; then exit 2; fi
if [[ ! "$soak_minutes" =~ ^[0-9]+$ ]] || (( soak_minutes < 1 || soak_minutes > 45 )); then exit 2; fi

export POSTGRES_PASSWORD="$(openssl rand -hex 24)"
export TYPHON_PORT="${TYPHON_PERF_PORT:-18082}"
export TYPHON_PERF_METRICS_PORT="${TYPHON_PERF_METRICS_PORT:-19090}"
project="typhon-perf-${GITHUB_RUN_ID:-$$}-$mode"
compose=(docker compose --project-name "$project" -f compose.yaml -f tests/compose_perf.yaml)
api="http://127.0.0.1:$TYPHON_PORT"
metrics="http://127.0.0.1:$TYPHON_PERF_METRICS_PORT/metrics"
work="$(mktemp -d)"
output="${TYPHON_PERF_OUTPUT:-$work}"
mkdir -p "$output"

cleanup() {
  local status=$?
  if [[ -n "${load_pid:-}" ]]; then
    kill "$load_pid" 2>/dev/null || true
    wait "$load_pid" 2>/dev/null || true
  fi
  if [[ "$status" != 0 ]]; then "${compose[@]}" logs --tail 50 >&2; fi
  "${compose[@]}" down --volumes --remove-orphans
  rm -rf -- "$work"
}
trap cleanup EXIT

wait_status() {
  local path="$1" want="$2"
  for _ in $(seq 1 70); do
    if [[ "$(curl -s --max-time 1 -o /dev/null -w '%{http_code}' "$api$path")" == "$want" ]]; then return; fi
    sleep 0.3
  done
  echo "$path did not reach $want" >&2
  return 1
}

db() {
  "${compose[@]}" exec -T db psql -X -U typhon -d typhon --set ON_ERROR_STOP=1 -At "$@"
}

sample() {
  local time proc series
  time="$(date -u +%FT%TZ)"
  proc="$("${compose[@]}" exec -T api sh -c 'awk "/^VmRSS:/ {print \$2}" /proc/1/status; awk "{print \$14+\$15}" /proc/1/stat')"
  series="$(curl -fsS --max-time 3 "$metrics" | awk '
    /^go_goroutines / { goroutines=$2 }
    /^go_memstats_heap_alloc_bytes / { heap=$2 }
    /^typhon_driver_pool_connections\{driver="postgres",state="total"\}/ { postgres=$2 }
    /^typhon_driver_pool_connections\{driver="redis",state="total"\}/ { redis=$2 }
    END { print goroutines, heap, postgres, redis }')"
  printf '%s %s %s\n' "$time" "$(paste -sd' ' <<< "$proc")" "$series"
}

{
  uname -srmo
  go version
  printf 'Available CPUs: %s\n' "$(nproc)"
  awk '/model name/ {print; exit}' /proc/cpuinfo
  docker version --format 'Docker Server {{.Server.Version}}'
  git rev-parse HEAD
} | tee "$output/environment.txt"

go build -trimpath -o "$work/load" ./tests/load
"${compose[@]}" up --build -d
wait_status /health/ready 200
curl -fsS -X POST -H 'Content-Type: application/json' \
  --data '{"name":"benchmark seed"}' "$api/records/seed" >/dev/null
echo 'utc rss_kib cpu_ticks goroutines heap_bytes postgres_conns redis_conns' | tee "$output/resources.txt"

if [[ "$mode" == soak ]]; then
  "$work/load" -url "$api" -workload mixed -concurrency 10 -duration "${soak_minutes}m" > "$output/soak.txt" &
  load_pid=$!
  sleep 10
  sample | tee -a "$output/resources.txt"
  # Sample every 30 seconds. Keep the same runner and container for the full soak.
  for ((seconds=30; seconds<soak_minutes*60; seconds+=30)); do
    sleep 30
    if ! kill -0 "$load_pid" 2>/dev/null; then break; fi
    sample | tee -a "$output/resources.txt"
  done
  wait "$load_pid"
  unset load_pid
  cat "$output/soak.txt"
  sample | tee -a "$output/resources.txt"
  awk '
    NR==2 { base_rss=$2; base_goroutines=$4 }
    NR>2 { if ($6>4 || $7>4) exit 1 }
    END { if (NR<3 || $2>base_rss+65536 || $4>base_goroutines+32) exit 1 }' "$output/resources.txt"
  exit
fi

for workers in 1 10 100 500; do
  flags=()
  if (( workers > 10 )); then flags+=(-allow-overload); fi
  "$work/load" -url "$api" -workload json -concurrency "$workers" -iterations 2000 "${flags[@]}"
  "$work/load" -url "$api" -workload mixed -concurrency "$workers" -iterations 2000 "${flags[@]}"
done | tee "$output/load.txt"
for workload in redis crud; do
  "$work/load" -url "$api" -workload "$workload" -concurrency 10 -iterations 50 | tee -a "$output/load.txt"
done
sample | tee -a "$output/resources.txt"

"${compose[@]}" stop redis
wait_status /health/ready 503
wait_status /health/live 204
wait_status /sample 200
wait_status /cache 503
"${compose[@]}" start redis
wait_status /health/ready 200

"${compose[@]}" stop db
wait_status /health/ready 503
wait_status /sample 200
wait_status /records/seed 503
"${compose[@]}" start db
wait_status /health/ready 200
wait_status /records/seed 200

db -c 'BEGIN; LOCK TABLE compose_records IN ACCESS EXCLUSIVE MODE; SELECT pg_sleep(5); ROLLBACK' >/dev/null &
locker=$!
for _ in $(seq 1 30); do
  if [[ "$(db -c "SELECT count(*) FROM pg_locks WHERE relation='compose_records'::regclass AND mode='AccessExclusiveLock' AND granted")" == 1 ]]; then break; fi
  sleep 0.05
done
test "$(db -c "SELECT count(*) FROM pg_locks WHERE relation='compose_records'::regclass AND mode='AccessExclusiveLock' AND granted")" = 1
curl_status=0
curl -s --max-time 0.15 "$api/records/seed" >/dev/null || curl_status=$?
test "$curl_status" = 28
for _ in $(seq 1 6); do
  blocked="$(db -c "SELECT count(*) FROM pg_stat_activity WHERE wait_event_type='Lock' AND query LIKE 'SELECT id, name FROM compose_records%'")"
  if [[ "$blocked" == 0 ]]; then break; fi
  sleep 0.02
done
test "$blocked" = 0
test "$(curl -s --max-time 3 -o /dev/null -w '%{http_code}' "$api/records/seed")" = 504
wait "$locker"
wait_status /records/seed 200

"${compose[@]}" stop --timeout 15 api
container="$("${compose[@]}" ps -a -q api)"
test "$(docker inspect -f '{{.State.ExitCode}}' "$container")" = 0
"${compose[@]}" start api
wait_status /health/ready 200
wait_status /records/seed 200
sample | tee -a "$output/resources.txt"
echo 'Redis and PostgreSQL outage/recovery, disconnect, timeout and container stop passed'

go test ./internal/typhon -run '^$' \
  -bench '^(BenchmarkTypedIntegerLoop|BenchmarkBytecodeCompileUserService|BenchmarkHTTPJSONReference)$' \
  -benchmem -benchtime=300ms -cpuprofile "$output/http-vm.pprof" -o "$work/bench.test" | tee "$output/go-benchmarks.txt"
go tool pprof -top -nodecount=12 "$work/bench.test" "$output/http-vm.pprof" | tee "$output/pprof-top.txt"
