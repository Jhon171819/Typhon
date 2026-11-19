# Configured native HTTP clients

`http.client(timeout?)` continues to accept the positional timeout
and uses the traditional Go redirect policy. Use
`http.client_with_options(options)` for an explicit policy. Create
one client per Engine at startup and reuse it across calls. Its
`close()` method is idempotent and closes idle connections.

| Option | Default | Behavior |
| --- | --- | --- |
| `connect_timeout` | 5 seconds | Bounds connection and TLS handshake stages. |
| `read_timeout` | 5 seconds | Bounds response headers and buffered response body separately. |
| `total_timeout` | 10 seconds | Covers all attempts, redirects and backoff. The calling request deadline can shorten it. |
| `max_response_bytes` | 8 MiB | Caps the buffered, decoded HTTP body. The VM byte budget can lower the cap. |
| `max_connections` | 16 | 1–64 connections per host, with the same bound on total idle connections. |
| `allow_http` | `False` | Allows plain HTTP for a known development or internal target. |
| `tls_root_pem` | system roots | Adds PEM roots while preserving verification of certificate and hostname. TLS 1.2 or later is required. |
| `max_redirects` | 0 | 0–4 redirects. Only GET and HEAD may follow, and only to the original scheme and host. A rejected cross-origin redirect remains a 3xx response. A loop beyond the limit raises `ConnectionError`. |
| `retries` | 0 | 0–4 additional attempts within the total deadline. |

Timeouts take seconds as a number between 0.001 and 300. Request body
size remains bounded by 8 MiB and by its VM budget. `request(method, url,
body, headers?)` sends a string. `request_json(method, url, value,
headers?)` serializes with the native JSON codec and supplies
`Content-Type: application/json` when absent. `request_bytes(method, url,
data, headers?)` takes `typhon.scalars.Bytes` and defaults to
`application/octet-stream`. Repeated header pairs are preserved.

`response.header_values(name)` returns all values. `response.json()`
requires `application/json` or an `application/*+json` media type and
uses the native JSON validation and duplicate-key rules. Use
`response.bytes()` for a binary body. Status responses such as 404 and
503 remain ordinary responses. Transport failures raise `ConnectionError`
or `TimeoutError`; invalid options, payload formats and configured
response-size violations raise `ValidationError`.

## Retries

Explicit retries apply to transient connection failure, EOF, timeout and
responses 408, 429, 502, 503 or 504. TLS certificate failures abort
immediately. Eligible methods are GET, HEAD, PUT, DELETE, OPTIONS and
TRACE. POST and PATCH require one valid `Idempotency-Key` and the
downstream must honor it. Retries preserve the exact body and headers.
Backoff grows with jitter, starting at 20 ms. A valid `Retry-After`
can make the wait longer. If it will not fit within the total budget,
the client returns the last response or transport error.

The Go transport can recover a stale pooled connection internally.
The configured client controls replay of request bodies. A POST
without an idempotency key runs once even if a response timeout
leaves the result ambiguous. Use a stable business key and the
[server idempotency contract](idempotency.md) for retried writes.

## Run the example

Set `UPSTREAM_URL` to a known HTTPS endpoint that returns JSON.

```sh
typhon run examples/http_client_json.ty
```

Constrain any application that selects destinations from user input.
Map input to service identifiers on an allowlist, keep redirects disabled,
and restrict egress using network policy. Validate resolved addresses at
connection time if arbitrary DNS names are permitted. Exclude private,
loopback, link-local and metadata ranges unless an approved service
requires them. DNS validation performed only before dialing can be
invalidated by a later lookup. Disable environment proxies where proxy
policy cannot enforce those same limits. A reusable HTTP client by itself
provides no complete SSRF boundary.
