package typhon

import (
	"context"
	"crypto/rand"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/jackc/pgx/v5"
)

func healthTestHandler(t *testing.T, p *httpWorkerPool) *httpDrain {
	t.Helper()
	health, err := newHTTPHealth(t.Context(), "/ready", "/live")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(health.close)
	p.liveness = "/live"
	gate := newHTTPDrain(p, p, "/ready")
	gate.health = health
	return gate
}

func healthTestRequest(handler http.Handler, path, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	return response
}

func waitHealth(t *testing.T, handler http.Handler, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if response := healthTestRequest(handler, "/ready", ""); response.Code == want {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("readiness did not reach status %d", want)
}

func TestNativeHTTPReadinessCacheCapacityAndDiagnostics(t *testing.T) {
	redis := miniredis.RunT(t)
	t.Setenv("TYPHON_HEALTH_POSTGRES_URL", "")
	t.Setenv("TYPHON_HEALTH_REDIS_URL", "redis://"+redis.Addr())
	t.Setenv("TYPHON_HEALTH_DIAGNOSTICS_PATH", "/diagnostics")
	token := rand.Text() + rand.Text()
	t.Setenv("TYPHON_HEALTH_DIAGNOSTICS_TOKEN", token)
	pool := newHTTPWorkerPool(t.Context(), 1, 0)
	handler := healthTestHandler(t, pool)
	if response := healthTestRequest(handler, "/ready", ""); response.Code != 503 || response.Body.Len() != 0 {
		t.Fatal("readiness leaked dependency details or skipped startup hysteresis")
	}
	waitHealth(t, handler, 200)

	commands := redis.CommandCount()
	for range 100 {
		if got := healthTestRequest(handler, "/ready", "").Code; got != 200 {
			t.Fatalf("unexpected readiness during probe burst: %d", got)
		}
	}
	if delta := redis.CommandCount() - commands; delta > 2 {
		t.Fatalf("public probe traffic generated %d Redis commands", delta)
	}
	if got := healthTestRequest(handler, "/diagnostics", "invalid"); got.Code != 401 || got.Body.Len() != 0 {
		t.Fatalf("unauthorized diagnostic response: %d %s", got.Code, got.Body.String())
	}
	if got := healthTestRequest(handler, "/diagnostics", token); got.Code != 200 ||
		!strings.Contains(got.Body.String(), `"redis":"healthy"`) || strings.Contains(got.Body.String(), redis.Addr()) {
		t.Fatalf("unexpected diagnostic: %d %s", got.Code, got.Body.String())
	}

	running := make(chan struct{})
	release := make(chan struct{})
	pool.idle <- http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(running)
		<-release
		w.WriteHeader(200)
	})
	work := make(chan int, 1)
	go func() { work <- healthTestRequest(handler, "/long", "").Code }()
	<-running
	live, ready := healthTestRequest(handler, "/live", "").Code, healthTestRequest(handler, "/ready", "").Code
	close(release)
	if processed := <-work; live != 204 || ready != 503 || processed != 200 {
		t.Fatalf("busy worker blocked host probe or readiness missed capacity: live=%d ready=%d work=%d", live, ready, processed)
	}
	redis.Close()
	waitHealth(t, handler, 503)
	if got := healthTestRequest(handler, "/live", "").Code; got != 204 {
		t.Fatalf("Redis outage affected liveness: %d", got)
	}
	if got := healthTestRequest(handler, "/diagnostics", token).Body.String(); !strings.Contains(got, `"redis":"unavailable"`) {
		t.Fatalf("diagnostics did not identify Redis: %s", got)
	}
}

func TestNativePostgresLazyPool(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	result, err := captureRun(t, sourceFile(t, fmt.Sprintf(`
import typhon.postgres as pg
db: pg.Connection = pg.connect_pooled(%q, {'lazy': True, 'connect_seconds': 0.1, 'acquire_seconds': 0.1})
print('loaded')
db.close()
`, "postgres://fixture@"+address+"/typhon?sslmode=disable")))
	if err != nil || result != "loaded\n" {
		t.Fatalf("lazy pool contacted the unavailable dependency at startup: %q: %v", result, err)
	}
}

func TestNativeHTTPReadinessPostgresRecoveryIntegration(t *testing.T) {
	base := os.Getenv("TYPHON_TEST_POSTGRES_URL")
	if base == "" {
		t.Skip("set TYPHON_TEST_POSTGRES_URL to exercise PostgreSQL recovery")
	}
	ctx, stop := context.WithTimeout(t.Context(), 15*time.Second)
	defer stop()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(driverFailure("PostgreSQL", connectionKind, err))
	}
	name := "typhon_probe_" + strings.ToLower(rand.Text())
	db := pgx.Identifier{name}.Sanitize()
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		if _, err := admin.Exec(cleanup, "DROP DATABASE IF EXISTS "+db+" WITH (FORCE)"); err != nil {
			t.Error(driverFailure("PostgreSQL", databaseKind, err))
		}
		if err := admin.Close(cleanup); err != nil {
			t.Error(driverFailure("PostgreSQL", connectionKind, err))
		}
	})
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+db); err != nil {
		t.Fatal(driverFailure("PostgreSQL", databaseKind, err))
	}
	target, err := url.Parse(base)
	if err != nil {
		t.Fatal("test PostgreSQL URL is invalid")
	}
	target.Path = "/" + name
	t.Setenv("TYPHON_HEALTH_POSTGRES_URL", target.String())
	t.Setenv("TYPHON_HEALTH_REDIS_URL", "")
	t.Setenv("TYPHON_HEALTH_DIAGNOSTICS_PATH", "")
	handler := healthTestHandler(t, newHTTPWorkerPool(t.Context(), 1, 1))
	waitHealth(t, handler, 200)
	if _, err := admin.Exec(ctx, "ALTER DATABASE "+db+" ALLOW_CONNECTIONS false"); err != nil {
		t.Fatal(driverFailure("PostgreSQL", databaseKind, err))
	}
	if _, err := admin.Exec(ctx, "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1", name); err != nil {
		t.Fatal(driverFailure("PostgreSQL", databaseKind, err))
	}
	waitHealth(t, handler, 503)
	if got := healthTestRequest(handler, "/live", "").Code; got != 204 {
		t.Fatalf("PostgreSQL outage affected liveness: %d", got)
	}
	if _, err := admin.Exec(ctx, "ALTER DATABASE "+db+" ALLOW_CONNECTIONS true"); err != nil {
		t.Fatal(driverFailure("PostgreSQL", databaseKind, err))
	}
	waitHealth(t, handler, 200)
}
