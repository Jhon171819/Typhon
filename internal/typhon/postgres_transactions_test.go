package typhon

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestNativePostgresTransactionRetryClassification(t *testing.T) {
	for _, code := range []string{"40001", "40P01", "23505", "57014", "08006"} {
		err := postgresCommitError(&pgconn.PgError{Code: code})
		want := code == "40001" || code == "40P01"
		if postgresRetryable(err) != want {
			t.Fatalf("retry classification changed for %s: %v", code, err)
		}
	}
	ambiguous := postgresCommitError(context.DeadlineExceeded)
	if postgresRetryable(ambiguous) || !strings.Contains(ambiguous.Error(), "outcome is unknown") ||
		!errors.Is(ambiguous, context.DeadlineExceeded) {
		t.Fatalf("ambiguous commit was not preserved: %v", ambiguous)
	}
	if postgresRetryable(&Exception{kind: databaseKind, message: "40001"}) || postgresRetryable(pgx.ErrTxCommitRollback) {
		t.Fatal("transaction replay requires a PostgreSQL SQLSTATE")
	}
}

func TestNativePostgresScopedTransactionIntegration(t *testing.T) {
	dsn := os.Getenv("TYPHON_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("set TYPHON_TEST_POSTGRES_URL for scoped transaction tests")
	}
	source := fmt.Sprintf(`import typhon.postgres as pg
cfg: dict[str, Any] = {'max_connections': 1, 'acquire_seconds': 1, 'query_seconds': 2}
db: pg.Connection = pg.connect_pooled(%q, cfg)
db.exec('CREATE TEMP TABLE units (id int PRIMARY KEY)')
db.exec('CREATE TEMP TABLE child (id int, CONSTRAINT fk_deferred FOREIGN KEY (id) REFERENCES units(id) DEFERRABLE INITIALLY DEFERRED)')

def partial(tx: pg.Transaction) -> Any:
    tx.exec('INSERT INTO units VALUES ($1)', [1])
    tx.exec('INSERT INTO units VALUES ($1)', [1])
    tx.commit()
    return 'unreachable'

def early(tx: pg.Transaction) -> Any:
    tx.exec('INSERT INTO units VALUES ($1)', [2])
    return 'early'

def commit(tx: pg.Transaction) -> Any:
    tx.exec('INSERT INTO units VALUES ($1)', [3])
    tx.commit()
    return 'committed'

def deferred_violation(tx: pg.Transaction) -> Any:
    tx.exec('INSERT INTO child VALUES ($1)', [99])
    tx.commit()
    return 'unreachable'

def change(tx: pg.Transaction) -> Any:
    tx.exec('CREATE TEMP TABLE denied (id int)')
    tx.commit()
    return None

attempts: list[int] = [0]
def retry_unit(tx: pg.Transaction) -> Any:
    attempts[0] = attempts[0] + 1
    tx.exec('INSERT INTO units VALUES ($1)', [5])
    if attempts[0] < 2:
        tx.exec("DO $$ BEGIN RAISE SQLSTATE '40001'; END $$")
    tx.commit()
    return 'retried'

after_commit: list[int] = [0]
def already_committed(tx: pg.Transaction) -> Any:
    after_commit[0] = after_commit[0] + 1
    tx.exec('INSERT INTO units VALUES ($1)', [6])
    tx.commit()
    db.exec("DO $$ BEGIN RAISE SQLSTATE '40P01'; END $$")
    return None

try:
    db.transaction(partial)
except DatabaseError as err:
    print(err.sqlstate)
print(db.transaction(early), db.transaction(commit))
try:
    db.transaction(deferred_violation)
except DatabaseError as err:
    print(err.sqlstate, err.constraint)
try:
    db.transaction(change, 'repeatable_read', True)
except DatabaseError as err:
    print(err.sqlstate)
try:
    db.transaction(retry_unit)
except DatabaseError as err:
    print(err.sqlstate, attempts[0])
attempts[0] = 0
print(db.transaction(retry_unit, 'serializable', False, 2), attempts[0])
try:
    db.transaction(already_committed, 'read_committed', False, 2)
except DatabaseError as err:
    print(err.sqlstate, after_commit[0])
print(db.query('SELECT array_agg(id ORDER BY id) AS ids FROM units').get(0, 'ids'))
print(db.query('SELECT count(*) AS total FROM child').get(0, 'total'), db.stats()['in_use'])
`, dsn)
	got, err := captureRun(t, sourceFile(t, source))
	const want = "23505\nearly committed\n23503 fk_deferred\n25006\n40001 1\nretried 2\n40P01 1\n[3, 5, 6]\n0 0\n"
	if err != nil || got != want {
		t.Fatalf("incorrect commit, rollback or retry behavior: %q %v", got, err)
	}
}

func TestNativePostgresScopedTransactionCancellationIntegration(t *testing.T) {
	dsn := os.Getenv("TYPHON_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("set TYPHON_TEST_POSTGRES_URL for request cancellation rollback")
	}
	ctx, stop := context.WithTimeout(t.Context(), 8*time.Second)
	defer stop()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(driverFailure("PostgreSQL", connectionKind, err))
	}
	tableName := "typhon_scoped_" + fmt.Sprint(time.Now().UnixNano())
	table := pgx.Identifier{tableName}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE TABLE "+table+" (id int)"); err != nil {
		t.Fatal(driverFailure("PostgreSQL", databaseKind, err))
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		if _, err := admin.Exec(cleanup, "DROP TABLE IF EXISTS "+table); err != nil {
			t.Error(driverFailure("PostgreSQL", databaseKind, err))
		}
		if err := admin.Close(cleanup); err != nil {
			t.Error(driverFailure("PostgreSQL", connectionKind, err))
		}
	}()
	engine := NewEngine()
	defer engine.Close()
	module, err := engine.LoadModule(sourceFile(t, fmt.Sprintf(`import typhon.http as http
import typhon.postgres as pg
options: dict[str, Any] = {'max_connections': 1, 'query_seconds': 4}
db: pg.Connection = pg.connect_pooled(%q, options)
def work(tx: pg.Transaction) -> Any:
    tx.exec(%q, [1])
    tx.exec(%q)
    tx.commit()
    return None
def handle(req: http.Request) -> http.Response:
    db.transaction(work)
    return http.response(200, 'complete')
`, dsn, "INSERT INTO "+table+" (id) VALUES ($1)", "SELECT pg_sleep(3) /* "+tableName+" */")))
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ExecuteModule(module, false); err != nil {
		t.Fatal(err)
	}
	handler := httpVMHandler(engine, module.Globals["handle"])
	client, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil).WithContext(client))
		done <- rec.Code
	}()
	for {
		var active bool
		if err := admin.QueryRow(ctx,
			"SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE pid <> pg_backend_pid() AND state='active' AND query LIKE $1)",
			"%pg_sleep%"+tableName+"%").Scan(&active); err != nil {
			t.Fatal(driverFailure("PostgreSQL", databaseKind, err))
		}
		if active {
			break
		}
		select {
		case status := <-done:
			t.Fatalf("transaction completed before the test cancelled it: %d", status)
		case <-ctx.Done():
			t.Fatal("scoped transaction did not reach its blocking query")
		case <-time.After(20 * time.Millisecond):
		}
	}
	cancel()
	select {
	case code := <-done:
		if code != statusClientClosedRequest {
			t.Fatalf("cancel returned %d", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("request cancellation did not return the leased connection")
	}
	db := module.Globals["db"].NativeObject.data.(*postgresConnection)
	var rows int
	if err := db.pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("cancelled transaction persisted data or held the pool: %d %v", rows, driverFailure("PostgreSQL", databaseKind, err))
	}
}
