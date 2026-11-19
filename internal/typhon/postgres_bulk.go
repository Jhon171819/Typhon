package typhon

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const maxPostgresBulkRows = 4096
const maxPostgresBulkBytes = 8 << 20

type postgresBulkWriter interface {
	CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error)
	SendBatch(context.Context, *pgx.Batch) pgx.BatchResults
}

type postgresBulk struct {
	table   pgx.Identifier
	columns []string
	sql     string
	rows    [][]any
}

func postgresBulkMethods(methods map[string]Type) {
	methods["copy_from"] = nativeFuncType("postgres.copy_from", []Type{listType(StrType()), listType(StrType()), AnyType()}, IntType(), 0)
	methods["exec_many"] = nativeFuncType("postgres.exec_many", []Type{StrType(), AnyType()}, IntType(), 0)
}

func postgresBulkBindings(attrs map[string]Value, typ Type, db *postgresConnection, tx *postgresTransaction) {
	for _, method := range []string{"copy_from", "exec_many"} {
		attrs[method] = driverFunc(typ.Members[method], func(vm *VM, args []Value) (Value, error) {
			return executePostgresBulk(vm, db, tx, method, args)
		})
	}
}

func makePostgresBulk(vm *VM, method string, args []Value) (postgresBulk, error) {
	var command postgresBulk
	width := 0
	if method == "copy_from" {
		segments, columns := *args[0].List, *args[1].List
		if len(segments) < 1 || len(segments) > 2 || len(columns) < 1 || len(columns) > 128 {
			return command, errors.New("PostgreSQL COPY requires table segments and 1 to 128 columns")
		}
		for _, name := range segments {
			if safePostgresIdentifier(name.Str) == "" {
				return command, errors.New("invalid PostgreSQL COPY table identifier")
			}
			command.table = append(command.table, name.Str)
		}
		seen := map[string]bool{}
		for _, name := range columns {
			if safePostgresIdentifier(name.Str) == "" || seen[name.Str] {
				return command, errors.New("invalid or duplicate PostgreSQL COPY column")
			}
			seen[name.Str] = true
			command.columns = append(command.columns, name.Str)
		}
		width = len(columns)
		args = args[1:]
	} else {
		sql := strings.TrimSuffix(strings.TrimSpace(args[0].Str), ";")
		if len(sql) == 0 || len(sql) > 1<<20 || strings.ContainsRune(sql, ';') {
			return command, errors.New("PostgreSQL exec_many expects a single bounded write template")
		}
		words := strings.Fields(sql)
		switch strings.ToUpper(words[0]) {
		case "INSERT", "UPDATE", "DELETE", "MERGE":
		default:
			return command, errors.New("PostgreSQL exec_many expects a single INSERT, UPDATE, DELETE or MERGE template")
		}
		command.sql = sql
	}
	if args[1].Kind != ValueList || len(*args[1].List) > maxPostgresBulkRows {
		return command, errors.New("PostgreSQL batch requires a list of at most 4096 rows")
	}
	list := *args[1].List
	command.rows = make([][]any, 0, len(list))
	if method != "copy_from" && len(list) > 0 && list[0].Kind == ValueList {
		width = len(*list[0].List)
	}
	if width > 128 {
		return command, errors.New("PostgreSQL batch accepts at most 128 values per row")
	}
	var bytes int64
	for _, input := range list {
		if err := vm.executionError(); err != nil {
			return command, err
		}
		if input.Kind != ValueList || len(*input.List) != width {
			return command, errors.New("every PostgreSQL batch row must match the expected number of values")
		}
		record, err := postgresEncodeParams(vm, *input.List)
		if err != nil {
			return command, err
		}
		rowBytes := int64(valueSlotBytes)
		for _, value := range record {
			rowBytes += postgresParamSize(value)
		}
		bytes += rowBytes
		if bytes > maxPostgresBulkBytes {
			return command, errors.New("PostgreSQL batch exceeds 8 MiB of encoded input")
		}
		if err := vm.chargeBytes(rowBytes); err != nil {
			return command, err
		}
		command.rows = append(command.rows, record)
	}
	return command, nil
}

func postgresParamSize(value any) int64 {
	switch data := value.(type) {
	case string:
		return int64(len(data)) + valueSlotBytes
	case []byte:
		return int64(len(data)) + valueSlotBytes
	case json.RawMessage:
		return int64(len(data)) + valueSlotBytes
	case pgtype.Array[any]:
		size := int64(valueSlotBytes)
		for _, element := range data.Elements {
			size += postgresParamSize(element)
		}
		return size
	case pgtype.Numeric:
		if data.Int != nil {
			return int64(data.Int.BitLen()/8) + valueSlotBytes
		}
	}
	return valueSlotBytes
}

func (command postgresBulk) execute(ctx context.Context, writer postgresBulkWriter) (_ int64, runErr error) {
	if len(command.rows) == 0 {
		return 0, nil
	}
	if command.table != nil {
		offset := 0
		return writer.CopyFrom(ctx, command.table, command.columns, pgx.CopyFromFunc(func() ([]any, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if offset == len(command.rows) {
				return nil, nil
			}
			row := command.rows[offset]
			offset++
			return row, nil
		}))
	}
	batch := &pgx.Batch{}
	for _, row := range command.rows {
		batch.Queue(command.sql, row...)
	}
	answers := writer.SendBatch(ctx, batch)
	defer func() {
		if err := answers.Close(); runErr == nil {
			runErr = err
		}
	}()
	count := int64(0)
	for range command.rows {
		tag, err := answers.Exec()
		if err != nil {
			return 0, err
		}
		count += tag.RowsAffected()
	}
	return count, nil
}

func executePostgresBulk(vm *VM, db *postgresConnection, tx *postgresTransaction, method string, args []Value) (Value, error) {
	if tx == nil {
		command, err := makePostgresBulk(vm, method, args)
		if err != nil || len(command.rows) == 0 {
			return IntValue(0), err
		}
		conn, ctx, cancel, err := db.acquireConn(vm)
		if err != nil {
			return NoneValue(), err
		}
		defer conn.Release()
		defer cancel()
		count, err := command.execute(ctx, conn)
		if err != nil {
			return NoneValue(), driverFailure("PostgreSQL", databaseKind, err)
		}
		return IntValue(count), nil
	}
	tx.mu.Lock()
	if err := tx.commandAllowed(); err != nil {
		tx.mu.Unlock()
		return NoneValue(), err
	}
	count, err := func() (int64, error) {
		defer tx.mu.Unlock()
		command, err := makePostgresBulk(vm, method, args)
		if err != nil {
			return 0, err
		}
		ctx, cancel := tx.operation(vm)
		defer cancel()
		n, err := command.execute(ctx, tx.tx)
		return n, driverFailure("PostgreSQL", databaseKind, err)
	}()
	if err != nil {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(vm.context()), cleanupTimeout)
		defer cancel()
		reportCleanupError("PostgreSQL bulk rollback", tx.rollback(cleanup))
		return NoneValue(), err
	}
	return IntValue(count), nil
}
