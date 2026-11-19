package typhon

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestNativePostgresPoolRejectsInvalidSettings(t *testing.T) {
	for _, options := range []string{
		"{'max_connections': 0}", "{'max_connections': 1, 'min_connections': 2}",
		"{'acquire_seconds': 0}", "{'grace_seconds': 3}", "{'typed': 'yes'}",
		"{'require_tls': True}", "{'share': 'invalid pool name'}", "{'typo': 1}",
	} {
		source := fmt.Sprintf(`import typhon.postgres as pg
options: dict[str, Any] = %s
db: pg.Connection = pg.connect_pooled('postgres://user@127.0.0.1:1/db?sslmode=disable', options)
`, options)
		err := RunFile(sourceFile(t, source))
		if err == nil || !strings.Contains(err.Error(), "ValidationError") || strings.Contains(err.Error(), "postgres://") {
			t.Fatalf("pool configuration error is missing or unsafe for %s: %v", options, err)
		}
	}
}

func TestNativePostgresPoolSharedHTTPWorkersIntegration(t *testing.T) {
	dsn := os.Getenv("TYPHON_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("set TYPHON_TEST_POSTGRES_URL to test pool sharing with PostgreSQL")
	}
	entered, release := make(chan struct{}, 1), make(chan struct{})
	barrier := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		entered <- struct{}{}
		select {
		case <-req.Context().Done():
		case <-release:
		}
		w.WriteHeader(204)
	}))
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
		barrier.Close()
	}()
	engine := NewEngine()
	defer engine.Close()
	module, err := engine.LoadModule(sourceFile(t, fmt.Sprintf(`import typhon.http as http
import typhon.postgres as pg
options: dict[str, Any] = {'share': 'api', 'max_connections': 1, 'acquire_seconds': 0.15, 'query_seconds': 1}
db: pg.Connection = pg.connect_pooled(%q, options)
upstream: http.Client = http.client(5)
def handler(req: http.Request) -> http.Response:
    if req.path == '/hold':
        tx: pg.Transaction = db.begin()
        try:
            upstream.get(%q)
            tx.commit()
        finally:
            tx.rollback()
        return http.response(200, 'committed')
    try:
        pid: Any = db.query('SELECT pg_backend_pid() AS id').get(0, 'id')
        return http.json(200, pid)
    except TimeoutError:
        return http.response(504, 'acquire deadline')
`, dsn, barrier.URL)))
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ExecuteModule(module, false); err != nil {
		t.Fatal(err)
	}
	pool := newHTTPWorkerPool(engine.ctx, 3, 8)
	defer pool.close()
	if err := pool.load(&VM{engine: engine}, module.Globals["handler"]); err != nil {
		t.Fatal(err)
	}
	mainDB := module.Globals["db"].NativeObject.data.(*postgresConnection)
	for _, worker := range pool.engines {
		handle := worker.modules[module.Path].Globals["db"].NativeObject.data.(*postgresConnection)
		if handle == mainDB || handle.pool != mainDB.pool {
			t.Fatal("each worker needs a private handle over the same physical pool")
		}
	}
	server := httptest.NewServer(pool)
	defer server.Close()
	client := server.Client()
	client.Timeout = 3 * time.Second
	held := make(chan error, 1)
	go func() {
		resp, err := client.Get(server.URL + "/hold")
		if resp != nil {
			resp.Body.Close()
			if resp.StatusCode != 200 {
				err = fmt.Errorf("hold request returned %d", resp.StatusCode)
			}
		}
		held <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first transaction did not reserve the shared connection")
	}
	resp, err := client.Get(server.URL + "/probe")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	stats := mainDB.pool.Stat()
	if err != nil || resp.StatusCode != 504 || string(body) != "acquire deadline" ||
		stats.TotalConns() != 1 || stats.AcquiredConns() != 1 || stats.CanceledAcquireCount() == 0 {
		t.Fatalf("pool did not remain bounded under contention: %d %s %v", resp.StatusCode, body, err)
	}
	close(release)
	if err := <-held; err != nil {
		t.Fatal(err)
	}
	resp, err = client.Get(server.URL + "/probe")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || mainDB.pool.Stat().TotalConns() != 1 {
		t.Fatal("committing did not free the shared connection")
	}
	if err := pool.close(); err != nil {
		t.Fatal(err)
	}
	if err := mainDB.pool.Ping(t.Context()); err != nil {
		t.Fatal("closing worker handles unexpectedly closed the main pool")
	}
}

func TestNativePostgresPoolGraceAndFailoverIntegration(t *testing.T) {
	dsn := os.Getenv("TYPHON_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("set TYPHON_TEST_POSTGRES_URL to test lifecycle and failover")
	}
	uri, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(driverFailure("PostgreSQL", validationKind, err))
	}
	unused, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, absentPort, err := net.SplitHostPort(unused.Addr().String())
	unused.Close()
	if err != nil {
		t.Fatal(err)
	}
	absent := *uri
	absent.Host = net.JoinHostPort("127.0.0.1", absentPort)
	fallback := *uri
	host, port := uri.Hostname(), uri.Port()
	if port == "" {
		port = "5432"
	}
	q := fallback.Query()
	q.Set("host", "127.0.0.1,"+host)
	q.Set("port", absentPort+","+port)
	fallback.RawQuery = q.Encode()
	source := fmt.Sprintf(`import typhon.postgres as pg
cfg: dict[str, Any] = {'share': 'writer', 'max_connections': 2, 'grace_seconds': 0.25, 'connect_seconds': 1}
try:
    pg.connect_pooled(%q, cfg)
except Error:
    print('initialization failed safely')
db: pg.Connection = pg.connect_pooled(%q, cfg)
next: pg.Connection = pg.connect_pooled(%q, cfg)
other: dict[str, Any] = {'share': 'writer', 'max_connections': 3}
try:
    pg.connect_pooled(%q, other)
except ValidationError:
    print('configuration conflict')
tx: pg.Transaction = db.begin()
tx.exec('SELECT 1')
db.close()
db.close()
try:
    tx.query('SELECT 1')
except ValidationError:
    print('rolled back')
try:
    db.query('SELECT 1')
except ValidationError:
    print('closed')
print(next.query('SELECT 42 AS value').get(0, 'value'))
print(next.stats()['max_connections'])
another: pg.Connection = pg.connect_pooled(%q, cfg)
def commit(t: pg.Transaction) -> None:
    t.commit()
    print('committed during grace')
pending: pg.Transaction = another.begin()
job: TyphonTask = spawn(commit, pending)
another.close()
join(job)
`, absent.String(), fallback.String(), fallback.String(), fallback.String(), fallback.String())
	got, err := captureRun(t, sourceFile(t, source))
	if err != nil || got != "initialization failed safely\nconfiguration conflict\nrolled back\nclosed\n42\n2\ncommitted during grace\n" {
		t.Fatalf("unexpected startup, failover, or handle close: %q %v", got, err)
	}

	engine := NewEngine()
	defer engine.Close()
	module, err := engine.LoadModule(sourceFile(t, fmt.Sprintf(`import typhon.postgres as pg
options: dict[str, Any] = {'max_connections': 1, 'health_seconds': 0.05}
db: pg.Connection = pg.connect_pooled(%q, options)
`, dsn)))
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ExecuteModule(module, false); err != nil {
		t.Fatal(err)
	}
	db := module.Globals["db"].NativeObject.data.(*postgresConnection)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(driverFailure("PostgreSQL", connectionKind, err))
	}
	defer admin.Close(context.Background())
	var pid, newPID int32
	if err := db.pool.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatal(driverFailure("PostgreSQL", databaseKind, err))
	}
	var killed bool
	if err := admin.QueryRow(ctx, "SELECT pg_terminate_backend($1)", pid).Scan(&killed); err != nil || !killed {
		t.Fatalf("could not interrupt the test client's backend: %v", driverFailure("PostgreSQL", databaseKind, err))
	}
	for ctx.Err() == nil {
		if err := db.pool.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&newPID); err == nil && newPID != pid {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("pool did not replace the failed PostgreSQL connection")
}
