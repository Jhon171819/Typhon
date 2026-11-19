# Performance and resilience

The reference Compose stack has two VM workers and a queue of 16.
PostgreSQL shares a four-connection application pool. Each worker's
Redis pool holds up to two connections. Keep these values, the Docker
memory limits and the runner configuration consistent when comparing runs.

## Reproduce

```sh
TYPHON_PERF_OUTPUT=performance-results bash tests/performance.sh smoke
TYPHON_PERF_OUTPUT=performance-soak TYPHON_SOAK_MINUTES=30 bash tests/performance.sh soak
```

The scripts use isolated Compose projects with disposable data. Ports
18082 and 19090 are loopback-only and overridable with `TYPHON_PERF_PORT`
and `TYPHON_PERF_METRICS_PORT`.

The smoke suite sends 2,000 fixed requests per JSON and mixed profile at
concurrency 1, 10, 100 and 500. The mixed profile rotates between the
same JSON, PostgreSQL and Redis reads. The CRUD profile creates, reads,
updates and deletes disjoint keys. The Redis profile exercises a native
command without a PostgreSQL dependency. The suite also stops and returns
each dependency independently, confirms that the in-memory route remains
responsive, disconnects a blocked SQL client, exercises the server query
deadline, and restarts the API after container SIGTERM.

The soak uses concurrent mixed reads for 30 minutes by default.
It records resident RSS, process CPU ticks, Go goroutines, heap allocation
and driver connections along with throughput and latency.
P50, p95 and p99 report the upper bound of their measured histogram bucket,
including overload responses. Throughput includes all completed requests.
The counters distinguish successful, overloaded and unexpected responses.

## Compare and diagnose

`BenchmarkHTTPJSONReference` compares a typed Typhon handler with a Go
`net/http` handler using the same JSON records, client, loopback HTTP
server and runner. The JSON serializers use different whitespace.
It compares this workload only. The integer-loop and bytecode-compile
benchmarks measure VM execution and compilation separately. The smoke
artifact includes allocations and a CPU profile that can be opened with
`go tool pprof`.

The smoke rejects unexpected responses at every concurrency. It permits
bounded-admission 503 responses only at concurrency 100 and 500.
The soak requires successful requests without overload, at most four
application connections per driver, final RSS at most 64 MiB above
the first loaded sample, and at most 32 additional goroutines. These
thresholds apply to this reference workload. Review them after material
changes to the stack or host.

The `Performance and resilience` workflow uploads the raw smoke artifacts.
Run a soak manually or apply the `performance-soak` label to a
same-repository PR. Record measured baselines by commit and runner.
