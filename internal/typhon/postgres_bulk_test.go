package typhon

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestNativePostgresBulkRejectsUnsafeShape(t *testing.T) {
	engine := NewEngine()
	defer engine.Close()
	vm := &VM{engine: engine}
	list := func(values ...string) Value {
		elems := make([]Value, len(values))
		for i, value := range values {
			elems[i] = StringValue(value)
		}
		return ListValue(elems)
	}
	rows := ListValue([]Value{ListValue([]Value{IntValue(1)})})
	for _, tc := range []struct {
		method string
		args   []Value
	}{
		{"copy_from", []Value{list("target; DROP TABLE target"), list("id"), rows}},
		{"copy_from", []Value{list("target"), list("id", "id"), rows}},
		{"copy_from", []Value{list("target"), list("id", "text"), rows}},
		{"exec_many", []Value{StringValue("INSERT INTO target VALUES ($1); COMMIT"), rows}},
		{"exec_many", []Value{StringValue("BEGIN"), rows}},
		{"exec_many", []Value{StringValue("INSERT INTO target VALUES ($1)"), ListValue(make([]Value, maxPostgresBulkRows+1))}},
	} {
		if _, err := makePostgresBulk(vm, tc.method, tc.args); err == nil {
			t.Fatalf("accepted an unsafe %s input", tc.method)
		}
	}
	value, err := makePostgresBulk(vm, "copy_from", []Value{list("app", "records"), list("id"), rows})
	if err != nil || value.table.Sanitize() != `"app"."records"` || value.rows[0][0] != int64(1) {
		t.Fatalf("safe identifier or typed value changed: %+v %v", value, err)
	}
}

func TestNativePostgresBulkIntegration(t *testing.T) {
	dsn := os.Getenv("TYPHON_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("set TYPHON_TEST_POSTGRES_URL for PostgreSQL COPY and exec_many")
	}
	source := fmt.Sprintf(`import typhon.postgres as pg
import typhon.scalars as s
opts: dict[str, Any] = {'max_connections': 1, 'query_seconds': 5, 'typed': True}
db: pg.Connection = pg.connect_pooled(%q, opts)
db.exec('CREATE TEMP TABLE incoming (id bigint PRIMARY KEY, external uuid, payload jsonb, label text)')
id: s.UUID = s.uuid('0cdfd2db-24f4-4a60-9f98-d44729d7bb12')
document: pg.Parameter = pg.jsonb({'complex': [9007199254740993, None]})
columns: list[str] = ['id', 'external', 'payload', 'label']
rows: list[list[Any]] = []
index: int = 1
while index <= 2500:
    item: list[Any] = [index, id, document, "São Paulo — literal '); --"]
    rows.append(item)
    index = index + 1
print(db.copy_from(['incoming'], columns, rows))
data: pg.Rows = db.query('SELECT external, payload, label FROM incoming WHERE id=$1', [1])
saved_uuid: s.UUID = data.get(0, 'external')
payload: dict[str, Any] = data.get(0, 'payload')
print(saved_uuid.text, payload['complex'], data.get(0, 'label'))

try:
    db.copy_from(['incoming'], columns, [[3001, id, document, 'ok'], [1, id, document, 'duplicate']])
except DatabaseError as err:
    print(err.sqlstate)

try:
    db.copy_from(['incoming'], columns, [[3002, id, document, 'ok'], [3003, 'invalid uuid', document, 'bad']])
except DatabaseError:
    print('copy type error')

try:
    db.exec_many('INSERT INTO incoming (id,label) VALUES ($1,$2)', [[4001, 'before'], [1, 'duplicate']])
except DatabaseError as err:
    print(err.sqlstate)

tx: pg.Transaction = db.begin()
tx.exec('INSERT INTO incoming (id) VALUES ($1)', [5001])
try:
    tx.copy_from(['incoming'], columns, [[1, id, document, 'duplicate']])
except DatabaseError:
    print('transaction rolled back')
try:
    tx.commit()
except ValidationError:
    print('already closed')

print(db.exec_many('UPDATE incoming SET label=$2 WHERE id=$1', [[1, 'first'], [2, 'second']]))
print(db.query('SELECT count(*) AS total FROM incoming').get(0, 'total'), db.stats()['in_use'])
`, dsn)
	got, err := captureRun(t, sourceFile(t, source))
	const want = "2500\n0cdfd2db-24f4-4a60-9f98-d44729d7bb12 [9007199254740993, None] São Paulo — literal '); --\n" +
		"23505\ncopy type error\n23505\ntransaction rolled back\nalready closed\n2\n2500 0\n"
	if err != nil || got != want {
		t.Fatalf("COPY/BATCH roundtrip or atomicity failed: %q %v", got, err)
	}
}

func BenchmarkNativePostgresBulk(b *testing.B) {
	dsn := os.Getenv("TYPHON_TEST_POSTGRES_URL")
	if dsn == "" {
		b.Skip("set TYPHON_TEST_POSTGRES_URL for the PostgreSQL ingestion benchmark")
	}
	const rowCount = 1000
	const template = "INSERT INTO ingest (id, label) VALUES ($1,$2)"
	input := make([][]any, rowCount)
	for i := range input {
		input[i] = []any{int64(i), "sample São Paulo"}
	}
	for _, name := range []string{"INSERT", "batch", "COPY"} {
		b.Run(name, func(b *testing.B) {
			ctx, cancel := context.WithTimeout(b.Context(), 2*time.Minute)
			defer cancel()
			conn, err := pgx.Connect(ctx, dsn)
			if err != nil {
				b.Fatal(driverFailure("PostgreSQL", connectionKind, err))
			}
			defer conn.Close(context.Background())
			if _, err := conn.Exec(ctx, "CREATE TEMP TABLE ingest (id bigint PRIMARY KEY, label text)"); err != nil {
				b.Fatal(driverFailure("PostgreSQL", databaseKind, err))
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				b.StopTimer()
				if _, err := conn.Exec(ctx, "TRUNCATE ingest"); err != nil {
					b.Fatal(driverFailure("PostgreSQL", databaseKind, err))
				}
				b.StartTimer()
				var n int64
				switch name {
				case "INSERT":
					n, err = serialPostgresInserts(ctx, conn, template, input)
				case "batch":
					n, err = (postgresBulk{sql: template, rows: input}).execute(ctx, conn)
				case "COPY":
					n, err = (postgresBulk{table: pgx.Identifier{"ingest"}, columns: []string{"id", "label"}, rows: input}).execute(ctx, conn)
				}
				if err != nil || n != rowCount {
					b.Fatalf("inserted %d rows: %v", n, driverFailure("PostgreSQL", databaseKind, err))
				}
			}
			b.ReportMetric(float64(b.N*rowCount)/b.Elapsed().Seconds(), "rows/s")
		})
	}
}

func serialPostgresInserts(ctx context.Context, conn *pgx.Conn, sql string, input [][]any) (int64, error) {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	var count int64
	for _, args := range input {
		tag, err := tx.Exec(ctx, sql, args...)
		if err != nil {
			return 0, err
		}
		count += tag.RowsAffected()
	}
	return count, tx.Commit(ctx)
}
