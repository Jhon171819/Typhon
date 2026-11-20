package typhon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const migrationLockID int64 = 6077984678708038961

const migrationHistoryColumns = ` (
  version bigint PRIMARY KEY,
  name text NOT NULL,
  up_checksum text NOT NULL,
  down_checksum text NOT NULL,
  state text NOT NULL CHECK (state IN ('applying', 'applied', 'reverting')),
  applied_at timestamptz NOT NULL DEFAULT clock_timestamp()
)`

type migrationHistory struct {
	version int64
	name    string
	upSum   string
	downSum string
	state   string
}

type migrationExecutor interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

// RunMigrations runs one explicit migration or seed command against PostgreSQL.
func RunMigrations(ctx context.Context, dsn, directory, action string, output io.Writer) (runErr error) {
	switch action {
	case "status", "plan", "up", "down":
	case "seed":
		if stage := os.Getenv("TYPHON_ENV"); stage != "development" && stage != "test" {
			return errors.New("seeds require TYPHON_ENV=development or TYPHON_ENV=test")
		}
	default:
		return errors.New("migration command must be status, plan, up, down or seed")
	}
	if dsn == "" {
		return errors.New("DATABASE_URL must be set for migrations")
	}
	versions, err := loadMigrationFiles(directory)
	if err != nil {
		return err
	}
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		return driverFailure("PostgreSQL", validationKind, err)
	}
	config.ConnectTimeout = 5 * time.Second
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return driverFailure("PostgreSQL", connectionKind, err)
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer stop()
		runErr = finalizeRun(runErr, driverFailure("PostgreSQL", connectionKind, conn.Close(cleanup)))
	}()
	lock, stop := context.WithTimeout(ctx, 30*time.Second)
	_, err = conn.Exec(lock, "SELECT pg_advisory_lock($1)", migrationLockID)
	stop()
	if err != nil {
		return fmt.Errorf("acquire database migration lock: %w", driverFailure("PostgreSQL", databaseKind, err))
	}
	if action == "seed" {
		return runDevelopmentSeeds(ctx, conn, versions, output)
	}
	var schema string
	if err := conn.QueryRow(ctx, "SELECT current_schema()").Scan(&schema); err != nil {
		return driverFailure("PostgreSQL", databaseKind, err)
	}
	table := pgx.Identifier{schema, "typhon_schema_migrations"}.Sanitize()
	history, err := readMigrationHistory(ctx, conn, table)
	if err != nil {
		return err
	}
	if err := validateMigrationHistory(versions, history); err != nil {
		return err
	}
	applied := len(history)
	switch action {
	case "status", "plan":
		start := 0
		if action == "plan" {
			start = applied
		}
		for i, version := range versions[start:] {
			state := "pending"
			if start+i < applied {
				state = "applied"
			}
			if _, err := fmt.Fprintf(output, "%06d %s %s %s\n", version.version, version.name, state, version.up.checksum); err != nil {
				return err
			}
		}
	case "down":
		if applied == 0 {
			return nil
		}
		return applyMigration(ctx, conn, table, versions[applied-1], true, output)
	case "up":
		if applied == len(versions) {
			return nil
		}
		if _, err := conn.Exec(ctx, "CREATE TABLE IF NOT EXISTS "+table+migrationHistoryColumns); err != nil {
			return driverFailure("PostgreSQL", databaseKind, err)
		}
		for _, version := range versions[applied:] {
			if err := applyMigration(ctx, conn, table, version, false, output); err != nil {
				return err
			}
		}
	}
	return nil
}

func readMigrationHistory(ctx context.Context, conn *pgx.Conn, table string) ([]migrationHistory, error) {
	var exists bool
	if err := conn.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", table).Scan(&exists); err != nil {
		return nil, driverFailure("PostgreSQL", databaseKind, err)
	}
	if !exists {
		return nil, nil
	}
	rows, err := conn.Query(ctx, "SELECT version,name,up_checksum,down_checksum,state FROM "+table+" ORDER BY version")
	if err != nil {
		return nil, driverFailure("PostgreSQL", databaseKind, err)
	}
	defer rows.Close()
	var history []migrationHistory
	for rows.Next() {
		var record migrationHistory
		if err := rows.Scan(&record.version, &record.name, &record.upSum, &record.downSum, &record.state); err != nil {
			return nil, driverFailure("PostgreSQL", databaseKind, err)
		}
		if len(history) >= 4096 {
			return nil, errors.New("migration history exceeds 4096 versions")
		}
		history = append(history, record)
	}
	return history, driverFailure("PostgreSQL", databaseKind, rows.Err())
}

func migrationDownChecksum(version migrationVersion) string {
	if version.down == nil {
		return ""
	}
	return version.down.checksum
}

func validateMigrationHistory(versions []migrationVersion, history []migrationHistory) error {
	for i, record := range history {
		if record.state != "applied" {
			return fmt.Errorf("migration %06d has an incomplete non-transactional operation; inspect PostgreSQL before continuing", record.version)
		}
		if i >= len(versions) || record.version != versions[i].version || record.name != versions[i].name {
			return fmt.Errorf("migration %06d is missing or out of order", record.version)
		}
		if record.upSum != versions[i].up.checksum || record.downSum != migrationDownChecksum(versions[i]) {
			return fmt.Errorf("checksum mismatch for applied migration %06d %s", record.version, record.name)
		}
	}
	return nil
}

func applyMigration(ctx context.Context, conn *pgx.Conn, table string, version migrationVersion, down bool, output io.Writer) error {
	file, phase := version.up, "applied"
	if down {
		file, phase = version.down, "reverted"
		if file == nil {
			return fmt.Errorf("migration %06d has no down SQL", version.version)
		}
	}
	var err error
	if file.nonTx {
		err = applyNonTransactional(ctx, conn, table, version, file, down)
	} else {
		err = withMigrationTx(ctx, conn, func(tx pgx.Tx) error {
			if err := execMigrationStatement(ctx, conn, file); err != nil {
				return err
			}
			if down {
				return changeMigrationHistory(ctx, tx, "DELETE FROM "+table+" WHERE version=$1", version.version)
			}
			return insertMigrationHistory(ctx, tx, table, version, "applied")
		})
	}
	if err != nil {
		return fmt.Errorf("migration %06d %s: %w", version.version, version.name, err)
	}
	_, err = fmt.Fprintf(output, "%06d %s %s %s\n", version.version, version.name, phase, file.checksum)
	return err
}

func applyNonTransactional(ctx context.Context, conn *pgx.Conn, table string, version migrationVersion, file *migrationFile, down bool) error {
	var err error
	if down {
		err = changeMigrationHistory(ctx, conn, "UPDATE "+table+" SET state='reverting' WHERE version=$1", version.version)
	} else {
		err = insertMigrationHistory(ctx, conn, table, version, "applying")
	}
	if err != nil {
		return err
	}
	if err := execMigrationStatement(ctx, conn, file); err != nil {
		return fmt.Errorf("incomplete non-transactional operation: %w", err)
	}
	if down {
		return changeMigrationHistory(ctx, conn, "DELETE FROM "+table+" WHERE version=$1", version.version)
	}
	return changeMigrationHistory(ctx, conn, "UPDATE "+table+" SET state='applied', applied_at=clock_timestamp() WHERE version=$1", version.version)
}

func insertMigrationHistory(ctx context.Context, writer migrationExecutor, table string, version migrationVersion, state string) error {
	return changeMigrationHistory(ctx, writer,
		"INSERT INTO "+table+" (version,name,up_checksum,down_checksum,state) VALUES ($1,$2,$3,$4,$5)",
		version.version, version.name, version.up.checksum, migrationDownChecksum(version), state)
}

func changeMigrationHistory(ctx context.Context, writer migrationExecutor, sql string, args ...any) error {
	_, err := writer.Exec(ctx, sql, args...)
	return driverFailure("PostgreSQL", databaseKind, err)
}

func execMigrationStatement(ctx context.Context, conn *pgx.Conn, file *migrationFile) error {
	// Extended protocol rejects multiple top-level commands in one file.
	_, err := conn.PgConn().ExecParams(ctx, file.sql, nil, nil, nil, nil).Close()
	return driverFailure("PostgreSQL", databaseKind, err)
}

func withMigrationTx(ctx context.Context, conn *pgx.Conn, apply func(pgx.Tx) error) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return driverFailure("PostgreSQL", databaseKind, err)
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer stop()
		if err := tx.Rollback(cleanup); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			reportCleanupError("migration rollback", driverFailure("PostgreSQL", databaseKind, err))
		}
	}()
	if err := apply(tx); err != nil {
		return err
	}
	return postgresCommitError(tx.Commit(ctx))
}

func runDevelopmentSeeds(ctx context.Context, conn *pgx.Conn, versions []migrationVersion, output io.Writer) error {
	err := withMigrationTx(ctx, conn, func(tx pgx.Tx) error {
		for _, version := range versions {
			if version.down != nil || version.up.nonTx {
				return errors.New("seeds require transactional up files")
			}
			if err := execMigrationStatement(ctx, conn, version.up); err != nil {
				return fmt.Errorf("seed %06d %s: %w", version.version, version.name, err)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, version := range versions {
		if _, err := fmt.Fprintf(output, "%06d %s seeded %s\n", version.version, version.name, version.up.checksum); err != nil {
			return err
		}
	}
	return nil
}
