package typhon

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

func postgresDriverTypes() map[string]Type {
	rows := Type{Kind: TypeNative, Name: "typhon.postgres.Rows", Members: map[string]Type{
		"count": IntType(), "columns": listType(StrType()),
		"get": nativeFuncType("postgres.rows.get", []Type{IntType(), StrType()}, AnyType(), 0),
		"row": nativeFuncType("postgres.rows.row", []Type{IntType()}, listType(AnyType()), 0),
	}}
	methods := func() map[string]Type {
		return map[string]Type{
			"exec":  nativeFuncType("postgres.exec", []Type{StrType(), listType(AnyType())}, IntType(), 1),
			"query": nativeFuncType("postgres.query", []Type{StrType(), listType(AnyType())}, rows, 1),
		}
	}
	tx := Type{Kind: TypeNative, Name: "typhon.postgres.Transaction", Members: methods()}
	tx.Members["commit"] = nativeFuncType("postgres.commit", nil, NoneType(), 0)
	tx.Members["rollback"] = nativeFuncType("postgres.rollback", nil, NoneType(), 0)
	connection := Type{Kind: TypeNative, Name: "typhon.postgres.Connection", Members: methods()}
	connection.Members["begin"] = nativeFuncType("postgres.begin", nil, tx, 0)
	connection.Members["close"] = nativeFuncType("postgres.close", nil, NoneType(), 0)
	return map[string]Type{
		"Rows": rows, "Transaction": tx, "Connection": connection,
		"connect":       nativeFuncType("postgres.connect", []Type{StrType(), FloatType()}, connection, 1),
		"connect_typed": nativeFuncType("postgres.connect_typed", []Type{StrType(), FloatType()}, connection, 1),
		"env":           nativeFuncType("postgres.env", []Type{StrType()}, StrType(), 0),
	}
}

func postgresDriverExports(types map[string]Type) map[string]Value {
	exports := driverTypeSymbols(types)
	exports["connect"] = driverFunc(types["connect"], func(vm *VM, args []Value) (Value, error) {
		return nativePostgresConnect(vm, args, false)
	})
	exports["connect_typed"] = driverFunc(types["connect_typed"], func(vm *VM, args []Value) (Value, error) {
		return nativePostgresConnect(vm, args, true)
	})
	exports["env"] = driverFunc(types["env"], driverEnv)
	return exports
}

type postgresConnection struct {
	scope   *resourceScope
	pool    *pgxpool.Pool
	timeout time.Duration
	typed   bool
	closed  atomic.Bool
	txs     resourceScope
}

type postgresTransaction struct {
	scope  *resourceScope
	parent *postgresConnection
	tx     pgx.Tx
	mu     sync.Mutex
	ended  bool
}

func (db *postgresConnection) close(ctx context.Context) error {
	if db.closed.Swap(true) {
		return nil
	}
	db.scope.remove(db)
	err := db.txs.close(ctx)
	db.pool.Close()
	return err
}

func (tx *postgresTransaction) rollback(ctx context.Context) error {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if tx.ended {
		return nil
	}
	tx.ended = true
	tx.scope.remove(tx)
	tx.parent.txs.remove(tx)
	err := tx.tx.Rollback(ctx)
	if errors.Is(err, pgx.ErrTxClosed) {
		return nil
	}
	return driverFailure("PostgreSQL", databaseKind, err)
}

func nativePostgresConnect(vm *VM, args []Value, typed bool) (Value, error) {
	timeout, err := driverTimeout(args, 1)
	if err != nil {
		return NoneValue(), err
	}
	if args[0].Str == "" {
		return NoneValue(), fmt.Errorf("PostgreSQL DSN must not be empty")
	}
	config, err := pgxpool.ParseConfig(args[0].Str)
	if err != nil {
		return NoneValue(), driverFailure("PostgreSQL", validationKind, err)
	}
	config.MaxConns = 16
	config.ConnConfig.ConnectTimeout = timeout
	messageLimit := int(min(vm.engine.quotas.engineBytes, 64<<20))
	config.ConnConfig.BuildFrontend = func(r io.Reader, w io.Writer) *pgproto3.Frontend {
		frontend := pgproto3.NewFrontend(r, w)
		frontend.SetMaxBodyLen(messageLimit)
		return frontend
	}
	ctx, cancel := driverContext(vm, timeout)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return NoneValue(), driverFailure("PostgreSQL", connectionKind, err)
	}
	if err = pool.Ping(ctx); err != nil {
		pool.Close()
		return NoneValue(), driverFailure("PostgreSQL", connectionKind, err)
	}
	db := &postgresConnection{scope: vm.resourceScope(), pool: pool, timeout: timeout, typed: typed}
	if err := db.scope.add(vm.context(), db, "PostgreSQL pool close", db.close); err != nil {
		return NoneValue(), err
	}
	typ := postgresDriverTypes()["Connection"]
	attrs := map[string]Value{
		"close": driverCloser(typ.Members["close"], db.close),
		"exec": driverFunc(typ.Members["exec"], func(vm *VM, args []Value) (Value, error) {
			if db.closed.Load() {
				return NoneValue(), fmt.Errorf("PostgreSQL connection is closed")
			}
			params, err := postgresParams(vm, args)
			if err != nil {
				return NoneValue(), err
			}
			ctx, cancel := driverContext(vm, db.timeout)
			defer cancel()
			tag, err := db.pool.Exec(ctx, args[0].Str, params...)
			return IntValue(tag.RowsAffected()), driverFailure("PostgreSQL", databaseKind, err)
		}),
		"query": driverFunc(typ.Members["query"], func(vm *VM, args []Value) (Value, error) {
			if db.closed.Load() {
				return NoneValue(), fmt.Errorf("PostgreSQL connection is closed")
			}
			params, err := postgresParams(vm, args)
			if err != nil {
				return NoneValue(), err
			}
			ctx, cancel := driverContext(vm, db.timeout)
			defer cancel()
			rows, err := db.pool.Query(ctx, args[0].Str, params...)
			if err != nil {
				return NoneValue(), driverFailure("PostgreSQL", databaseKind, err)
			}
			return postgresRows(vm, rows, cancel, db.typed)
		}),
		"begin": driverFunc(typ.Members["begin"], func(vm *VM, args []Value) (Value, error) {
			if db.closed.Load() {
				return NoneValue(), fmt.Errorf("PostgreSQL connection is closed")
			}
			ctx, cancel := driverContext(vm, db.timeout)
			defer cancel()
			raw, err := db.pool.Begin(ctx)
			if err != nil {
				return NoneValue(), driverFailure("PostgreSQL", databaseKind, err)
			}
			tx := &postgresTransaction{scope: vm.resourceScope(), parent: db, tx: raw}
			if err := tx.scope.add(vm.context(), tx, "PostgreSQL transaction rollback", tx.rollback); err != nil {
				return NoneValue(), err
			}
			if err := db.txs.add(vm.context(), tx, "PostgreSQL transaction rollback", tx.rollback); err != nil {
				return NoneValue(), err
			}
			return postgresTxValue(vm, tx), nil
		}),
	}
	return scopedNativeObject(vm, typ, attrs), nil
}

func postgresTxValue(vm *VM, tx *postgresTransaction) Value {
	typ := postgresDriverTypes()["Transaction"]
	return scopedNativeObject(vm, typ, map[string]Value{
		"exec": driverFunc(typ.Members["exec"], func(vm *VM, args []Value) (Value, error) {
			tx.mu.Lock()
			defer tx.mu.Unlock()
			if tx.ended || tx.parent.closed.Load() {
				return NoneValue(), fmt.Errorf("PostgreSQL transaction is closed")
			}
			params, err := postgresParams(vm, args)
			if err != nil {
				return NoneValue(), err
			}
			ctx, cancel := driverContext(vm, tx.parent.timeout)
			defer cancel()
			tag, err := tx.tx.Exec(ctx, args[0].Str, params...)
			return IntValue(tag.RowsAffected()), driverFailure("PostgreSQL", databaseKind, err)
		}),
		"query": driverFunc(typ.Members["query"], func(vm *VM, args []Value) (Value, error) {
			tx.mu.Lock()
			defer tx.mu.Unlock()
			if tx.ended || tx.parent.closed.Load() {
				return NoneValue(), fmt.Errorf("PostgreSQL transaction is closed")
			}
			params, err := postgresParams(vm, args)
			if err != nil {
				return NoneValue(), err
			}
			ctx, cancel := driverContext(vm, tx.parent.timeout)
			defer cancel()
			rows, err := tx.tx.Query(ctx, args[0].Str, params...)
			if err != nil {
				return NoneValue(), driverFailure("PostgreSQL", databaseKind, err)
			}
			return postgresRows(vm, rows, cancel, tx.parent.typed)
		}),
		"commit": driverFunc(typ.Members["commit"], func(vm *VM, args []Value) (Value, error) {
			tx.mu.Lock()
			defer tx.mu.Unlock()
			if tx.ended || tx.parent.closed.Load() {
				return NoneValue(), fmt.Errorf("PostgreSQL transaction is closed")
			}
			ctx, cancel := driverContext(vm, tx.parent.timeout)
			defer cancel()
			err := tx.tx.Commit(ctx)
			tx.ended = true
			tx.scope.remove(tx)
			tx.parent.txs.remove(tx)
			return NoneValue(), driverFailure("PostgreSQL", databaseKind, err)
		}),
		"rollback": driverCloser(typ.Members["rollback"], tx.rollback),
	})
}

func postgresParams(vm *VM, args []Value) ([]any, error) {
	if len(args) < 2 {
		return nil, nil
	}
	var params []any
	for _, value := range *args[1].List {
		switch value.Kind {
		case ValueNone:
			params = append(params, nil)
		case ValueInt:
			params = append(params, value.Int)
		case ValueFloat:
			if math.IsInf(value.Float, 0) || math.IsNaN(value.Float) {
				return nil, fmt.Errorf("non-finite SQL parameter")
			}
			params = append(params, value.Float)
		case ValueString:
			params = append(params, value.Str)
		case ValueBool:
			params = append(params, value.Bool)
		case ValueNativeObject:
			if err := vm.checkNativeOwner(value.NativeObject); err != nil {
				return nil, err
			}
			scalar, ok := scalarData(value)
			if !ok {
				return nil, fmt.Errorf("unsupported native SQL parameter")
			}
			switch data := scalar.sql.(type) {
			case pgtype.Timestamp:
				if data.Time.Nanosecond()%1000 != 0 {
					return nil, fmt.Errorf("PostgreSQL timestamp requires microsecond precision")
				}
			case pgtype.Timestamptz:
				if data.Time.Nanosecond()%1000 != 0 {
					return nil, fmt.Errorf("PostgreSQL timestamp requires microsecond precision")
				}
			}
			params = append(params, scalar.sql)
		default:
			return nil, fmt.Errorf("unsupported SQL parameter: %s", value.Kind)
		}
	}
	return params, nil
}

func postgresValue(value any, depth int) (Value, error) {
	if depth > 64 {
		return NoneValue(), fmt.Errorf("PostgreSQL value depth limit exceeded")
	}
	switch value := value.(type) {
	case nil:
		return NoneValue(), nil
	case bool:
		return BoolValue(value), nil
	case int16:
		return IntValue(int64(value)), nil
	case int32:
		return IntValue(int64(value)), nil
	case int64:
		return IntValue(value), nil
	case float32:
		return postgresValue(float64(value), depth)
	case float64:
		if math.IsInf(value, 0) || math.IsNaN(value) {
			return NoneValue(), fmt.Errorf("non-finite PostgreSQL float")
		}
		return FloatValue(value), nil
	case string:
		return StringValue(value), nil
	case []byte:
		return StringValue(string(value)), nil
	case time.Time:
		return StringValue(value.Format(time.RFC3339Nano)), nil
	case pgtype.Numeric:
		encoded, err := value.Value()
		if err != nil {
			return NoneValue(), driverFailure("PostgreSQL", databaseKind, err)
		}
		return postgresValue(encoded, depth+1)
	case [16]byte:
		return StringValue(fmt.Sprintf("%x-%x-%x-%x-%x", value[:4], value[4:6], value[6:8], value[8:10], value[10:])), nil
	case []any:
		var items []Value
		for _, item := range value {
			converted, err := postgresValue(item, depth+1)
			if err != nil {
				return NoneValue(), err
			}
			items = append(items, converted)
		}
		return ListValue(items), nil
	case map[string]any:
		dict := DictValue()
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			item, err := postgresValue(value[key], depth+1)
			if err != nil {
				return NoneValue(), err
			}
			if err := dict.Dict.put(StringValue(key), item); err != nil {
				return NoneValue(), err
			}
		}
		return dict, nil
	case driver.Valuer:
		encoded, err := value.Value()
		if err != nil {
			return NoneValue(), driverFailure("PostgreSQL", databaseKind, err)
		}
		return postgresValue(encoded, depth+1)
	default:
		return NoneValue(), fmt.Errorf("unsupported PostgreSQL result type %T; cast it to text in SQL", value)
	}
}

func postgresRows(vm *VM, rows pgx.Rows, cancel context.CancelFunc, typed bool) (_ Value, runErr error) {
	defer func() {
		var quota *QuotaError
		if errors.As(runErr, &quota) {
			cancel()
		}
		rows.Close()
	}()
	var columns []Value
	index := map[string]int{}
	fields := rows.FieldDescriptions()
	for i, field := range fields {
		if err := vm.chargeBytes(int64(len(field.Name)) + valueSlotBytes); err != nil {
			return NoneValue(), err
		}
		columns = append(columns, StringValue(field.Name))
		index[field.Name] = i
	}
	var result [][]Value
	for rows.Next() {
		if err := vm.executionError(); err != nil {
			return NoneValue(), err
		}
		if err := vm.chargeSQLRow(); err != nil {
			return NoneValue(), err
		}
		for _, column := range rows.RawValues() {
			if err := vm.chargeBytes(2*int64(len(column)) + valueSlotBytes); err != nil {
				return NoneValue(), err
			}
		}
		var values []any
		var err error
		if typed {
			values = postgresScanTargets(fields)
			err = rows.Scan(values...)
		} else {
			values, err = rows.Values()
		}
		if err != nil {
			return NoneValue(), driverFailure("PostgreSQL", databaseKind, err)
		}
		var row []Value
		for _, value := range values {
			var converted Value
			if typed {
				converted, err = postgresTypedValue(vm, value)
			} else {
				converted, err = postgresValue(value, 0)
			}
			if err != nil {
				return NoneValue(), err
			}
			if err := vm.chargeValue(converted, 0); err != nil {
				return NoneValue(), err
			}
			row = append(row, converted)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return NoneValue(), driverFailure("PostgreSQL", databaseKind, err)
	}
	typ := postgresDriverTypes()["Rows"]
	getRow := func(index int64) ([]Value, error) {
		if index < 0 || index >= int64(len(result)) {
			return nil, fmt.Errorf("PostgreSQL row index out of range")
		}
		return result[index], nil
	}
	return nativeObject(vm, typ, map[string]Value{
		"count": IntValue(int64(len(result))), "columns": ListValue(columns),
		"get": driverFunc(typ.Members["get"], func(vm *VM, args []Value) (Value, error) {
			row, err := getRow(args[0].Int)
			if err != nil {
				return NoneValue(), err
			}
			col, ok := index[args[1].Str]
			if !ok {
				return NoneValue(), fmt.Errorf("unknown PostgreSQL column")
			}
			return row[col], nil
		}),
		"row": driverFunc(typ.Members["row"], func(vm *VM, args []Value) (Value, error) {
			row, err := getRow(args[0].Int)
			if err != nil {
				return NoneValue(), err
			}
			return ListValue(row), nil
		}),
	}), nil
}
