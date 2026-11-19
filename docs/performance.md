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
It compares this workload only. The warmed integer loop compiles the
module before the timer and measures calls to its bytecode. The cold
integer loop includes initialization and compilation. The separate
bytecode benchmark measures loading, checking and compiling the user
service. The smoke artifact includes allocation counts and a CPU
profile for `go tool pprof`.

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

## Initial measured baseline

Commit `127c2f9` ran on an Ubuntu amd64 GitHub-hosted runner with four
available CPUs, AMD EPYC 9V74 and Go 1.26.0. Each row used 2,000
loopback requests against the same Compose stack. P95 is the upper
histogram bound, including 503 responses.

| Concurrency | JSON req/s | JSON 2xx / 503 | JSON p95 | Mixed req/s | Mixed 2xx / 503 | Mixed p95 |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 1 | 3,481 | 2,000 / 0 | ≤1 ms | 2,629 | 2,000 / 0 | ≤1 ms |
| 10 | 7,886 | 2,000 / 0 | ≤5 ms | 5,417 | 2,000 / 0 | ≤5 ms |
| 100 | 17,697 | 413 / 1,587 | ≤20 ms | 19,598 | 217 / 1,783 | ≤20 ms |
| 500 | 14,596 | 244 / 1,756 | ≤100 ms | 14,960 | 189 / 1,811 | ≤100 ms |

All rows had zero unexpected responses. CRUD at concurrency 10
completed 200 requests at 2,255 req/s and p95 ≤10 ms. Redis at
concurrency 10 completed 50 requests at 4,551 req/s and p95 ≤5 ms.
The highest request rates include deliberate rejection and describe
admission behavior. The logs have the p50, p99 and raw counts.

The loopback GET benchmark measured about 175 µs and 113,112 B
per Typhon request, versus 60 µs and 5,934 B in the Go handler.
The cold integer loop took 82 µs and the separate compilation
benchmark took 327 µs on their respective fixtures. The combined CPU
profile attributed 18% of sampled CPU to Linux syscalls and 10% to
futexes. This profile mixes compilation, VM and HTTP workloads.
A request-only profile would be needed to attribute the allocation
difference to particular functions.
[Initial smoke logs and artifacts](https://github.com/Jhon171819/Typhon/actions/runs/44633277572)
