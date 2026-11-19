package typhon

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestNativeRequestQuotasAndRecovery(t *testing.T) {
	t.Setenv("TYPHON_LIMIT_REQUEST_BYTES", "4096")
	t.Setenv("TYPHON_LIMIT_INSTRUCTIONS", "350")
	engine := NewEngine()
	defer engine.Close()
	module, err := engine.LoadModule(sourceFile(t, `import json
import typhon.http as http
def task_work() -> None:
    i: int = 0
    while i < 14:
        i = i + 1
def handle(req: http.Request) -> http.Response:
    if req.path == '/work':
        while True:
            x: int = 1
    if req.path == '/task':
        a: TyphonTask = spawn(task_work)
        b: TyphonTask = spawn(task_work)
        c: TyphonTask = spawn(task_work)
        join(a)
        join(b)
        join(c)
    if req.path == '/bytes':
        first: str = 'a' * 2500
        second: str = first + first
    if req.path == '/json':
        json.loads(req.body)
    return http.response(200, 'ready')
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ExecuteModule(module, false); err != nil {
		t.Fatal(err)
	}
	var metrics httpPoolMetrics
	engine.httpMetrics.Store(&metrics)
	handler := httpVMHandler(engine, module.Globals["handle"])
	for _, tc := range []struct {
		path, body, reason string
		status             int
	}{
		{"/bytes", "", "byte budget exceeded", 422},
		{"/work", "", "instruction budget exceeded", 422},
		{"/task", "", "instruction budget exceeded", 422},
		{"/json", "[" + strings.Repeat("0,", 50) + "0]", "byte budget exceeded", 422},
		{"/", strings.Repeat("x", 4096), "request body is too large", 413},
		{"/", "", "ready", 200},
	} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body)))
		if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.reason) {
			t.Errorf("%s: got %d %q; expected %d %q", tc.path, w.Code, w.Body.String(), tc.status, tc.reason)
		}
	}
	if got := metrics.quotaRejected.Load(); got != 5 {
		t.Fatalf("quota rejection count = %d", got)
	}
}

func TestNativeLivenessBypassesBusyWorkers(t *testing.T) {
	pool := newHTTPWorkerPool(context.Background(), 1, 0)
	pool.liveness = "/_alive"
	started, release, complete := make(chan struct{}), make(chan struct{}), make(chan struct{})
	pool.idle <- http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		w.WriteHeader(200)
	})
	go func() {
		defer close(complete)
		workerStatus(pool, context.Background())
	}()
	defer func() { close(release); <-complete }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("worker did not start")
	}
	if got := workerStatus(pool, context.Background()); got != 503 {
		t.Fatalf("busy worker accepted ordinary request: %d", got)
	}
	w := httptest.NewRecorder()
	pool.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/_alive", nil))
	if w.Code != 204 {
		t.Fatalf("liveness depended on a busy worker: %d", w.Code)
	}
}

func TestQuotaConfiguration(t *testing.T) {
	t.Setenv("TYPHON_LIMIT_HTTP_QUEUE", "-1")
	engine := NewEngine()
	defer engine.Close()
	module, err := engine.LoadModule(sourceFile(t, "print('unreachable')\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ExecuteModule(module, true); err == nil || !strings.Contains(err.Error(), "TYPHON_LIMIT_HTTP_QUEUE") {
		t.Fatalf("invalid quota did not prevent startup: %v", err)
	}
}

func TestNativePostgresQuotaAndRollbackIntegration(t *testing.T) {
	dsn := os.Getenv("TYPHON_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("set TYPHON_TEST_POSTGRES_URL for a dedicated test PostgreSQL")
	}
	t.Setenv("TYPHON_LIMIT_REQUEST_BYTES", "8192")
	t.Setenv("TYPHON_LIMIT_SQL_ROWS", "2")
	ctx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(driverFailure("PostgreSQL", connectionKind, err))
	}
	table := pgx.Identifier{fmt.Sprintf("typhon_quota_%d", time.Now().UnixNano())}.Sanitize()
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		if _, err := admin.Exec(cleanup, "DROP TABLE IF EXISTS "+table); err != nil {
			t.Error(driverFailure("PostgreSQL", databaseKind, err))
		}
		if err := admin.Close(cleanup); err != nil {
			t.Error(driverFailure("PostgreSQL", connectionKind, err))
		}
	})
	if _, err := admin.Exec(ctx, "CREATE TABLE "+table+" (id int)"); err != nil {
		t.Fatal(driverFailure("PostgreSQL", databaseKind, err))
	}
	engine := NewEngine()
	defer engine.Close()
	module, err := engine.LoadModule(sourceFile(t, fmt.Sprintf(`import typhon.http as http
import typhon.postgres as pg
db: pg.Connection = pg.connect(%q, 2)
def handle(req: http.Request) -> http.Response:
    if req.path == '/bytes' or req.path == '/rows':
        tx: pg.Transaction = db.begin()
        tx.exec(%q, [1])
        if req.path == '/bytes':
            tx.query('SELECT repeat($1::text, $2::int)', ['x', 65536])
        else:
            tx.query('SELECT generate_series(1, 3)')
        tx.commit()
    return http.json(200, db.query('SELECT 1 AS ready').get(0, 'ready'))
`, dsn, "INSERT INTO "+table+" (id) VALUES ($1)")))
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ExecuteModule(module, false); err != nil {
		t.Fatal(err)
	}
	handler := httpVMHandler(engine, module.Globals["handle"])
	for i := range 30 {
		path, reason, status := "/", "1", 200
		switch i % 3 {
		case 0:
			path, reason, status = "/bytes", "byte budget exceeded", 422
		case 1:
			path, reason, status = "/rows", "SQL row budget exceeded", 422
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != status || !strings.Contains(w.Body.String(), reason) {
			t.Fatalf("%d %s returned %d %q", i, path, w.Code, w.Body)
		}
	}
	var count int
	if err := admin.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil || count != 0 {
		t.Fatalf("quota exhaustion left %d committed rows: %v", count, driverFailure("PostgreSQL", databaseKind, err))
	}
}
