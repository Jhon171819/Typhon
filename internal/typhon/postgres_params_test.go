package typhon

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestNativePostgresParameterConstructors(t *testing.T) {
	engine := NewEngine()
	defer engine.Close()
	module, err := engine.LoadModule(sourceFile(t, `import typhon.postgres as pg
numbers: pg.Parameter = pg.array('int4', [1, None, 7])
empty: pg.Parameter = pg.array('text', [])
missing: pg.Parameter = pg.null('text[]')
json_null: pg.Parameter = pg.jsonb(None)
json_number: pg.Parameter = pg.jsonb({'key': [9007199254740993]})
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ExecuteModule(module, false); err != nil {
		t.Fatal(err)
	}
	codecs := pgtype.NewMap()
	encode := func(name string, oid uint32) []byte {
		t.Helper()
		param := module.Globals[name].NativeObject.data.(*postgresParameter)
		encoded, err := codecs.Encode(oid, pgtype.BinaryFormatCode, param.data, nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return encoded
	}
	var integers pgtype.FlatArray[pgtype.Int4]
	if err := codecs.Scan(pgtype.Int4ArrayOID, pgtype.BinaryFormatCode, encode("numbers", pgtype.Int4ArrayOID), &integers); err != nil ||
		len(integers) != 3 || integers[0].Int32 != 1 || integers[1].Valid || integers[2].Int32 != 7 {
		t.Fatalf("nullable integers: %+v %v", integers, err)
	}
	if encode("empty", pgtype.TextArrayOID) == nil || encode("missing", pgtype.TextArrayOID) != nil {
		t.Fatal("empty and null arrays became indistinguishable")
	}
	if !strings.HasSuffix(string(encode("json_null", pgtype.JSONBOID)), "null") ||
		!strings.Contains(string(encode("json_number", pgtype.JSONBOID)), "9007199254740993") {
		t.Fatal("JSONB encoding lost null or integer precision")
	}
	for _, stmt := range []string{
		"pg.array('int4', [2147483648])", "pg.array('text', [[1]])", "pg.array('numeric', [1.5])", "pg.null('unsupported')",
	} {
		path := sourceFile(t, "import typhon.postgres as pg\n"+stmt+"\n")
		if err := RunFile(path); err == nil {
			t.Fatalf("accepted invalid SQL parameter: %s", stmt)
		}
	}
}

func TestNativePostgresSQLStateIsSafe(t *testing.T) {
	err := driverFailure("PostgreSQL", databaseKind, &pgconn.PgError{
		Code: "23505", ConstraintName: "unique_records", TableName: "records", ColumnName: "not\nsafe",
		Message: "private payload", Detail: "another secret", InternalQuery: "SELECT secret",
	})
	var metadata *NativeCause
	var raw *pgconn.PgError
	if !errors.As(err, &metadata) || metadata.Code() != "23505" || metadata.Constraint() != "unique_records" ||
		metadata.Table() != "records" || metadata.Column() != "" || errors.As(err, &raw) ||
		strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("unsafe or missing SQL diagnostic: %v", err)
	}
}

func TestNativePostgresComplexParameterIntegration(t *testing.T) {
	dsn := os.Getenv("TYPHON_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("set TYPHON_TEST_POSTGRES_URL to exercise PostgreSQL JSONB, arrays, and errors")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(driverFailure("PostgreSQL", connectionKind, err))
	}
	schema := "typhon_params_" + strings.ToLower(rand.Text())
	quoted := pgx.Identifier{schema}.Sanitize()
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), cleanupTimeout)
		defer stop()
		if _, err := admin.Exec(cleanup, "DROP SCHEMA IF EXISTS "+quoted+" CASCADE"); err != nil {
			t.Error(driverFailure("PostgreSQL", databaseKind, err))
		}
		if err := admin.Close(cleanup); err != nil {
			t.Error(driverFailure("PostgreSQL", connectionKind, err))
		}
	})
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(driverFailure("PostgreSQL", databaseKind, err))
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(driverFailure("PostgreSQL", validationKind, err))
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	t.Setenv("DATABASE_URL", parsed.String())
	output, err := captureRun(t, sourceFile(t, `import typhon.postgres as pg
import typhon.scalars as s

db: pg.Connection = pg.connect_typed(pg.env('DATABASE_URL'), 3)
db.exec('CREATE TABLE owner (id bigint PRIMARY KEY)')
db.exec('CREATE TABLE record (id uuid PRIMARY KEY, owner bigint CONSTRAINT record_owner_ref REFERENCES owner(id), label text CONSTRAINT record_label_uniq UNIQUE, meta jsonb, numbers int[], names text[], amount numeric, data bytea, registered date, seen timestamptz)')
db.exec('INSERT INTO owner (id) VALUES ($1)', [1])
id: s.UUID = s.uuid('3275b3bb-d54d-4fec-b90c-aa1f7250c778')
label: str = "x' OR 1=1 --"
meta: dict[str, Any] = {'nested': [{'integer': 9007199254740993}], 'flag': True}
amount: s.Decimal = s.decimal('123456789012345.0123456789012345')
data: s.Bytes = s.bytes_hex('610062ff')
day: s.Date = s.date('2025-06-10')
when: s.Timestamp = s.timestamp('2025-06-10T12:00:00.123456Z')
db.exec('INSERT INTO record VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)', [
    id, 1, label, meta, pg.array('int4', [3, None, 5]), pg.array('text', ['one', 'two']), amount, data, day, when
])
rows: pg.Rows = db.query('SELECT meta, numbers, names, amount, data, id, registered, seen FROM record WHERE label = ANY($1) AND meta @> $2', [
    pg.array('text', [label]), pg.jsonb({'flag': True})
])
result: dict[str, Any] = rows.get(0, 'meta')
nested: list[Any] = result['nested']
item: dict[str, Any] = nested[0]
money: s.Decimal = rows.get(0, 'amount')
binary: s.Bytes = rows.get(0, 'data')
record_id: s.UUID = rows.get(0, 'id')
registered: s.Date = rows.get(0, 'registered')
seen: s.Timestamp = rows.get(0, 'seen')
print(rows.count, item['integer'], rows.get(0, 'numbers'), rows.get(0, 'names'))
print(money.text, binary.hex(), record_id.text, registered.text, seen.text)
print(db.query("SELECT ($1::uuid IS NULL) AS sql_null, ($2::jsonb = 'null'::jsonb) AS json_null, ($3::text[] IS NULL) AS array_null", [pg.null('uuid'), pg.jsonb(None), pg.null('text[]')]).row(0))
try:
    db.exec('INSERT INTO record (id, owner, label) VALUES ($1,$2,$3)', [s.uuid('c6da2b89-9aff-432a-8406-2c7bb4c30907'), 1, label])
except DatabaseError as e:
    print(e.sqlstate, e.constraint, e.table, e.column)
try:
    db.exec('INSERT INTO record (id, owner) VALUES ($1,$2)', [s.uuid('c6da2b89-9aff-432a-8406-2c7bb4c30907'), 99])
except DatabaseError as e:
    print(e.sqlstate, e.constraint, e.table, e.column)
tx: pg.Transaction = db.begin()
try:
    tx.exec("SET LOCAL statement_timeout = '30ms'")
    tx.exec('SELECT pg_sleep(1)')
except TimeoutError as e:
    print(e.kind, e.sqlstate)
finally:
    tx.rollback()
print(db.query('SELECT count(*) FROM record').get(0, 'count'))
`))
	if err != nil {
		t.Fatal(err)
	}
	const expected = "1 9007199254740993 [3, None, 5] [one, two]\n" +
		"123456789012345.0123456789012345 610062ff 3275b3bb-d54d-4fec-b90c-aa1f7250c778 2025-06-10 2025-06-10T12:00:00.123456Z\n" +
		"[True, True, True]\n23505 record_label_uniq record None\n23503 record_owner_ref record None\nTimeoutError 57014\n1\n"
	if output != expected {
		t.Fatalf("unexpected PostgreSQL round-trip or SQLSTATE results: %q", output)
	}
	testRealPostgresDeadlock(t, ctx, parsed.String())
}

func testRealPostgresDeadlock(t *testing.T, ctx context.Context, dsn string) {
	t.Helper()
	conns := make([]*pgx.Conn, 2)
	for i := range conns {
		conn, err := pgx.Connect(ctx, dsn)
		if err != nil {
			t.Fatal(driverFailure("PostgreSQL", connectionKind, err))
		}
		defer conn.Close(context.Background())
		conns[i] = conn
	}
	for _, sql := range []string{"CREATE TABLE locks (id integer PRIMARY KEY)", "INSERT INTO locks VALUES (1),(2)"} {
		if _, err := conns[0].Exec(ctx, sql); err != nil {
			t.Fatal(driverFailure("PostgreSQL", databaseKind, err))
		}
	}
	txs := make([]pgx.Tx, 2)
	for i, conn := range conns {
		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatal(driverFailure("PostgreSQL", databaseKind, err))
		}
		txs[i] = tx
		defer tx.Rollback(context.Background())
		if _, err := tx.Exec(ctx, "SELECT id FROM locks WHERE id = $1 FOR UPDATE", i+1); err != nil {
			t.Fatal(driverFailure("PostgreSQL", databaseKind, err))
		}
	}
	results := make(chan error, 2)
	for i, tx := range txs {
		go func() {
			_, err := tx.Exec(ctx, "SELECT id FROM locks WHERE id = $1 FOR UPDATE", 2-i)
			results <- driverFailure("PostgreSQL", databaseKind, err)
		}()
	}
	deadlocks := 0
	for range txs {
		if err := <-results; err != nil {
			var cause *NativeCause
			var ex *Exception
			if !errors.As(err, &cause) || !errors.As(err, &ex) || cause.Code() != "40P01" || ex.Kind() != databaseKind {
				t.Error(fmt.Errorf("expected a typed deadlock: %w", err))
			} else {
				deadlocks++
			}
		}
	}
	if deadlocks != 1 {
		t.Fatalf("expected one deadlock, got %d", deadlocks)
	}
}
