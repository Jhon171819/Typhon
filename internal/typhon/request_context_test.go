package typhon

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	redistest "github.com/alicebob/miniredis/v2/server"
	"github.com/jackc/pgx/v5"
)

func TestNativeRequestBodyDeadline(t *testing.T) {
	engine := NewEngine()
	defer engine.Close()
	module, err := engine.LoadModule(sourceFile(t, `import typhon.http as http
def handle(req: http.Request) -> http.Response:
    return http.response(200, req.body)
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ExecuteModule(module, false); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(httpVMHandlerWithTimeout(engine, module.Globals["handle"], 100*time.Millisecond))
	defer server.Close()
	conn, err := net.Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprint(conn, "POST / HTTP/1.1\r\nHost: localhost\r\nContent-Length: 2\r\nConnection: close\r\n\r\na"); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 504 {
		t.Fatalf("partial body exceeded the request deadline: %d", response.StatusCode)
	}
}

func TestNativeRequestDeadlineAndJoinIsolation(t *testing.T) {
	engine := NewEngine()
	defer engine.Close()
	module, err := engine.LoadModule(sourceFile(t, `import typhon.http as http
seen: list[bool] = []
def spin() -> None:
    while True:
        iterations: int = 1
def handle(req: http.Request) -> http.Response:
    if req.path == '/remaining':
        return http.json(200, req.remaining() > 0 and req.remaining() <= 1)
    if req.path == '/join':
        task: TyphonTask = spawn(spin)
        join(task, 0.01)
    try:
        if req.path == '/spin':
            spin()
    finally:
        seen.append(req.cancelled())
    return http.response(200, 'ready')
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ExecuteModule(module, false); err != nil {
		t.Fatal(err)
	}
	handler := httpVMHandlerWithTimeout(engine, module.Globals["handle"], time.Second)
	for _, tc := range []struct {
		path, body string
		status     int
		deadline   time.Duration
	}{
		{"/remaining", "true", 200, time.Second},
		{"/spin", "request deadline exceeded\n", 504, 20 * time.Millisecond},
		{"/join", "request deadline exceeded\n", 504, time.Second},
		{"/ok", "ready", 200, time.Second},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), tc.deadline)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, tc.path, nil).WithContext(ctx))
		cancel()
		if response.Code != tc.status || response.Body.String() != tc.body {
			t.Fatalf("%s returned %d %q", tc.path, response.Code, response.Body)
		}
	}
	if got := module.Globals["seen"].String(); got != "[True, False]" {
		t.Fatalf("cancelled request did not inspect its original context in finally: %s", got)
	}
}

func TestNativeRedisRequestCancellation(t *testing.T) {
	redis := miniredis.RunT(t)
	arrived, release := make(chan struct{}, 2), make(chan struct{})
	defer close(release)
	if err := redis.Server().Register("TYPHONSLOW", func(c *redistest.Peer, _ string, _ []string) {
		arrived <- struct{}{}
		<-release
		c.WriteInline("OK")
	}); err != nil {
		t.Fatal(err)
	}
	engine := NewEngine()
	defer engine.Close()
	module, err := engine.LoadModule(sourceFile(t, fmt.Sprintf(`import typhon.http as http
import typhon.redis as redis
cache: redis.Connection = redis.connect(%q, 3)
def handle(req: http.Request) -> http.Response:
    if req.path == '/slow':
        cache.command(['TYPHONSLOW'])
    return http.response(200, cache.ping())
`, "redis://"+redis.Addr())))
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ExecuteModule(module, false); err != nil {
		t.Fatal(err)
	}
	handler := httpVMHandler(engine, module.Globals["handle"])
	for _, tc := range []struct {
		due         time.Duration
		cancelEarly bool
		status      int
	}{{2 * time.Second, true, statusClientClosedRequest}, {100 * time.Millisecond, false, 504}} {
		ctx, cancel := context.WithTimeout(context.Background(), tc.due)
		result := make(chan int, 1)
		go func() {
			out := httptest.NewRecorder()
			handler.ServeHTTP(out, httptest.NewRequest(http.MethodGet, "/slow", nil).WithContext(ctx))
			result <- out.Code
		}()
		select {
		case <-arrived:
		case <-time.After(2 * time.Second):
			cancel()
			t.Fatal("Redis command did not reach the server")
		}
		if tc.cancelEarly {
			cancel()
		}
		select {
		case code := <-result:
			if code != tc.status {
				t.Errorf("got %d, want %d", code, tc.status)
			}
		case <-time.After(700 * time.Millisecond):
			cancel()
			t.Fatal("Redis socket waited for its longer operation timeout")
		}
		cancel()
		next := httptest.NewRecorder()
		handler.ServeHTTP(next, httptest.NewRequest(http.MethodGet, "/", nil))
		if next.Code != 200 || next.Body.String() != "PONG" {
			t.Fatalf("Redis connection was not reusable: %d %s", next.Code, next.Body)
		}
	}
}

func TestNativePostgresRequestCancellationIntegration(t *testing.T) {
	dsn := os.Getenv("TYPHON_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("set TYPHON_TEST_POSTGRES_URL for a dedicated test PostgreSQL")
	}
	ctx, stop := context.WithTimeout(context.Background(), 12*time.Second)
	defer stop()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(driverFailure("PostgreSQL", connectionKind, err))
	}
	defer admin.Close(ctx)
	engine := NewEngine()
	defer engine.Close()
	marker := fmt.Sprintf("typ_context_%d", time.Now().UnixNano())
	module, err := engine.LoadModule(sourceFile(t, fmt.Sprintf(`import typhon.http as http
import typhon.postgres as pg
db: pg.Connection = pg.connect(%q, 5)
def handle(req: http.Request) -> http.Response:
    if req.path == '/slow':
        tx: pg.Transaction = db.begin()
        tx.exec(%q)
    return http.json(200, db.query('SELECT 1 AS ready').get(0, 'ready'))
`, dsn, "SELECT pg_sleep(8) /* "+marker+" */")))
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ExecuteModule(module, false); err != nil {
		t.Fatal(err)
	}
	handler := httpVMHandler(engine, module.Globals["handle"])
	server := httptest.NewServer(handler)
	defer server.Close()
	requestCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, server.URL+"/slow", nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		response, err := server.Client().Do(req)
		if response != nil {
			response.Body.Close()
		}
		done <- err
	}()
	waitActive := func(want bool) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for {
			var active bool
			err := admin.QueryRow(ctx, "SELECT EXISTS (SELECT FROM pg_stat_activity WHERE pid <> pg_backend_pid() AND state = 'active' AND query LIKE $1)", "%"+marker+"%").Scan(&active)
			if err != nil {
				t.Fatal(driverFailure("PostgreSQL", databaseKind, err))
			}
			if active == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("PostgreSQL activity remained %v, expected %v", active, want)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitActive(true)
	cancel()
	if err := <-done; err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("client disconnect: %v", err)
	}
	waitActive(false)
	next := httptest.NewRecorder()
	handler.ServeHTTP(next, httptest.NewRequest(http.MethodGet, "/", nil))
	if next.Code != 200 || next.Body.String() != "1" {
		t.Fatalf("worker unavailable after cancellation: %d", next.Code)
	}
}
