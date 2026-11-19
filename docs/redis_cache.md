# Redis tenant cache

Create a cache with `redis_connection.cache(domain, version, ttl?)`. The
domain has 1–64 UTF-8 bytes, the version is a positive 31-bit integer and
the TTL defaults to 60 seconds. A supplied TTL can range from 0.001 to
86,400 seconds, in milliseconds. Increment the version when changing a
JSON schema incompatibly. Existing keys expire under their former TTL.

`cache.lookup(req, key)` derives the tenant from the current authenticated
and authorized HTTP request. It accepts a logical key of 1–256 UTF-8
bytes. Key components use separate URL-safe Base64 encodings, keeping
delimiters in tenant, domain and logical IDs unambiguous. Control
characters in those names are rejected. Keep credentials and sensitive
fields out of Redis keys and values.

The request-scoped `redis.CacheEntry` has `hit: bool` and `value: Any`.
A miss returns `hit == False` and `value is None`. Cached JSON `null`
returns `hit == True` and `value is None`. Values use the native JSON
codec and the serialized payload is limited to 128 KiB.

## Cache-aside with PostgreSQL

The full [records API example](../examples/http_rls.ty) uses PostgreSQL
as the source of truth. This code runs after the router has authorized
the request.

```typhon
def get_label(req: http.Request) -> http.Response:
    key: str = req.param('id')
    entry: redis.CacheEntry = cache.lookup(req, key)
    if entry.hit:
        return http.json(200, entry.value)
    value: Any = db.tenant_transaction(req, read_record, 'read_committed', True)
    if value is None:
        return web.problem(req, 404)
    entry.store(req, value)
    return http.json(200, value)
```

Call `cache.invalidate(req, key)` after PostgreSQL confirms a mutation.
A rolled-back mutation leaves the old cache value valid. Redis atomically
replaces a miss token with the first accepted fill. After invalidation
or token expiry, a late fill returns `False`. Concurrent misses can still
read PostgreSQL independently. Keep the invalidation close to commit.

The example uses lazy Redis connection and falls back to PostgreSQL on
read-side cache errors. It propagates invalidation failures after commit.
The database write remains committed in that case. A Redis outage between
commit and invalidation can leave an old cached value until TTL expiry.
Use a durable outbox when reliable cross-service invalidation is required.

`redis_connection.stats()` provides cumulative `cache_hits`,
`cache_misses`, `cache_errors` and `cache_invalidations` counters.
They contain no tenant, key or operation labels. An invalidation counts
when Redis acknowledges the command, even when the target has expired.
