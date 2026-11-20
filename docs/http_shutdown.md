# Graceful HTTP shutdown

The CLI handles SIGTERM and SIGINT via Go's signal context. HTTP serving
uses a separate worker context so it can finish admitted requests after
the signal. The listener remains available during this window.

1. The server marks readiness false, rejects new application requests with
   503, and disconnects SSE streams so clients can reconnect elsewhere.
2. Previously admitted requests, including queued work, can use the
   remaining grace period. Their own client and request deadlines still
   apply.
3. Once admitted requests finish, or when grace expires, the server
   cancels remaining work and closes the listener. The request and Engine
   cleanup hooks release resources and roll back uncommitted transactions.

Set `TYPHON_HTTP_SHUTDOWN_GRACE` to a Go duration such as `500ms` or
`10s`. The default is `5s`. Valid values range from `0s` to `1m`.
`0s` immediately cancels work. With this sequence the CLI normally
exits with code 0. The signal watchdog allows eight extra seconds for
closing and cleanup, then exits with code 1 if shutdown is still stuck.
Set the container or orchestrator termination timeout above grace plus
eight seconds if the CLI should enforce that hard deadline.

Enable the host readiness probe with
`TYPHON_READINESS_PATH=/health/ready`. It returns 200 after startup and
503 during drain, without entering the worker queue. [Optional health checks](http_health.md)
also consider admission capacity and configured dependencies. An independent
`TYPHON_LIVENESS_PATH=/health/live` continues to return 204 while the
listener responds. Leave either path unset to keep application routes
in control of those URLs.

Example configuration:

```sh
TYPHON_HTTP_SHUTDOWN_GRACE=3s \
  TYPHON_READINESS_PATH=/health/ready \
  TYPHON_LIVENESS_PATH=/health/live \
  typhon run examples/http_router.ty
```

The CLI integration test sends a real SIGTERM on Linux. It verifies
that short GET and transactional POST requests finish during grace, new
work receives 503, and an unfinished POST rolls back. The
[container test](../tests/docker_shutdown.sh) uses a disposable
PostgreSQL fixture to exercise `docker stop --time` against the
[shutdown test service](../examples/http_shutdown.ty).
