package typhon

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestNativePostgresCursorLimits(t *testing.T) {
	for _, user := range []int64{0, -1, 1025} {
		if _, err := postgresFetchSize([]Value{StringValue("SELECT 1"), ListValue(nil), IntValue(user)}); err == nil {
			t.Fatalf("invalid fetch size %d accepted", user)
		}
	}
	t.Setenv("TYPHON_LIMIT_SQL_ROWS", "1")
	t.Setenv("TYPHON_LIMIT_SQL_CURSOR_ROWS", "2")
	engine := NewEngine()
	defer engine.Close()
	if engine.quotaErr != nil {
		t.Fatal(engine.quotaErr)
	}
	vm := &VM{engine: engine}
	if err := vm.chargeSQLCursorRow(); err != nil {
		t.Fatal(err)
	}
	if err := vm.chargeSQLRow(); err == nil {
		t.Fatal("raising the cursor limit also raised the materialized SQL limit")
	}
	if err := vm.chargeSQLCursorRow(); err != nil {
		t.Fatal(err)
	}
	if err := vm.chargeSQLCursorRow(); err == nil {
		t.Fatal("cursor row quota was ignored")
	}
}

func TestNativePostgresCursorLargeDatasetIntegration(t *testing.T) {
	dsn := os.Getenv("TYPHON_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("set TYPHON_TEST_POSTGRES_URL to test a bounded 100,000-row stream")
	}
	engine := NewEngine()
	defer engine.Close()
	module, err := engine.LoadModule(sourceFile(t, fmt.Sprintf(`import typhon.postgres as pg
opts: dict[str, Any] = {'max_connections': 1, 'query_seconds': 25}
db: pg.Connection = pg.connect_pooled(%q, opts)
report: pg.Cursor = db.cursor('SELECT value AS id FROM generate_series(1,100000) AS value', [], 128)
`, dsn)))
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ExecuteModule(module, false); err != nil {
		t.Fatal(err)
	}
	cursor := module.Globals["report"].NativeObject.data.(*postgresCursor)
	vm := &VM{engine: engine}
	runtime.GC()
	var baseline runtime.MemStats
	runtime.ReadMemStats(&baseline)
	peak := baseline.HeapAlloc
	total, nextSample := 0, 5000
	for {
		batch, err := cursor.take(vm, 128)
		if err != nil {
			t.Fatal(err)
		}
		if len(batch) == 0 {
			break
		}
		if len(batch) > 128 {
			t.Fatal("cursor exceeded its configured batch size")
		}
		for _, returned := range batch {
			total++
			if got := (*returned.List)[0].Int; got != int64(total) {
				t.Fatalf("missing, duplicate or reordered record at %d (%d)", total, got)
			}
		}
		if total >= nextSample {
			runtime.GC()
			var current runtime.MemStats
			runtime.ReadMemStats(&current)
			peak = max(peak, current.HeapAlloc)
			nextSample += 5000
		}
	}
	delta := peak - baseline.HeapAlloc
	db := module.Globals["db"].NativeObject.data.(*postgresConnection)
	if total != 100000 || delta > 16<<20 || db.pool.Stat().AcquiredConns() != 0 {
		t.Fatalf("cursor rows=%d peak live heap increment=%d leased=%d", total, delta, db.pool.Stat().AcquiredConns())
	}
	t.Logf("streamed %d rows in batches of 128 with %d incremental sampled live heap bytes", total, delta)
}

func TestNativePostgresCursorBatchCloseAndErrorsIntegration(t *testing.T) {
	dsn := os.Getenv("TYPHON_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("set TYPHON_TEST_POSTGRES_URL for cursor lifecycle tests")
	}
	source := fmt.Sprintf(`import typhon.postgres as pg
config: dict[str, Any] = {'max_connections': 1, 'query_seconds': 5}
db: pg.Connection = pg.connect_pooled(%q, config)
data: pg.Cursor = db.cursor("SELECT id, repeat('b',1048576) AS payload FROM generate_series(1, 5) AS id", [], 128)
first: list[list[Any]] = data.batch()
second: list[list[Any]] = data.batch()
print(data.columns, len(first), first[0][0], len(second), second[1][0])
print(data.next(), db.stats()['in_use'])

slow: pg.Cursor = db.cursor("SELECT id, CASE WHEN id = 10000 THEN pg_sleep(5)::text ELSE repeat('a', 32) END FROM generate_series(1,10000) AS id")
head: list[Any] | None = slow.next()
if head is not None:
    print(head[0])
slow.close()
slow.close()
try:
    slow.next()
except ValidationError:
    print('closed')

db.exec('CREATE TEMP TABLE samples (id int)')
tx: pg.Transaction = db.begin()
tx.exec('INSERT INTO samples VALUES ($1)', [7])
seen: pg.Cursor = tx.cursor('SELECT id FROM samples', [], 1)
print(seen.next())
try:
    tx.exec('SELECT 1')
except ValidationError:
    print('in use')
seen.close()
try:
    tx.commit()
except ValidationError:
    print('must roll back')
tx.rollback()

complete: pg.Transaction = db.begin()
consumed: pg.Cursor = complete.cursor('SELECT 2', [], 1)
print(consumed.next(), consumed.next())
complete.commit()

try:
    broken: pg.Cursor = db.cursor('SELECT 1 / (id - 4000) FROM generate_series(1, 4096) AS id')
    while len(broken.batch()) > 0:
        continue
except DatabaseError as e:
    print(e.sqlstate)
print(db.query('SELECT 1 AS alive').get(0, 'alive'), db.stats()['in_use'])
`, dsn)
	began := time.Now()
	got, err := captureRun(t, sourceFile(t, source))
	const want = "[id, payload] 3 1 2 5\nNone 0\n1\nclosed\n[7]\nin use\nmust roll back\n[2] None\n22012\n1 0\n"
	if err != nil || got != want || time.Since(began) >= 5*time.Second {
		t.Fatalf("cursor streamed or closed incorrectly: %q %v in %s", got, err, time.Since(began))
	}
}

func TestNativePostgresCursorDownloadCancellationIntegration(t *testing.T) {
	dsn := os.Getenv("TYPHON_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("set TYPHON_TEST_POSTGRES_URL to test a cancelled native cursor download")
	}
	engine := NewEngine()
	defer engine.Close()
	module, err := engine.LoadModule(sourceFile(t, fmt.Sprintf(`import json
import typhon.http as http
import typhon.postgres as pg
import typhon.scalars as s
config: dict[str, Any] = {'max_connections': 1, 'query_seconds': 5}
db: pg.Connection = pg.connect_pooled(%q, config)
def produce(state: Any) -> s.Bytes | None:
    reader: pg.Cursor = state
    item: list[Any] | None = reader.next()
    if item is None:
        return None
    return s.bytes_utf8(json.dumps(item) + '\n')
def download(req: http.Request) -> http.Response:
    tx: pg.Transaction = db.begin()
    cur: pg.Cursor = tx.cursor("SELECT id, CASE WHEN id = 10000 THEN pg_sleep(5)::text ELSE repeat('z', 32) END FROM generate_series(1,10000) AS id")
    return http.stream(200, produce, cur, [['Content-Type','application/x-ndjson']])
`, dsn)))
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ExecuteModule(module, false); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(httpVMHandler(engine, module.Globals["download"]))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	header := make([]byte, 5)
	_, err = io.ReadFull(resp.Body, header)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != 200 || !strings.HasPrefix(string(header), "[1,") {
		t.Fatalf("missing first row before PostgreSQL finished producing the result: %q %v", header, err)
	}
	db := module.Globals["db"].NativeObject.data.(*postgresConnection)
	var marker int
	if err := db.pool.QueryRow(ctx, "SELECT 1").Scan(&marker); err != nil || marker != 1 {
		t.Fatalf("disconnect did not release the transaction cursor: %v", driverFailure("PostgreSQL", databaseKind, err))
	}
}
