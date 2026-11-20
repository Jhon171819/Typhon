package typhon

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func writeMigration(t *testing.T, directory, filename, sql string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(directory, filename), []byte(sql), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestNativeMigrationFilesAndSeedGuard(t *testing.T) {
	directory := t.TempDir()
	writeMigration(t, directory, "000001_create.up.sql", "/* safe /* nested */ comment */\nCREATE TABLE example(id int);\n")
	first, err := loadMigrationFiles(directory)
	if err != nil || len(first) != 1 {
		t.Fatalf("valid migration not loaded: %v", err)
	}
	writeMigration(t, directory, "000001_create.up.sql", "\ufeff/* safe /* nested */ comment */\r\nCREATE TABLE example(id int);\r\n")
	second, err := loadMigrationFiles(directory)
	if err != nil || first[0].up.checksum != second[0].up.checksum {
		t.Fatalf("checksum is not portable across normal file line endings: %v", err)
	}
	writeMigration(t, directory, "000003_skipped.up.sql", "ALTER TABLE example ADD COLUMN value text")
	if _, err := loadMigrationFiles(directory); err == nil || !strings.Contains(err.Error(), "000002") {
		t.Fatalf("migration gap accepted: %v", err)
	}

	for _, sql := range []string{
		"BEGIN", "/* hidden */ COMMIT", "CREATE INDEX CONCURRENTLY idx ON example(id)",
		"-- typhon:no-transaction\nDROP TABLE example",
	} {
		dir := t.TempDir()
		writeMigration(t, dir, "000001_invalid.up.sql", sql)
		if _, err := loadMigrationFiles(dir); err == nil {
			t.Fatalf("accepted unsafe migration mode: %s", sql)
		}
	}
	t.Setenv("TYPHON_ENV", "production")
	if err := RunMigrations(t.Context(), "", directory, "seed", io.Discard); err == nil || !strings.Contains(err.Error(), "seeds require") {
		t.Fatalf("seeds did not enforce an explicit development/test environment: %v", err)
	}
}

func nativeMigrationDB(t *testing.T) (context.Context, string, *pgx.Conn, string) {
	t.Helper()
	dsn := os.Getenv("TYPHON_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("set TYPHON_TEST_POSTGRES_URL to test PostgreSQL migrations")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	t.Cleanup(cancel)
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(driverFailure("PostgreSQL", connectionKind, err))
	}
	name := "typhon_migrations_" + strings.ToLower(rand.Text())
	schema := pgx.Identifier{name}.Sanitize()
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), cleanupTimeout)
		defer stop()
		if _, err := admin.Exec(cleanup, "DROP SCHEMA IF EXISTS "+schema+" CASCADE"); err != nil {
			t.Error(driverFailure("PostgreSQL", databaseKind, err))
		}
		if err := admin.Close(cleanup); err != nil {
			t.Error(driverFailure("PostgreSQL", connectionKind, err))
		}
	})
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(driverFailure("PostgreSQL", databaseKind, err))
	}
	target, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(driverFailure("PostgreSQL", validationKind, err))
	}
	params := target.Query()
	params.Set("search_path", name+",public")
	target.RawQuery = params.Encode()
	return ctx, target.String(), admin, schema
}

func TestNativeMigrationsLockChecksumsAndAtomicHistoryIntegration(t *testing.T) {
	ctx, dsn, admin, schema := nativeMigrationDB(t)
	directory := t.TempDir()
	const name2 = "000002_title.up.sql"
	const alter = "DO $$ BEGIN PERFORM pg_sleep(0.1); ALTER TABLE widgets ADD COLUMN title text; END $$"
	writeMigration(t, directory, "000001_widgets.up.sql", "CREATE TABLE widgets (id int PRIMARY KEY)")
	writeMigration(t, directory, "000001_widgets.down.sql", "DROP TABLE widgets")
	writeMigration(t, directory, name2, alter)
	writeMigration(t, directory, "000002_title.down.sql", "ALTER TABLE widgets DROP COLUMN title")
	var plan bytes.Buffer
	if err := RunMigrations(ctx, dsn, directory, "plan", &plan); err != nil ||
		strings.Count(plan.String(), " pending ") != 2 {
		t.Fatalf("migration plan is incomplete: %q %v", plan.String(), err)
	}
	var exists bool
	if err := admin.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", schema+".typhon_schema_migrations").Scan(&exists); err != nil || exists {
		t.Fatalf("plan unexpectedly mutated the schema: %v", driverFailure("PostgreSQL", databaseKind, err))
	}
	start := make(chan struct{})
	errs := make(chan error, 2)
	var outputs [2]bytes.Buffer
	var workers sync.WaitGroup
	for i := range outputs {
		workers.Go(func() {
			<-start
			errs <- RunMigrations(ctx, dsn, directory, "up", &outputs[i])
		})
	}
	close(start)
	workers.Wait()
	for range outputs {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if strings.Count(outputs[0].String()+outputs[1].String(), " applied ") != 2 ||
		outputs[0].Len() != 0 && outputs[1].Len() != 0 {
		t.Fatalf("concurrent migrators did not apply once: %q %q", outputs[0].String(), outputs[1].String())
	}
	var status bytes.Buffer
	if err := RunMigrations(ctx, dsn, directory, "status", &status); err != nil ||
		strings.Count(status.String(), " applied ") != 2 {
		t.Fatalf("failed to audit applied migrations: %q %v", status.String(), err)
	}
	writeMigration(t, directory, name2, alter+" -- changed once applied")
	if err := RunMigrations(ctx, dsn, directory, "up", io.Discard); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("edited applied migration was accepted: %v", err)
	}
	writeMigration(t, directory, name2, alter)
	if err := RunMigrations(ctx, dsn, directory, "down", io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := RunMigrations(ctx, dsn, directory, "up", io.Discard); err != nil {
		t.Fatal(err)
	}
	seeds := t.TempDir()
	writeMigration(t, seeds, "000001_sample.up.sql", "INSERT INTO widgets (id,title) VALUES (1, 'fixture') ON CONFLICT DO NOTHING")
	t.Setenv("TYPHON_ENV", "test")
	for range 2 {
		if err := RunMigrations(ctx, dsn, seeds, "seed", io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	const third = "000003_fails.up.sql"
	writeMigration(t, directory, third, "DO $$ BEGIN INSERT INTO widgets (id) VALUES (2); RAISE EXCEPTION 'fixture failure'; END $$")
	if err := RunMigrations(ctx, dsn, directory, "up", io.Discard); err == nil {
		t.Fatal("failed migration reported success")
	}
	var records, applied int
	if err := admin.QueryRow(ctx, "SELECT count(*) FROM "+schema+".widgets").Scan(&records); err != nil {
		t.Fatal(driverFailure("PostgreSQL", databaseKind, err))
	}
	if err := admin.QueryRow(ctx, "SELECT count(*) FROM "+schema+".typhon_schema_migrations").Scan(&applied); err != nil {
		t.Fatal(driverFailure("PostgreSQL", databaseKind, err))
	}
	if records != 1 || applied != 2 {
		t.Fatalf("failed migration kept partial data or its history: records=%d applied=%d", records, applied)
	}
	writeMigration(t, directory, third, "CREATE TABLE escape (id int); COMMIT")
	if err := RunMigrations(ctx, dsn, directory, "up", io.Discard); err == nil {
		t.Fatal("multi-statement migration escaped its transaction")
	}
	if err := admin.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", schema+".escape").Scan(&exists); err != nil || exists {
		t.Fatalf("multi-statement reset left a table: %v", driverFailure("PostgreSQL", databaseKind, err))
	}
}

func TestNativeMigrationsDirtyNonTransactionalIntegration(t *testing.T) {
	ctx, dsn, _, _ := nativeMigrationDB(t)
	directory := t.TempDir()
	writeMigration(t, directory, "000001_index.up.sql", "-- typhon:no-transaction\nCREATE INDEX CONCURRENTLY idx ON missing_table(id)")
	err := RunMigrations(ctx, dsn, directory, "up", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "incomplete non-transactional operation") {
		t.Fatalf("failed non-transactional SQL was not marked incomplete: %v", err)
	}
	err = RunMigrations(ctx, dsn, directory, "status", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "inspect PostgreSQL before continuing") {
		t.Fatalf("migration tool silently ignored the incomplete operation: %v", err)
	}
}
