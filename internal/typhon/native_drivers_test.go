package typhon

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/jackc/pgx/v5"
)

func TestNativeDriverContracts(t *testing.T) {
	for _, tc := range []struct{ source, want string }{
		{"from typhon.http import missing\n", "has no export"},
		{"import typhon.http as http\nhttp.missing()\n", "has no attribute"},
		{"from typhon.http import client\nclient('bad')\n", "argument 1"},
		{"from typhon.http import response\nresponse(200)\n", "arguments"},
		{"from typhon.http import serve\ndef bad(x: str) -> str:\n    return x\nserve('127.0.0.1:0', bad)\n", "argument 2"},
		{"from typhon.postgres import Connection\ndef f(db: Connection) -> None:\n    db.exec('SELECT 1', 'bad')\n", "argument 2"},
		{"from typhon.redis import Connection\ndef f(db: Connection) -> None:\n    db.set('key', 1)\n", "argument 2"},
	} {
		if err := CheckFile(sourceFile(t, tc.source)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("want %q for %s, got %v", tc.want, tc.source, err)
		}
	}
	for _, tc := range []struct{ source, want string }{
		{"from typhon.http import client\nx: Any = 'bad'\nclient(x)\n", "expected float"},
		{"from typhon.http import response\nresponse(200, 'ok', [['bad name', 'value']])\n", "header name"},
		{"from typhon.http import response\nresponse(200, 'ok', [['X-Test', '\x01']])\n", "header value"},
		{"from typhon.http import client\nc: Any = client()\nc.close()\nc.get('http://localhost')\n", "client is closed"},
		{"from typhon.http import response\nresponse(99, 'bad')\n", "status"},
	} {
		if err := RunFile(sourceFile(t, tc.source)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("want %q, got %v", tc.want, err)
		}
	}
}

func TestNativeHTTPClient(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Reply", "native")
		w.WriteHeader(201)
		fmt.Fprintf(w, "%s:%s:%s", r.Method, body, r.Header.Get("X-Test"))
	}))
	defer server.Close()
	source := fmt.Sprintf(`import typhon.http as http
c: http.Client = http.client(2)
r: http.Response = c.request('POST', %q, 'hello', [['X-Test', 'yes']])
print(r.status, r.body, r.header('X-Reply'), c is c)
c.close()
`, server.URL)
	output, err := captureRun(t, sourceFile(t, source))
	if err != nil || output != "201 POST:hello:yes native True\n" {
		t.Fatalf("%q, %v", output, err)
	}
	// Imports must never create a Python worker.
	e := NewEngine()
	defer e.Close()
	m, err := e.LoadModule(sourceFile(t, "import typhon.http as http\nimport typhon.postgres as pg\nimport typhon.redis as redis\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err = e.ExecuteModule(m, true); err != nil {
		t.Fatal(err)
	}
	if e.bridge != nil {
		t.Fatal("native drivers started Python")
	}
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer blocked.Close()
	err = RunFile(sourceFile(t, fmt.Sprintf("from typhon.http import Client, client\nc: Client = client(0.05)\nc.get(%q)\n", blocked.URL)))
	if err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
		t.Fatal("HTTP timeout:", err)
	}
}

func TestNativeHTTPServer(t *testing.T) {
	e := NewEngine()
	defer e.Close()
	m, err := e.LoadModule(sourceFile(t, `from typhon.http import Request, Response, response, json
def handle(req: Request) -> Response:
    if req.path == '/fail':
        bad: int = 1 // 0
    if req.path == '/json':
        return json(200, [9007199254740993, True, None])
    return response(202, req.method + ':' + req.query('name') + ':' + req.body, [['X-Reply', req.header('X-Test')]])
`))
	if err != nil {
		t.Fatal(err)
	}
	if err = e.ExecuteModule(m, false); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(httpVMHandler(e, m.Globals["handle"]))
	defer server.Close()
	for _, tc := range []struct {
		path   string
		status int
		body   string
	}{
		{"/fail", 500, "internal server error\n"},
		{"/?name=Ada", 202, "POST:Ada:hello"},
		{"/json", 200, "[9007199254740993, true, null]"},
	} {
		req, _ := http.NewRequest("POST", server.URL+tc.path, strings.NewReader("hello"))
		req.Header.Set("X-Test", "yes")
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != tc.status || string(body) != tc.body {
			t.Fatalf("%s: %d %s", tc.path, resp.StatusCode, body)
		}
		if tc.status == 202 && resp.Header.Get("X-Reply") != "yes" {
			t.Fatal("missing response header")
		}
	}
	resp, err := server.Client().Post(server.URL, "text/plain", strings.NewReader(strings.Repeat("x", maxHTTPBody+1)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 413 {
		t.Fatalf("oversize request returned %d", resp.StatusCode)
	}
}

func TestNativeHTTPServeCancellation(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := sourceFile(t, fmt.Sprintf(`from typhon.http import Request, Response, response, serve
def handle(req: Request) -> Response:
    return response(200, 'alive')
serve(%q, handle)
`, address))
	done := make(chan error, 1)
	go func() { done <- RunFileWithOptions(path, RunOptions{Context: ctx}) }()
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := client.Get("http://" + address)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Fatal(resp.StatusCode)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server did not start:", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(6 * time.Second):
		t.Fatal("server did not shut down")
	}
	if resp, err := client.Get("http://" + address); err == nil {
		resp.Body.Close()
		t.Fatal("listener remained open")
	}
}

func testRedisProgram(t *testing.T, url string) {
	t.Helper()
	source := fmt.Sprintf(`from typhon.redis import Connection, connect
db: Connection = connect(%q, 2)
print(db.ping(), db.get('typhon:test:missing'))
db.delete('typhon:test:count')
print(db.incr('typhon:test:count'), db.incr('typhon:test:count'))
db.set('typhon:test:value', 'hello', 60)
print(db.get('typhon:test:value'), db.exists('typhon:test:value'), db.ttl('typhon:test:value') > 0)
db.set('typhon:test:persistent', 'ok')
print(db.ttl('typhon:test:persistent'), db.ttl('typhon:test:missing'))
print(db.command(['MGET', 'typhon:test:value', 'typhon:test:missing']))
print(db.delete('typhon:test:value'), db.exists('typhon:test:value'))
db.delete('typhon:test:count')
db.delete('typhon:test:persistent')
db.close()
db.close()
`, url)
	output, err := captureRun(t, sourceFile(t, source))
	want := "PONG None\n1 2\nhello True True\n-1 -2\n[hello, None]\n1 False\n"
	if err != nil || output != want {
		t.Fatalf("got %q (%v), want %q", output, err, want)
	}
}

func TestNativeRedis(t *testing.T) {
	server := miniredis.RunT(t)
	url := "redis://" + server.Addr() + "/0"
	testRedisProgram(t, url)
	source := fmt.Sprintf("from typhon.redis import Connection, connect\ndb: Connection = connect(%q)\ndb.set('expires', 'yes', 0.05)\n", url)
	if err := RunFile(sourceFile(t, source)); err != nil {
		t.Fatal(err)
	}
	server.FastForward(60 * time.Millisecond)
	if server.Exists("expires") {
		t.Fatal("TTL did not expire")
	}
	for _, statement := range []string{"db.set('x', 'y', -1)", "db.command([])", "db.close()\ndb.ping()", "db.command(['UNKNOWN_COMMAND'])", "db.command(['SELECT', '1'])", "db.command(['MULTI'])"} {
		source := fmt.Sprintf("from typhon.redis import Connection, connect\ndb: Connection = connect(%q)\n%s\n", url, statement)
		if err := RunFile(sourceFile(t, source)); err == nil {
			t.Fatal("accepted:", statement)
		}
	}
}

func TestNativeRedisIntegration(t *testing.T) {
	url := os.Getenv("TYPHON_TEST_REDIS_URL")
	if url == "" {
		t.Skip("set TYPHON_TEST_REDIS_URL for a dedicated test Redis")
	}
	testRedisProgram(t, url)
}

func TestNativePostgresIntegration(t *testing.T) {
	url := os.Getenv("TYPHON_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("set TYPHON_TEST_POSTGRES_URL for a dedicated test PostgreSQL")
	}
	// Verify actual writes across committed and rolled-back transactions.
	table := fmt.Sprintf("typhon_driver_tx_%d", time.Now().UnixNano())
	admin, err := pgx.Connect(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = admin.Exec(ctx, "DROP TABLE IF EXISTS "+table)
		_ = admin.Close(ctx)
	})
	writeSource := fmt.Sprintf(`import typhon.postgres as pg
db: pg.Connection = pg.connect(%q, 2)
db.exec('CREATE TABLE %s (id bigint)')
tx: pg.Transaction = db.begin()
tx.exec('INSERT INTO %s VALUES ($1)', [1])
tx.commit()
undo: pg.Transaction = db.begin()
undo.exec('INSERT INTO %s VALUES ($1)', [2])
undo.rollback()
abandoned: pg.Transaction = db.begin()
abandoned.exec('INSERT INTO %s VALUES ($1)', [3])
print(db.query('SELECT id FROM %s ORDER BY id').row(0))
`, url, table, table, table, table, table)
	output, err := captureRun(t, sourceFile(t, writeSource))
	if err != nil || output != "[1]\n" {
		t.Fatalf("transaction writes: %q, %v", output, err)
	}
	var count int
	if err := admin.QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&count); err != nil || count != 1 {
		t.Fatalf("rollback on Engine close: %d, %v", count, err)
	}
	source := fmt.Sprintf(`from typhon.postgres import Connection, Transaction, Rows, connect
db: Connection = connect(%q, 2)
rows: Rows = db.query('SELECT $1::bigint AS id, $2::text AS name, $3::boolean AS active, $4::text AS optional', [9007199254740993, 'Ada', True, None])
print(rows.count, rows.columns, rows.get(0, 'id'), rows.get(0, 'name'), rows.get(0, 'active'), rows.get(0, 'optional'))
values: Rows = db.query("SELECT 12345678901234567890.123::numeric AS exact, '2026-10-10'::date AS day, ARRAY[1,2] AS numbers, '00000000-0000-0000-0000-000000000001'::uuid AS uid")
print(values.get(0, 'exact'), values.get(0, 'day'), values.get(0, 'numbers'), values.get(0, 'uid'))
tx: Transaction = db.begin()
tx.exec('CREATE TEMP TABLE typhon_driver_test (id bigint) ON COMMIT DROP')
print(tx.exec('INSERT INTO typhon_driver_test VALUES ($1)', [42]))
print(tx.query('SELECT id FROM typhon_driver_test').row(0))
tx.rollback()
tx.rollback()
committed: Transaction = db.begin()
committed.exec('SELECT 1')
committed.commit()
db.close()
db.close()
`, url)
	output, err = captureRun(t, sourceFile(t, source))
	want := "1 [id, name, active, optional] 9007199254740993 Ada True None\n12345678901234567890.123 2026-10-10T00:00:00Z [1, 2] 00000000-0000-0000-0000-000000000001\n1\n[42]\n"
	if err != nil || output != want {
		t.Fatalf("got %q (%v), want %q", output, err, want)
	}
	for _, statement := range []string{
		"db.query('SELECT missing_column')",
		"db.query('SELECT 1').get(1, 'missing')",
		"db.query('SELECT 1 AS id').get(0, 'missing')",
		"db.exec('SELECT $1', [[1]])",
		"tx: Transaction = db.begin()\ntx.commit()\ntx.exec('SELECT 1')",
		"db.close()\ndb.query('SELECT 1')",
		"db.query('SELECT pg_sleep(3)')",
	} {
		if err := RunFile(sourceFile(t, fmt.Sprintf("from typhon.postgres import Connection, Transaction, connect\ndb: Connection = connect(%q, 0.1)\n%s\n", url, statement))); err == nil {
			t.Fatal("accepted:", statement)
		}
	}
	// Request errors must release a shared pool's abandoned transaction.
	e := NewEngine()
	defer e.Close()
	m, err := e.LoadModule(sourceFile(t, fmt.Sprintf(`from typhon.postgres import Connection, Transaction, connect
from typhon.http import Request, Response, response
db: Connection = connect(%q, 1)
def handle(req: Request) -> Response:
    tx: Transaction = db.begin()
    tx.exec('SELECT 1')
    bad: int = 1 // 0
    return response(200, 'unreachable')
`, url)))
	if err != nil {
		t.Fatal(err)
	}
	if err = e.ExecuteModule(m, false); err != nil {
		t.Fatal(err)
	}
	handler := httpVMHandler(e, m.Globals["handle"])
	for i := 0; i < 20; i++ {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
		if w.Code != 500 {
			t.Fatalf("request %d returned %d", i, w.Code)
		}
	}
	vm := &VM{engine: e}
	query, err := vm.getAttr(m.Globals["db"], "query")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := vm.callValue(query, []Value{StringValue("SELECT 1")}, nil); err != nil {
		t.Fatal("pool exhausted after handler errors:", err)
	}
}
