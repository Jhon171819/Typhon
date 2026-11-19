package typhon

import (
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

func lockTestClient(t *testing.T, dsn string) (*VM, Value) {
	t.Helper()
	engine := NewEngine()
	t.Cleanup(func() {
		if err := engine.Close(); err != nil {
			t.Error(err)
		}
	})
	vm := &VM{engine: engine}
	value, err := nativeRedisConnect(vm, []Value{StringValue(dsn)})
	if err != nil {
		t.Fatal(err)
	}
	return vm, value.NativeObject.Attrs["lock"]
}

func checkRedisLockOwnership(t *testing.T, dsn string, wait func()) {
	t.Helper()
	first, acquireFirst := lockTestClient(t, dsn)
	second, acquireSecond := lockTestClient(t, dsn)
	key := StringValue(rand.Text())
	acquire := func(vm *VM, method Value, ttl float64) Value {
		t.Helper()
		lock, err := vm.callValue(method, []Value{key, FloatValue(ttl)}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return lock
	}
	release := func(vm *VM, lock Value, expected bool) {
		t.Helper()
		deleted, err := vm.callValue(lock.NativeObject.Attrs["release"], nil, nil)
		if err != nil || deleted.Bool != expected {
			t.Fatalf("release: %v, error: %v, expected %v", deleted.Bool, err, expected)
		}
	}
	old := acquire(first, acquireFirst, 0.15)
	if old.Kind == ValueNone {
		t.Fatal("initial owner could not acquire an empty lock")
	}
	if got := acquire(second, acquireSecond, 1); got.Kind != ValueNone {
		t.Fatal("second owner acquired a live lock")
	}
	wait()
	current := acquire(second, acquireSecond, 1)
	if current.Kind == ValueNone {
		t.Fatal("second owner could not acquire expired lock")
	}
	release(first, old, false)
	if got := acquire(first, acquireFirst, 1); got.Kind != ValueNone {
		t.Fatal("old owner deleted the successor's lock")
	}
	release(second, current, true)
	release(second, current, false)
	if got := acquire(first, acquireFirst, 1); got.Kind == ValueNone {
		t.Fatal("released lock was not reusable")
	}
	if err := first.engine.Close(); err != nil {
		t.Fatal(err)
	}
	if got := acquire(second, acquireSecond, 1); got.Kind == ValueNone {
		t.Fatal("Engine shutdown did not release owned lock")
	}
}

func TestNativeRedisLockOwnership(t *testing.T) {
	server := miniredis.RunT(t)
	checkRedisLockOwnership(t, "redis://"+server.Addr(), func() { server.FastForward(time.Second) })
}

func TestNativeRedisLockOwnershipIntegration(t *testing.T) {
	dsn := os.Getenv("TYPHON_TEST_REDIS_URL")
	if dsn == "" {
		t.Skip("set TYPHON_TEST_REDIS_URL to exercise locks on Redis")
	}
	checkRedisLockOwnership(t, dsn, func() { time.Sleep(200 * time.Millisecond) })
}

func idempotencyTestHandler(t *testing.T, path string, verifier rateFixtureAuth) (*LoadedModule, http.Handler) {
	t.Helper()
	engine := NewEngine()
	t.Cleanup(func() {
		if err := engine.Close(); err != nil {
			t.Error(err)
		}
	})
	module, err := engine.LoadModule(path)
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
		route.auth = verifier
	}
	return module, httpVMHandler(engine, routes)
}

func sendIdempotent(t *testing.T, handler http.Handler, path, token, key, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "text/plain")
	for _, entry := range strings.Split(key, ",") {
		if entry != "" {
			req.Header.Add("Idempotency-Key", entry)
		}
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	return response
}

func TestNativeHTTPIdempotencyMiddleware(t *testing.T) {
	server := miniredis.RunT(t)
	path := sourceFile(t, fmt.Sprintf(`
import typhon.auth as auth
import typhon.http as http
import typhon.redis as redis
import typhon.web as web
coordinator: redis.Connection = redis.connect(%q)
token: auth.Authenticator = auth.jwt('https://issuer.example', 'test', 'https://issuer.example/jwks')
policy: auth.Policy = auth.policy({'buyer': ['write']})
routes: web.Router = web.router()
routes.use_errors()
calls: list[int] = [0]

def write(req: http.Request) -> http.Response:
    calls[0] = calls[0] + 1
    if req.body == 'ambiguous':
        raise TimeoutError('outcome unconfirmed')
    if req.body == 'session':
        return http.response(201, 'ok', [['Set-Cookie', 'sid=demo']])
    return http.response(201, req.body, [['Location', '/status']])

routes.add('POST', '/write', write)
routes.require_auth('POST', '/write', token)
routes.authorize('POST', '/write', policy, 'write')
routes.idempotency('POST', '/write', coordinator, 'test', 60)
`, "redis://"+server.Addr()))
	module, handler := idempotencyTestHandler(t, path, rateFixtureAuth{
		"a": {subject: "shared", tenant: "a", roles: []string{"buyer"}},
		"b": {subject: "shared", tenant: "b", roles: []string{"buyer"}},
	})
	assert := func(path, token, key, body string, status int, header, expected string) *httptest.ResponseRecorder {
		t.Helper()
		got := sendIdempotent(t, handler, path, token, key, body)
		if got.Code != status || got.Header().Get(header) != expected {
			t.Fatalf("%s %s got %d %q, expected %d %q: %s", path, key, got.Code, got.Header().Get(header), status, expected, got.Body.String())
		}
		return got
	}
	assert("/write", "invalid", "order-1", "item", 401, "", "")
	assert("/write", "a", "", "item", 400, "", "")
	assert("/write", "a", "one,two", "item", 400, "", "")
	assert("/write", "a", "order-1", strings.Repeat("x", maxIdempotencyRequest+1), 413, "", "")
	assert("/write?mode=1", "a", "order-1", "item", 201, "Location", "/status")
	replay := assert("/write?mode=1", "a", "order-1", "item", 201, "Idempotency-Replayed", "true")
	if replay.Body.String() != "item" || replay.Header().Get("Location") != "/status" {
		t.Fatal("replay did not preserve status, body and location")
	}
	assert("/write?mode=2", "a", "order-1", "item", 409, "Idempotency-Status", "conflict")
	assert("/write?mode=1", "a", "order-1", "different", 409, "Idempotency-Status", "conflict")
	assert("/write?mode=2", "b", "order-1", "different", 201, "Idempotency-Replayed", "")
	assert("/write", "a", "order-2", "ambiguous", 504, "", "")
	assert("/write", "a", "order-2", "ambiguous", 409, "Idempotency-Status", "in-progress")
	assert("/write", "a", "order-3", "session", 201, "Set-Cookie", "sid=demo")
	assert("/write", "a", "order-3", "session", 409, "Idempotency-Status", "unreplayable")
	server.Close()
	assert("/write", "a", "order-4", "item", 503, "", "")
	if got := (*module.Globals["calls"].List)[0].Int; got != 4 {
		t.Fatalf("expected only 4 handler executions, got %d", got)
	}
}

func TestNativePostgresHTTPIdempotencyIntegration(t *testing.T) {
	redisURL := os.Getenv("TYPHON_TEST_REDIS_URL")
	if redisURL == "" {
		t.Skip("set TYPHON_TEST_REDIS_URL to exercise idempotency across Redis and PostgreSQL")
	}
	ctx, adminDSN, admin, schema := nativeMigrationDB(t)
	if err := RunMigrations(ctx, adminDSN, filepath.Join("..", "..", "examples", "idempotency", "migrations"), "up", io.Discard); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DATABASE_URL", tenantTestRole(t, ctx, admin, schema, adminDSN, "idempotent_orders"))
	t.Setenv("REDIS_URL", redisURL)
	t.Setenv("AUTH_ISSUER", "https://issuer.example")
	t.Setenv("AUTH_AUDIENCE", "test")
	t.Setenv("AUTH_JWKS_URL", "https://issuer.example/jwks")
	tenant := rand.Text()
	identity := rateFixtureAuth{"a": {subject: "alice", tenant: tenant, roles: []string{"buyer"}}}
	server := make([]http.Handler, 8)
	var cache *redisConnection
	for i := range server {
		module, handler := idempotencyTestHandler(t, filepath.Join("..", "..", "examples", "http_idempotency.ty"), identity)
		server[i] = handler
		cache = module.Globals["coordinator"].NativeObject.data.(*redisConnection)
	}

	start := make(chan struct{})
	results := make(chan *httptest.ResponseRecorder, 100)
	var callers sync.WaitGroup
	defer callers.Wait()
	for i := range 100 {
		callers.Go(func() {
			<-start
			results <- sendIdempotent(t, server[i%len(server)], "/orders", "a", "same", "widget")
		})
	}
	close(start)
	executed, conflicts := 0, 0
	var original string
	for range 100 {
		var response *httptest.ResponseRecorder
		select {
		case response = <-results:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		switch response.Code {
		case 201:
			if response.Header().Get("Idempotency-Replayed") == "" {
				executed++
			}
			if original == "" {
				original = response.Body.String()
			} else if response.Body.String() != original {
				t.Fatalf("requests returned distinct orders: %s and %s", original, response.Body.String())
			}
		case 409:
			if response.Header().Get("Idempotency-Status") != "in-progress" {
				t.Fatalf("unexpected conflict: %s", response.Body.String())
			}
			conflicts++
		default:
			t.Fatalf("unexpected idempotency response: %d %s", response.Code, response.Body.String())
		}
	}
	if executed != 1 {
		t.Fatalf("expected one original execution, got %d", executed)
	}
	t.Logf("100 concurrent requests: one execution, %d replays, %d in progress", 99-conflicts, conflicts)

	clear := func() {
		t.Helper()
		key := "typhon:idempotency:" + idempotencyDigest("orders-v1", tenant, "alice", "same")
		if err := cache.client.Del(ctx, key).Err(); err != nil {
			t.Fatal(err)
		}
	}
	assert := func(payload string, code int, header, value string) {
		t.Helper()
		response := sendIdempotent(t, server[0], "/orders", "a", "same", payload)
		if response.Code != code || response.Header().Get(header) != value ||
			(code == 201 && response.Body.String() != original) {
			t.Fatalf("request after reservation loss: %d %s", response.Code, response.Body.String())
		}
	}
	assert("changed", 409, "Idempotency-Status", "conflict")
	clear()
	assert("changed", 409, "Idempotency-Status", "")
	clear()
	assert("widget", 201, "Idempotency-Replayed", "")

	var count int64
	err := admin.QueryRow(ctx, "SELECT count(*) FROM "+schema+".idempotent_orders").Scan(&count)
	if err != nil || count != 1 {
		t.Fatalf("expected a single PostgreSQL order after Redis record loss, got %d: %v", count, err)
	}
}
