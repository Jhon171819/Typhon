# Host HTTP health probes

Configure the paths before `http.serve`. When a path is unset, the
application owns its routing. Probes support GET and HEAD and do not enter
the VM worker queue.

| Setting | Behavior |
| --- | --- |
| `TYPHON_LIVENESS_PATH=/health/live` | 204 while the listener can respond, including during drain and dependency failure. |
| `TYPHON_READINESS_PATH=/health/ready` | 200 with admission capacity and all configured probes healthy. 503 during startup checks, full admission, an essential dependency outage, stale checks or drain. |
| `TYPHON_HEALTH_DIAGNOSTICS_PATH=/health/diagnostics` | Optional JSON snapshot with readiness, drain, queue capacity and states of configured dependencies. Requires one bearer token via `TYPHON_HEALTH_DIAGNOSTICS_TOKEN`. |

Configure required dependencies with `TYPHON_HEALTH_POSTGRES_URL` and
`TYPHON_HEALTH_REDIS_URL`. The server pings each configured dependency at
most once per second with a 300 ms deadline and one dedicated connection
per dependency. Two consecutive successes or failures change status.
The cache becomes stale after three seconds. Public probe responses
have no diagnostics, and probe traffic does not trigger new pings.
Protect the optional diagnostics path with TLS and supply a random
32–128 character token. Responses contain fixed dependency names and
states, never connection strings or backend errors.

The [executable example](../examples/http_health.ty) uses lazy startup
for both application clients. Set `DATABASE_URL` and `REDIS_URL` first.
Then start it with:

```sh
TYPHON_LIVENESS_PATH=/health/live \
  TYPHON_READINESS_PATH=/health/ready \
  TYPHON_HEALTH_POSTGRES_URL="$DATABASE_URL" \
  TYPHON_HEALTH_REDIS_URL="$REDIS_URL" \
  typhon run examples/http_health.ty
```

Lazy PostgreSQL is opt-in with `{'lazy': True}` in
`pg.connect_pooled`. Existing connection forms still probe the database
at initialization. Use lazy startup when an orchestrator should keep the
process running while a dependency is unavailable.

## Orchestrators

For Compose, use an HTTP liveness check for the API container if its
health status controls restart or startup gating. Treat application
readiness as a separate traffic signal. Dependency `service_healthy`
conditions can help startup ordering as a convenience.

For Kubernetes, configure `startupProbe` against `/health/live` with
enough budget for initialization. Use `livenessProbe` on that path
and `readinessProbe` on `/health/ready`. Set the termination window
to cover [HTTP drain and cleanup](http_shutdown.md). A database outage
then removes the pod from service endpoints while it remains live.
