package typhon

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

type rateFixtureAuth map[string]verifiedRequestIdentity

func (a rateFixtureAuth) verify(_ context.Context, token string) (verifiedRequestIdentity, error) {
	identity, ok := a[token]
	if !ok {
		return verifiedRequestIdentity{}, errInvalidBearer
	}
	return identity, nil
}

func rateFixture(t *testing.T, dsn, namespace string) (*httptest.Server, *httpPoolMetrics) {
	t.Helper()
	engine := NewEngine()
	t.Cleanup(func() {
		if err := engine.Close(); err != nil {
			t.Error(err)
		}
	})
	module, err := engine.LoadModule(sourceFile(t, fmt.Sprintf(`import typhon.auth as auth
import typhon.http as http
import typhon.web as web
import typhon.redis as redis
cache: redis.Connection = redis.connect_with_pool(%q, 2, 0, 1, 1, 1, False)
verifier: auth.Authenticator = auth.jwt('https://issuer.example', 'test', 'https://issuer.example/jwks')
closed: web.RateLimiter = web.rate_limiter(cache, %q, 10, 60, False, {'plan:team': 25})
opened: web.RateLimiter = web.rate_limiter(cache, %q, 10, 60, True)
routes: web.Router = web.router()
routes.use_errors()
def handle(req: http.Request) -> http.Response:
    return http.response(200, 'ok')
routes.add('GET', '/closed', handle)
routes.require_auth('GET', '/closed', verifier)
routes.rate_limit('GET', '/closed', closed)
routes.add('GET', '/open', handle)
routes.require_auth('GET', '/open', verifier)
routes.rate_limit('GET', '/open', opened)
`, dsn, namespace, namespace)))
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ExecuteModule(module, false); err != nil {
		t.Fatal(err)
	}
	router, _ := routerValue(module.Globals["routes"])
	router.frozen = true
	for _, route := range router.routes {
		route.auth = rateFixtureAuth{
			"a":    {subject: "demo-subject", tenant: "tenant-alpha"},
			"b":    {subject: "demo-subject", tenant: "tenant-beta"},
			"team": {subject: "team-member", tenant: "tenant-alpha", scopes: map[string]bool{"plan:team": true}},
		}
	}
	stats := &httpPoolMetrics{}
	engine.httpMetrics.Store(stats)
	server := httptest.NewServer(httpVMHandler(engine, module.Globals["routes"]))
	t.Cleanup(server.Close)
	return server, stats
}

func makeRateRequest(server *httptest.Server, path, caller string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, server.URL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+caller)
	req.Header.Set("X-Forwarded-For", "192.0.2.10")
	req.Header.Set("X-Tenant", "looks-like-tenant")
	resp, err := server.Client().Do(req)
	if err != nil {
		return nil, err
	}
	_, readErr := io.Copy(io.Discard, resp.Body)
	closeErr := resp.Body.Close()
	if readErr != nil {
		return nil, readErr
	}
	return resp, closeErr
}

func testSharedRateLimit(t *testing.T, dsn string) {
	t.Helper()
	namespace := "fixture-" + rand.Text()
	first, s1 := rateFixture(t, dsn, namespace)
	second, s2 := rateFixture(t, dsn, namespace)
	start := make(chan struct{})
	permits := make(chan struct{}, 12)
	var requests sync.WaitGroup
	for i := range 100 {
		server := first
		if i%2 == 1 {
			server = second
		}
		requests.Go(func() {
			<-start
			permits <- struct{}{}
			defer func() { <-permits }()
			resp, err := makeRateRequest(server, "/closed", "a")
			if err != nil {
				t.Error(err)
				return
			}
			if resp.StatusCode != 200 && resp.StatusCode != 429 || resp.Header.Get("X-RateLimit-Limit") != "10" {
				t.Errorf("unexpected rate response: %d %v", resp.StatusCode, resp.Header)
			}
			if resp.StatusCode == 429 {
				wait, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
				if wait < 1 || wait > 60 || resp.Header.Get("X-RateLimit-Remaining") != "0" {
					t.Errorf("invalid 429 headers: %v", resp.Header)
				}
			}
		})
	}
	close(start)
	requests.Wait()
	if got := s1.rateAllowed.Load() + s2.rateAllowed.Load(); got != 10 {
		t.Fatalf("10 requests should reach the handlers, got %d", got)
	}
	if got := s1.rateDenied.Load() + s2.rateDenied.Load(); got != 90 {
		t.Fatalf("90 requests should be rejected, got %d", got)
	}
	for _, tc := range []struct {
		who, limit, remaining string
	}{{"b", "10", "9"}, {"team", "25", "24"}} {
		resp, err := makeRateRequest(second, "/closed", tc.who)
		if err != nil || resp.StatusCode != 200 || resp.Header.Get("X-RateLimit-Limit") != tc.limit ||
			resp.Header.Get("X-RateLimit-Remaining") != tc.remaining {
			t.Fatalf("tenant or plan not isolated: %v %v", resp, err)
		}
	}
}

func TestNativeDistributedRateLimit(t *testing.T) {
	cache := miniredis.RunT(t)
	testSharedRateLimit(t, "redis://"+cache.Addr())
	for _, key := range cache.Keys() {
		if !strings.HasPrefix(key, "typhon:rate:") || strings.Contains(key, "tenant-alpha") ||
			strings.Contains(key, "demo-subject") || cache.TTL(key) <= 0 {
			t.Fatalf("unbounded or identifying rate limit key: %q", key)
		}
	}
	cache.FastForward(61 * time.Second)
	if len(cache.Keys()) != 0 {
		t.Fatal("rate limit state outlived the window")
	}
}

func TestNativeDistributedRateLimitIntegration(t *testing.T) {
	dsn := os.Getenv("TYPHON_TEST_REDIS_URL")
	if dsn == "" {
		t.Skip("set TYPHON_TEST_REDIS_URL to exercise limits across instances using Redis")
	}
	testSharedRateLimit(t, dsn)
}

func TestNativeDistributedRateLimitCapacityAndFallback(t *testing.T) {
	cache := miniredis.RunT(t)
	dsn := "redis://" + cache.Addr()
	client := redis.NewClient(&redis.Options{Addr: cache.Addr()})
	defer client.Close()
	for i, allowed := range []int64{1, 1, 0} {
		reply, err := webRateScript.Run(t.Context(), client, []string{"index", fmt.Sprint(i)}, 1, 1000, 2).Int64Slice()
		if err != nil || len(reply) != 3 || reply[0] != allowed {
			t.Fatalf("counter capacity: %v %v", reply, err)
		}
	}
	cache.Close()
	server, stats := rateFixture(t, dsn, "offline")
	start := time.Now()
	for _, tc := range []struct {
		path   string
		status int
	}{{"/closed", 503}, {"/open", 200}} {
		resp, err := makeRateRequest(server, tc.path, "a")
		if err != nil || resp.StatusCode != tc.status {
			t.Fatalf("%s: %v %v", tc.path, resp, err)
		}
	}
	if stats.rateErrors.Load() != 2 || time.Since(start) >= time.Second {
		t.Fatal("unavailable Redis blocked the API or skipped failure metrics")
	}
}
