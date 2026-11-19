# Tenant isolation with PostgreSQL row security

[The records API](../examples/http_rls.ty) uses a shared table with a
`(tenant_id, id)` primary key. Its [migrations](../examples/rls/migrations)
enable and force row-level security, with one policy per CRUD action.
Reads, updates and deletes select rows visible to the bound tenant. Inserts
and updates also check the tenant of the resulting row.

## Deploy the example

Run the schema migrations as a deployment role that owns the database
objects. Point `DATABASE_URL` at that role while running the CLI.

```sh
typhon migrate up examples/rls/migrations
```

Provision a separate application login outside the service. Use
`NOSUPERUSER NOBYPASSRLS NOINHERIT` and keep it out of the owner and
privileged roles. Grant `CONNECT` on the target database, `USAGE` on the
application schema, and `SELECT, INSERT, UPDATE, DELETE` on `tenant_records`.
Keep schema changes, role administration, `TRUNCATE` and `REFERENCES`
privileges with deployment or database operators. PostgreSQL RLS applies to
row operations. Superusers and roles with `BYPASSRLS` always bypass it, and
owners can change policies even with `FORCE ROW LEVEL SECURITY`. See
[PostgreSQL row security](https://www.postgresql.org/docs/16/ddl-rowsecurity.html).

Set `DATABASE_URL` to the application login and set `REDIS_URL`,
`AUTH_ISSUER`, `AUTH_AUDIENCE` and `AUTH_JWKS_URL` for the API. Start the
example with:

```sh
typhon run examples/http_rls.ty
```

Call `GET /records/{id}` with a bearer access token from the configured
issuer. The example expects a signed tenant and the `member` role, plus
the authorization policy's normal scope requirements.

## Transaction and cache behavior

`db.tenant_transaction(req, callback, isolation?, read_only?, max_retries?)`
accepts a callback of type `(pg.Transaction, http.Request) -> Any`. The
request must belong to the current VM request and have passed its router
authentication and policy checks. The helper rejects a database login or
SQL identity with privileged role membership. It pins the connection,
sets `typhon.tenant_id` and `row_security` with transaction-local
`set_config`, then calls the callback. Commit or rollback clears the
setting before pool reuse. Every opted-in retry repeats the binding.

```python
def read_record(tx: pg.Transaction, req: http.Request) -> Any:
    rows: pg.Rows = tx.query('SELECT label FROM tenant_records WHERE id = $1', [req.param('id')])
    if rows.count == 0:
        return None
    return {'label': rows.get(0, 'label')}

def read(req: http.Request) -> http.Response:
    value: Any = db.tenant_transaction(req, read_record, 'read_committed', True)
    if value is None:
        return web.problem(req, 404)
    return http.response(200, json.dumps(value), [['Content-Type', 'application/json']])
```

Call `tx.commit()` explicitly when persisting a mutation. Keep external
calls and effects with no safe retry outside the callback. Use trusted,
parameterized SQL. Application code with arbitrary SQL access can change a
custom PostgreSQL setting, so keep the database credentials and SQL
execution away from user-controlled code.

The example serializes the verified tenant and record ID together for its
Redis key. Cache reads occur after route authorization. Successful writes
invalidate the corresponding key after commit. Its ten-second TTL bounds
the lifetime of a stale entry if a concurrent read races an invalidation.

## Operator audit

Use a restricted, separately authenticated operator path for exceptional
cross-tenant maintenance. The deployment migrations provision
`tenant_admin_audit`. With a role granted insert access to this table, record
the approved change reference and operation alongside the maintenance SQL in
the same transaction. PostgreSQL fills the actor with `current_user` and the
timestamp with `clock_timestamp()`. Keep grants away from the service login,
use individual operator identities, and avoid customer data, tokens and SQL
parameters in audit text. The runtime also emits request ID, authorized
action and outcome for tenant transaction calls.
