# Idempotent HTTP writes and Redis locks

## HTTP requests

Call `routes.idempotency(method, pattern, redis_connection, namespace, ttl?)`
after registering an authenticated and authorized POST, PUT, PATCH or
DELETE route. Use a Redis connection created by each Engine at startup.
Use the same namespace across replicas of a service. The default TTL
is 24 hours, configurable from 60 seconds to seven days.

Clients send one `Idempotency-Key` header of 1–128 ASCII letters, digits
or `-._~`. Retry with the same key and exact request during the intended
retry window. Redis scopes the hashed key to the namespace, verified
tenant and subject. The fingerprint covers HTTP method, route template,
actual path, raw query, tenant, content type and exact buffered body
(maximum 256 KiB). Keys and fingerprints are kept out of runtime logs.

| State | Result |
| --- | --- |
| Key unused | Reserve before running the handler. |
| Same key, different fingerprint | 409 and `Idempotency-Status: conflict`. |
| Same key, operation pending or outcome unknown | 409 and `Idempotency-Status: in-progress`, with `Retry-After: 1`. |
| Same key, replayable result | Original status, body, `Content-Type` and `Location`, with `Idempotency-Replayed: true`. |
| Same key, finished without a replayable result | 409 and `Idempotency-Status: unreplayable`. |
| Redis unavailable or completion unconfirmed | 503. Retry with the same key after recovery. |

Replay covers buffered 2xx responses of up to 64 KiB with only the
supported response headers. For example, a response with `Set-Cookie`
is returned once to the caller and its contents are never saved for replay.
Any timeout, cancellation, thrown error or 5xx before confirmation can
leave the reservation pending through its TTL. Keep sensitive bodies
out of replayable routes. User middleware runs for both original and
replayed requests. Keep the critical effect in the route handler.

## Durable PostgreSQL guarantee

The [orders example](../examples/http_idempotency.ty) uses
`req.idempotency_fingerprint` inside the handler to persist the same
fingerprint in PostgreSQL. The composite primary key is
`(tenant_id, actor, request_key)`. The order and its idempotency record
are the same row. A retry after Redis expiry, eviction, restart or
failover can therefore return the persisted order. Reusing its key with
different content returns 409. Keep the record for the full business retry
window. For side effects outside the database, make the external action
idempotent or use a transactional outbox with durable deduplication.

Run the [migrations](../examples/idempotency/migrations) as the deployment
role in a database or schema reserved for this example:

```sh
typhon migrate up examples/idempotency/migrations
```

Switch `DATABASE_URL` to the application login described in the
[tenant isolation guide](postgres_rls.md). Grant `SELECT, INSERT` on
`idempotent_orders`. Set `REDIS_URL`, `AUTH_ISSUER`, `AUTH_AUDIENCE`
and `AUTH_JWKS_URL`, then run:

```sh
typhon run examples/http_idempotency.ty
```

Send a bearer access token with the `buyer` role to `POST /orders`.
Use a plain-text body of 1–80 characters and retain the same key and
content for retries.

## Lock leases

`conn.lock(key, ttl)` returns a `redis.Lock` or `None` if contested.
Keys have 1–256 bytes. The required lease lasts 0.001–60 seconds.
Acquisition is `SET NX PX` with an opaque random ownership token.
`release()` atomically checks that token before deleting and reports
whether it removed the owned key. Request or Engine cleanup attempts
the same safe release in its bounded cleanup window. The lease expires
if a network interruption prevents release.

```typhon
lock: redis.Lock | None = coordinator.lock('report-refresh', 2)
if lock is not None:
    try:
        print('refreshing report')
    finally:
        lock.release()
```

A lease can expire while the original owner is still executing. Redis
failover or network ambiguity can also allow competing work. Use this
lock for best-effort coordination. When the protected service must refuse
stale writers, it must enforce a monotonically increasing fencing value
on each write. Use PostgreSQL constraints and transactions for effects
that require a durable uniqueness guarantee.
