package typhon

import (
	"crypto/rand"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

func cacheTestHandler(t *testing.T, dsn, namespace string) http.Handler {
	t.Helper()
	engine := NewEngine()
	t.Cleanup(func() {
		if err := engine.Close(); err != nil {
			t.Error(err)
		}
	})
	module, err := engine.LoadModule(sourceFile(t, fmt.Sprintf(`
import typhon.auth as auth
import typhon.http as http
import typhon.redis as redis
import typhon.web as web
pool: redis.Connection = redis.connect_with_pool(%q, 2, 0, 0.1, 0.1, 0.1, False)
cache: redis.Cache = pool.cache(%q, 1, 2)
other: redis.Cache = pool.cache(%q, 2)
verifier: auth.Authenticator = auth.jwt('https://issuer.example', 'test', 'https://issuer.example/jwks')
policy: auth.Policy = auth.policy({'member': ['cache:use']})
routes: web.Router = web.router()
routes.use_errors()

def get(req: http.Request) -> http.Response:
    item: redis.CacheEntry = cache.lookup(req, req.param('id'))
    if item.hit:
        return http.json(200, item.value)
    return http.json(404, item.value)

def get_other(req: http.Request) -> http.Response:
    item: redis.CacheEntry = other.lookup(req, req.param('id'))
    return http.json(200, item.hit)

def put(req: http.Request) -> http.Response:
    item: redis.CacheEntry = cache.lookup(req, req.param('id'))
    return http.json(200, item.store(req, req.json()))

def fence(req: http.Request) -> http.Response:
    key: str = req.param('id')
    stale: redis.CacheEntry = cache.lookup(req, key)
    cache.invalidate(req, key)
    fresh: redis.CacheEntry = cache.lookup(req, key)
    accepted: bool = fresh.store(req, {'label': 'fresh'})
    rejected: bool = stale.store(req, {'label': 'stale'})
    again: bool = fresh.store(req, {'label': 'second fill'})
    return http.json(200, [accepted, rejected, again])

def stats(req: http.Request) -> http.Response:
    return http.json(200, pool.stats())

routes.add('GET', '/cache/{id}', get)
routes.require_auth('GET', '/cache/{id}', verifier)
routes.authorize('GET', '/cache/{id}', policy, 'cache:use')
routes.add('PUT', '/cache/{id}', put)
routes.require_auth('PUT', '/cache/{id}', verifier)
routes.authorize('PUT', '/cache/{id}', policy, 'cache:use')
routes.add('GET', '/other/{id}', get_other)
routes.require_auth('GET', '/other/{id}', verifier)
routes.authorize('GET', '/other/{id}', policy, 'cache:use')
routes.add('POST', '/fence/{id}', fence)
routes.require_auth('POST', '/fence/{id}', verifier)
routes.authorize('POST', '/fence/{id}', policy, 'cache:use')
routes.add('GET', '/stats', stats)
`, dsn, namespace, namespace)))
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
		if route.auth != nil {
			route.auth = rateFixtureAuth{
				"a": {subject: "alice", tenant: "a:b", roles: []string{"member"}},
				"b": {subject: "bob", tenant: "a", roles: []string{"member"}},
			}
		}
	}
	return httpVMHandler(engine, routes)
}

func cacheTestRequest(t *testing.T, handler http.Handler, method, path, token, body string, status int, want string) {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant", "a")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != status || !strings.Contains(response.Body.String(), want) {
		t.Fatalf("%s %s: got %d %s, expected %d containing %q", method, path, response.Code, response.Body.String(), status, want)
	}
}

func checkRedisTenantCache(t *testing.T, handler http.Handler) {
	t.Helper()
	send := func(method, path, token, body string, status int, want string) {
		t.Helper()
		cacheTestRequest(t, handler, method, path, token, body, status, want)
	}
	send("GET", "/cache/shared", "invalid", "", 401, "")
	send("GET", "/cache/shared", "a", "", 404, "null")
	send("PUT", "/cache/shared", "a", `{"items": [9, null, false]}`, 200, "true")
	send("PUT", "/cache/shared", "b", "null", 200, "true")
	send("GET", "/cache/shared", "a", "", 200, `{"items": [9, null, false]}`)
	send("GET", "/cache/shared", "b", "", 200, "null")
	send("GET", "/cache/b:shared", "b", "", 404, "null")
	send("GET", "/other/shared", "a", "", 200, "false")
	send("POST", "/fence/race", "a", "", 200, "[true, false, false]")
	send("GET", "/cache/race", "a", "", 200, `"fresh"`)
	send("GET", "/stats", "", "", 200, `"cache_hits": 3`)
	send("GET", "/stats", "", "", 200, `"cache_misses": 7`)
	send("GET", "/stats", "", "", 200, `"cache_invalidations": 1`)
}

func TestNativeRedisTenantCache(t *testing.T) {
	server := miniredis.RunT(t)
	handler := cacheTestHandler(t, "redis://"+server.Addr(), "records:cache")
	checkRedisTenantCache(t, handler)
	server.FastForward(3 * time.Second)
	cacheTestRequest(t, handler, "GET", "/cache/shared", "a", "", 404, "null")
	cacheTestRequest(t, handler, "GET", "/cache/shared", "b", "", 404, "null")
	server.Close()
	cacheTestRequest(t, handler, "GET", "/cache/shared", "b", "", 503, "")
	cacheTestRequest(t, handler, "GET", "/stats", "", "", 200, `"cache_errors": 1`)
}

func TestNativeRedisTenantCacheIntegration(t *testing.T) {
	dsn := os.Getenv("TYPHON_TEST_REDIS_URL")
	if dsn == "" {
		t.Skip("set TYPHON_TEST_REDIS_URL to exercise the tenant cache on Redis")
	}
	checkRedisTenantCache(t, cacheTestHandler(t, dsn, "cache-"+rand.Text()))
}
