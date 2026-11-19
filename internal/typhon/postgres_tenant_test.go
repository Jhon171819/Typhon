package typhon

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func tenantFixture(t *testing.T, databaseURL string, identity rateFixtureAuth) (*postgresConnection, http.Handler) {
	t.Helper()
	t.Setenv("DATABASE_URL", databaseURL)
	engine := NewEngine()
	t.Cleanup(func() {
		if err := engine.Close(); err != nil {
			t.Error(err)
		}
	})
	example, err := os.ReadFile(filepath.Join("..", "..", "examples", "http_rls.ty"))
	if err != nil {
		t.Fatal(err)
	}
	module, err := engine.LoadModule(sourceFile(t, string(example)+`
attempts: list[int] = [0]

def retry_record(tx: pg.Transaction, req: http.Request) -> Any:
    attempts[0] = attempts[0] + 1
    tx.exec("INSERT INTO tenant_records (tenant_id, id, label) VALUES (pg_catalog.current_setting('typhon.tenant_id'), 'retried', 'after retry')", [])
    if attempts[0] == 1:
        tx.exec("DO $$ BEGIN RAISE SQLSTATE '40001'; END $$", [])
    tenant: Any = tx.query("SELECT pg_catalog.current_setting('typhon.tenant_id') AS tenant", []).get(0, 'tenant')
    tx.commit()
    return [tenant, attempts[0]]

def retry(req: http.Request) -> http.Response:
    value: Any = db.tenant_transaction(req, retry_record, 'serializable', False, 1)
    return http.response(200, json.dumps(value))

routes.add('POST', '/retry', retry)
routes.require_auth('POST', '/retry', token)
routes.authorize('POST', '/retry', policy, 'records:write')
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ExecuteModule(module, false); err != nil {
		t.Fatal(err)
	}
	routes := module.Globals["routes"]
	router, _ := routerValue(routes)
	router.frozen = true
	for _, route := range router.routes {
		route.auth = identity
	}
	return module.Globals["db"].NativeObject.data.(*postgresConnection), httpVMHandler(engine, routes)
}

func tenantTestRole(t *testing.T, ctx context.Context, admin *pgx.Conn, schema, dsn string) string {
	t.Helper()
	role := "typhon_api_" + strings.ToLower(rand.Text())
	identifier := pgx.Identifier{role}.Sanitize()
	password := rand.Text()
	// The password and identifier are generated for the fixture, from known alphabets.
	if _, err := admin.Exec(ctx, "CREATE ROLE "+identifier+" LOGIN NOINHERIT NOSUPERUSER NOBYPASSRLS PASSWORD '"+password+"'"); err != nil {
		t.Fatal(driverFailure("PostgreSQL", databaseKind, err))
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		for _, sql := range []string{"DROP OWNED BY " + identifier, "DROP ROLE " + identifier} {
			if _, err := admin.Exec(cleanup, sql); err != nil {
				t.Error(driverFailure("PostgreSQL", databaseKind, err))
			}
		}
	})
	for _, sql := range []string{
		"GRANT USAGE ON SCHEMA " + schema + " TO " + identifier,
		"GRANT SELECT, INSERT, UPDATE, DELETE ON " + schema + ".tenant_records TO " + identifier,
	} {
		if _, err := admin.Exec(ctx, sql); err != nil {
			t.Fatal(driverFailure("PostgreSQL", databaseKind, err))
		}
	}
	target, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(driverFailure("PostgreSQL", validationKind, err))
	}
	target.User = url.UserPassword(role, password)
	return target.String()
}

func requireTenantCheck(t *testing.T, ctx context.Context, db *postgresConnection, tenantA, tenantB string) {
	t.Helper()
	for _, sql := range []string{
		"INSERT INTO tenant_records (tenant_id, id, label) VALUES ($1, 'cross-tenant', 'invalid')",
		"UPDATE tenant_records SET tenant_id = $1 WHERE id = 'shared'",
	} {
		tx, err := db.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, "SELECT pg_catalog.set_config('typhon.tenant_id',$1,true)", tenantA); err != nil {
			t.Fatal(errors.Join(err, tx.Rollback(ctx)))
		}
		_, err = tx.Exec(ctx, sql, tenantB)
		rollbackErr := tx.Rollback(ctx)
		var violation *pgconn.PgError
		if !errors.As(err, &violation) || violation.Code != "42501" || rollbackErr != nil {
			t.Fatalf("cross-tenant WITH CHECK expected 42501, got %v, rollback: %v", err, rollbackErr)
		}
	}
}

func TestNativePostgresTenantIsolationIntegration(t *testing.T) {
	ctx, adminDSN, admin, schema := nativeMigrationDB(t)
	if err := RunMigrations(ctx, adminDSN, filepath.Join("..", "..", "examples", "rls", "migrations"), "up", io.Discard); err != nil {
		t.Fatal(err)
	}
	dsn := tenantTestRole(t, ctx, admin, schema, adminDSN)
	tenantA, tenantB := rand.Text(), rand.Text()
	if _, err := admin.Exec(ctx, "INSERT INTO "+schema+".tenant_records (tenant_id,id,label) VALUES ($1,'shared','alpha'), ($2,'shared','beta'), ($2,'private','beta-private')", tenantA, tenantB); err != nil {
		t.Fatal(err)
	}
	redisURL := os.Getenv("TYPHON_TEST_REDIS_URL")
	if redisURL == "" {
		redisURL = "redis://" + miniredis.RunT(t).Addr() + "/0"
	}
	t.Setenv("REDIS_URL", redisURL)
	t.Setenv("AUTH_ISSUER", "https://issuer.example")
	t.Setenv("AUTH_AUDIENCE", "test")
	t.Setenv("AUTH_JWKS_URL", "https://issuer.example/jwks")
	identity := rateFixtureAuth{
		"a":         {subject: "alice", tenant: tenantA, roles: []string{"member"}},
		"b":         {subject: "bob", tenant: tenantB, roles: []string{"member"}},
		"no-policy": {subject: "charlie", tenant: tenantA},
	}
	db, handler := tenantFixture(t, dsn, identity)
	send := func(handler http.Handler, method, path, token, body string, status int, want string) {
		t.Helper()
		req := httptest.NewRequestWithContext(ctx, method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Tenant", tenantB)
		got := httptest.NewRecorder()
		handler.ServeHTTP(got, req)
		if got.Code != status || (want != "" && !strings.Contains(got.Body.String(), want)) {
			t.Fatalf("%s %s (%s): got %d %s, expected %d containing %q", method, path, token, got.Code, got.Body.String(), status, want)
		}
	}
	checkScope := func(pid int32) int32 {
		t.Helper()
		var actual int32
		var sessionTenant *string
		err := db.pool.QueryRow(ctx, "SELECT pg_backend_pid(), NULLIF(pg_catalog.current_setting('typhon.tenant_id', true), '')").Scan(&actual, &sessionTenant)
		if err != nil || sessionTenant != nil || (pid != 0 && pid != actual) {
			t.Fatalf("expected the same connection without a tenant, previous pid=%d current pid=%d tenant=%v err=%v", pid, actual, sessionTenant, err)
		}
		return actual
	}
	pid := checkScope(0)
	before := db.pool.Stat().AcquireCount()
	send(handler, "GET", "/records/shared", "invalid", "", 401, "")
	send(handler, "GET", "/records/shared", "no-policy", "", 403, "")
	if got := db.pool.Stat().AcquireCount(); got != before {
		t.Fatalf("denied request acquired a SQL connection: %d -> %d", before, got)
	}

	send(handler, "GET", "/records/shared", "a", "", 200, "alpha")
	send(handler, "GET", "/records/shared", "b", "", 200, "beta")
	before = db.pool.Stat().AcquireCount()
	send(handler, "GET", "/records/shared", "a", "", 200, "alpha")
	send(handler, "GET", "/records/shared", "b", "", 200, "beta")
	if got := db.pool.Stat().AcquireCount(); got != before {
		t.Fatalf("tenant cache hits acquired a SQL connection: %d -> %d", before, got)
	}
	checkScope(pid)
	requireTenantCheck(t, ctx, db, tenantA, tenantB)

	send(handler, "GET", "/records/private", "a", "", 404, "")
	send(handler, "PUT", "/records/private", "a", "tamper", 404, "")
	send(handler, "DELETE", "/records/private", "a", "", 404, "")
	send(handler, "GET", "/records/private", "b", "", 200, "beta-private")
	send(handler, "POST", "/records/created", "a", "alpha-only", 201, "")
	send(handler, "GET", "/records/created", "b", "", 404, "")
	send(handler, "GET", "/records/created", "a", "", 200, "alpha-only")
	send(handler, "PUT", "/records/shared", "a", "alpha-updated", 204, "")
	send(handler, "GET", "/records/shared", "a", "", 200, "alpha-updated")
	send(handler, "GET", "/records/shared", "b", "", 200, "beta")
	send(handler, "DELETE", "/records/shared", "a", "", 204, "")
	send(handler, "GET", "/records/shared", "a", "", 404, "")
	send(handler, "GET", "/records/shared", "b", "", 200, "beta")
	send(handler, "POST", "/retry", "a", "", 200, `["`+tenantA+`",2]`)
	send(handler, "GET", "/records/retried", "b", "", 404, "")
	send(handler, "GET", "/records/retried", "a", "", 200, "after retry")
	checkScope(pid)

	_, privileged := tenantFixture(t, adminDSN, identity)
	send(privileged, "GET", "/records/privileged-only", "a", "", 403, "")
}
