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
	cursor := postgresCursorType()
	rows := Type{Kind: TypeNative, Name: "typhon.postgres.Rows", Members: map[string]Type{
		"count": IntType(), "columns": listType(StrType()),
		"get": nativeFuncType("postgres.rows.get", []Type{IntType(), StrType()}, AnyType(), 0),
		"row": nativeFuncType("postgres.rows.row", []Type{IntType()}, listType(AnyType()), 0),
	}}
	methods := func() map[string]Type {
		output := map[string]Type{
			"exec":   nativeFuncType("postgres.exec", []Type{StrType(), listType(AnyType())}, IntType(), 1),
			"query":  nativeFuncType("postgres.query", []Type{StrType(), listType(AnyType())}, rows, 1),
			"cursor": nativeFuncType("postgres.cursor", []Type{StrType(), listType(AnyType()), IntType()}, cursor, 2),
		}
		postgresBulkMethods(output)
		return output
	}
	tx := Type{Kind: TypeNative, Name: "typhon.postgres.Transaction", Members: methods()}
	tx.Members["commit"] = nativeFuncType("postgres.commit", nil, NoneType(), 0)
	tx.Members["rollback"] = nativeFuncType("postgres.rollback", nil, NoneType(), 0)
	connection := Type{Kind: TypeNative, Name: "typhon.postgres.Connection", Members: methods()}
	connection.Members["begin"] = nativeFuncType("postgres.begin", []Type{StrType(), BoolType()}, tx, 2)
	callback := nativeFuncType("postgres.transaction.callback", []Type{tx}, AnyType(), 0)
	connection.Members["transaction"] = nativeFuncType("postgres.transaction",
		[]Type{callback, StrType(), BoolType(), IntType()}, AnyType(), 3)
	request := httpDriverTypes()["Request"]
	tenantCallback := nativeFuncType("postgres.tenant_transaction.callback", []Type{tx, request}, AnyType(), 0)
	connection.Members["tenant_transaction"] = nativeFuncType("postgres.tenant_transaction",
		[]Type{request, tenantCallback, StrType(), BoolType(), IntType()}, AnyType(), 3)
	connection.Members["close"] = nativeFuncType("postgres.close", nil, NoneType(), 0)
	connection.Members["stats"] = nativeFuncType("postgres.stats", nil, dictType(StrType(), IntType()), 0)
	types := map[string]Type{
		"Rows": rows, "Cursor": cursor, "Transaction": tx, "Connection": connection,
		"connect":        nativeFuncType("postgres.connect", []Type{StrType(), FloatType()}, connection, 1),
		"connect_typed":  nativeFuncType("postgres.connect_typed", []Type{StrType(), FloatType()}, connection, 1),
		"connect_pooled": nativeFuncType("postgres.connect_pooled", []Type{StrType(), dictType(StrType(), AnyType())}, connection, 0),
		"env":            nativeFuncType("postgres.env", []Type{StrType()}, StrType(), 0),
	}
	postgresParamTypes(types)
	return types
}

func postgresDriverExports(types map[string]Type) map[string]Value {
	exports := driverTypeSymbols(types)
	postgresParamExports(exports, types)
	exports["connect"] = driverFunc(types["connect"], func(vm *VM, args []Value) (Value, error) {
		return nativePostgresConnect(vm, args, false)
	})
	exports["connect_typed"] = driverFunc(types["connect_typed"], func(vm *VM, args []Value) (Value, error) {
		return nativePostgresConnect(vm, args, true)
	})
	exports["connect_pooled"] = driverFunc(types["connect_pooled"], nativePostgresPooled)
	exports["env"] = driverFunc(types["env"], driverEnv)
	return exports
}

type postgresConnection struct {
	scope     *resourceScope
	pool      *pgxpool.Pool
	timeout   time.Duration
	acquire   time.Duration
	grace     time.Duration
	typed     bool
	closing   atomic.Bool
	closed    atomic.Bool
	txs       resourceScope
	cursors   resourceScope
	release   func()
	closeOnce sync.Once
	closeErr  error
	activeMu  sync.Mutex
	activeTxs map[*postgresTransaction]bool
}

type postgresTransaction struct {
	scope          *resourceScope
	parent         *postgresConnection
	tx             pgx.Tx
	conn           *pgxpool.Conn
	ctx            context.Context
	cancel         context.CancelFunc
	done           chan struct{}
	mu             sync.Mutex
	ended          bool
	committed      bool
	commitErr      error
	cursors        resourceScope
	cursor         atomic.Pointer[postgresCursor]
	cursorRollback atomic.Bool
}

func (db *postgresConnection) close(ctx context.Context) error {
	db.closeOnce.Do(func() {
		db.activeMu.Lock()
		db.closing.Store(true)
		active := make([]*postgresTransaction, 0, len(db.activeTxs))
		for tx := range db.activeTxs {
			active = append(active, tx)
		}
		db.activeMu.Unlock()
		if len(active) > 0 && db.grace > 0 {
			wait := db.grace
			if deadline, ok := ctx.Deadline(); ok {
				wait = min(wait, time.Until(deadline)/2)
			}
			grace, cancel := context.WithTimeout(ctx, wait)
			for _, tx := range active {
				select {
				case <-tx.done:
				case <-grace.Done():
				}
			}
			cancel()
		}
		db.closed.Store(true)
		for _, tx := range active {
			tx.cancel()
		}
		db.scope.remove(db)
		db.closeErr = errors.Join(db.cursors.close(ctx), db.txs.close(ctx))
		db.release()
	})
	return db.closeErr
}

func (tx *postgresTransaction) rollback(ctx context.Context) error {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if tx.ended {
		return nil
	}
	cursorErr := tx.cursors.close(ctx)
	var err error
	if !tx.tx.Conn().PgConn().IsClosed() {
		err = tx.tx.Rollback(ctx)
	}
	tx.finish()
	if errors.Is(err, pgx.ErrTxClosed) {
		return cursorErr
	}
	return errors.Join(cursorErr, driverFailure("PostgreSQL", databaseKind, err))
}

func (tx *postgresTransaction) commandAllowed() error {
	if tx.ended || tx.parent.closed.Load() {
		return errors.New("PostgreSQL transaction is closed")
	}
	if tx.cursor.Load() != nil {
		return errors.New("consume or close the PostgreSQL cursor before another transaction command")
	}
	if tx.cursorRollback.Load() {
		return errors.New("roll back the PostgreSQL transaction after a cursor closed early or failed")
	}
	return nil
}

func (tx *postgresTransaction) finish() {
	tx.ended = true
	tx.scope.remove(tx)
	tx.parent.txs.remove(tx)
	tx.cancel()
	tx.conn.Release()
	tx.parent.activeMu.Lock()
	delete(tx.parent.activeTxs, tx)
	tx.parent.activeMu.Unlock()
	close(tx.done)
}

func (tx *postgresTransaction) operation(vm *VM) (context.Context, context.CancelFunc) {
	ctx, cancel := driverContext(vm, tx.parent.timeout)
	stop := context.AfterFunc(tx.ctx, cancel)
	if tx.ctx.Err() != nil {
		cancel()
	}
	return ctx, func() {
		stop()
		cancel()
	}
}

func (db *postgresConnection) acquireConn(vm *VM) (*pgxpool.Conn, context.Context, context.CancelFunc, error) {
	if db.closing.Load() {
		return nil, nil, nil, errors.New("PostgreSQL connection is closed")
	}
	wait := db.timeout
	if db.acquire > 0 {
		wait = db.acquire
	}
	ctx, cancel := driverContext(vm, wait)
	if db.acquire > 0 {
		vm.engine.execMu.Unlock()
	}
	conn, err := db.pool.Acquire(ctx)
	if db.acquire > 0 {
		vm.engine.execMu.Lock()
		cancel()
		ctx, cancel = driverContext(vm, db.timeout)
	}
	if err != nil {
		cancel()
		return nil, nil, nil, driverFailure("PostgreSQL", databaseKind, err)
	}
	if db.closing.Load() {
		conn.Release()
		cancel()
		return nil, nil, nil, errors.New("PostgreSQL connection is closed")
	}
	return conn, ctx, cancel, nil
}

func nativePostgresConnect(vm *VM, args []Value, typed bool) (Value, error) {
	timeout, err := driverTimeout(args, 1)
	if err != nil {
		return NoneValue(), err
	}
	config, err := postgresPoolConfig(vm, args[0].Str, timeout)
	if err != nil {
		return NoneValue(), err
	}
	ctx, cancel := driverContext(vm, timeout)
	defer cancel()
	pool, err := openPostgresPool(ctx, config)
	if err != nil {
		return NoneValue(), err
	}
	return postgresConnectionValue(vm, &postgresConnection{
		scope: vm.resourceScope(), pool: pool, timeout: timeout, typed: typed, release: pool.Close,
	})
}

func postgresPoolConfig(vm *VM, dsn string, timeout time.Duration) (*pgxpool.Config, error) {
	if dsn == "" {
		return nil, fmt.Errorf("PostgreSQL DSN must not be empty")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, driverFailure("PostgreSQL", validationKind, err)
	}
	config.MaxConns = 16
	config.ConnConfig.ConnectTimeout = timeout
	messageLimit := int(min(vm.engine.quotas.engineBytes, 64<<20))
	config.ConnConfig.BuildFrontend = func(r io.Reader, w io.Writer) *pgproto3.Frontend {
		frontend := pgproto3.NewFrontend(r, w)
		frontend.SetMaxBodyLen(messageLimit)
		return frontend
	}
	return config, nil
}

func postgresConnectionValue(vm *VM, db *postgresConnection) (Value, error) {
	if err := db.scope.add(vm.context(), db, "PostgreSQL pool close", db.close); err != nil {
		return NoneValue(), err
	}
	typ := postgresDriverTypes()["Connection"]
	closeHandle := driverCloser(typ.Members["close"], db.close)
	if db.acquire > 0 {
		closeHandle = driverFunc(typ.Members["close"], func(vm *VM, args []Value) (Value, error) {
			ctx, cancel := driverContext(vm, cleanupTimeout)
			defer cancel()
			db.closing.Store(true)
			vm.engine.execMu.Unlock()
			err := db.close(ctx)
			vm.engine.execMu.Lock()
			return NoneValue(), err
		})
		closeHandle.Native.cleanup = true
	}
	attrs := map[string]Value{
		"close": closeHandle,
		"stats": driverFunc(typ.Members["stats"], func(vm *VM, args []Value) (Value, error) {
			if db.closing.Load() {
				return NoneValue(), errors.New("PostgreSQL connection is closed")
			}
			return db.stats(), nil
		}),
		"exec": driverFunc(typ.Members["exec"], func(vm *VM, args []Value) (Value, error) {
			if db.closing.Load() {
				return NoneValue(), errors.New("PostgreSQL connection is closed")
			}
			params, err := postgresParams(vm, args)
			if err != nil {
				return NoneValue(), err
			}
			conn, ctx, cancel, err := db.acquireConn(vm)
			if err != nil {
				return NoneValue(), err
			}
			defer conn.Release()
			defer cancel()
			tag, err := conn.Exec(ctx, args[0].Str, params...)
			return IntValue(tag.RowsAffected()), driverFailure("PostgreSQL", databaseKind, err)
		}),
		"query": driverFunc(typ.Members["query"], func(vm *VM, args []Value) (Value, error) {
			if db.closing.Load() {
				return NoneValue(), errors.New("PostgreSQL connection is closed")
			}
			params, err := postgresParams(vm, args)
			if err != nil {
				return NoneValue(), err
			}
			conn, ctx, cancel, err := db.acquireConn(vm)
			if err != nil {
				return NoneValue(), err
			}
			defer conn.Release()
			defer cancel()
			rows, err := conn.Query(ctx, args[0].Str, params...)
			if err != nil {
				return NoneValue(), driverFailure("PostgreSQL", databaseKind, err)
			}
			return postgresRows(vm, rows, cancel, db.typed)
		}),
		"begin": driverFunc(typ.Members["begin"], func(vm *VM, args []Value) (Value, error) {
			options, err := postgresTxOptions(args)
			if err != nil {
				return NoneValue(), err
			}
			tx, err := db.beginTransaction(vm, options)
			if err != nil {
				return NoneValue(), err
			}
			return postgresTxValue(vm, tx), nil
		}),
		"transaction":        driverFunc(typ.Members["transaction"], db.transaction),
		"tenant_transaction": driverFunc(typ.Members["tenant_transaction"], db.tenantTransaction),
		"cursor":             driverFunc(typ.Members["cursor"], db.cursor),
	}
	postgresBulkBindings(attrs, typ, db, nil)
	value := scopedNativeObject(vm, typ, attrs)
	value.NativeObject.data = db
	return value, nil
}

func postgresTxValue(vm *VM, tx *postgresTransaction) Value {
	typ := postgresDriverTypes()["Transaction"]
	attrs := map[string]Value{
		"exec": driverFunc(typ.Members["exec"], func(vm *VM, args []Value) (Value, error) {
			tx.mu.Lock()
			defer tx.mu.Unlock()
			if err := tx.commandAllowed(); err != nil {
				return NoneValue(), err
			}
			params, err := postgresParams(vm, args)
			if err != nil {
				return NoneValue(), err
			}
			ctx, cancel := tx.operation(vm)
			defer cancel()
			tag, err := tx.tx.Exec(ctx, args[0].Str, params...)
			return IntValue(tag.RowsAffected()), driverFailure("PostgreSQL", databaseKind, err)
		}),
		"query": driverFunc(typ.Members["query"], func(vm *VM, args []Value) (Value, error) {
			tx.mu.Lock()
			defer tx.mu.Unlock()
			if err := tx.commandAllowed(); err != nil {
				return NoneValue(), err
			}
			params, err := postgresParams(vm, args)
			if err != nil {
				return NoneValue(), err
			}
			ctx, cancel := tx.operation(vm)
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
			if err := tx.commandAllowed(); err != nil {
				return NoneValue(), err
			}
			ctx, cancel := tx.operation(vm)
			defer cancel()
			err := tx.tx.Commit(ctx)
			tx.committed = err == nil
			tx.commitErr = postgresCommitError(err)
			tx.finish()
			return NoneValue(), tx.commitErr
		}),
		"rollback": driverCloser(typ.Members["rollback"], tx.rollback),
		"cursor":   driverFunc(typ.Members["cursor"], tx.openCursor),
	}
	postgresBulkBindings(attrs, typ, tx.parent, tx)
	value := scopedNativeObject(vm, typ, attrs)
	value.NativeObject.data = tx
	return value
}

func postgresParams(vm *VM, args []Value) ([]any, error) {
	if len(args) < 2 {
		return nil, nil
	}
	return postgresEncodeParams(vm, *args[1].List)
}

func postgresEncodeParams(vm *VM, values []Value) ([]any, error) {
	params := make([]any, 0, len(values))
	for _, value := range values {
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
		case ValueDict:
			data, err := postgresJSONParam(vm, value)
			if err != nil {
				return nil, err
			}
			params = append(params, data)
		case ValueNativeObject:
			if binding, ok := value.NativeObject.data.(*postgresParameter); ok {
				if err := vm.checkNativeOwner(value.NativeObject); err != nil {
					return nil, err
				}
				params = append(params, binding.data)
				continue
			}
			scalar, err := postgresScalarParam(vm, value)
			if err != nil {
				return nil, err
			}
			params = append(params, scalar)
		case ValueList:
			return nil, fmt.Errorf("use postgres.array or postgres.jsonb for a list parameter")
		default:
			return nil, fmt.Errorf("unsupported SQL parameter: %s", value.Kind)
		}
	}
	return params, nil
}

func postgresScalarParam(vm *VM, value Value) (any, error) {
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
	return scalar.sql, nil
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
		row, err := postgresDecodedRow(vm, rows, typed)
		if err != nil {
			return NoneValue(), err
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

func postgresDecodedRow(vm *VM, rows pgx.Rows, typed bool) ([]Value, error) {
	for _, column := range rows.RawValues() {
		if err := vm.chargeBytes(2*int64(len(column)) + valueSlotBytes); err != nil {
			return nil, err
		}
	}
	var values []any
	var err error
	if typed {
		values = postgresScanTargets(rows.FieldDescriptions())
		err = rows.Scan(values...)
	} else {
		values, err = rows.Values()
	}
	if err != nil {
		return nil, driverFailure("PostgreSQL", databaseKind, err)
	}
	row := make([]Value, 0, len(values))
	for _, value := range values {
		var converted Value
		if typed {
			converted, err = postgresTypedValue(vm, value)
		} else {
			converted, err = postgresValue(value, 0)
		}
		if err != nil {
			return nil, err
		}
		if err := vm.chargeValue(converted, 0); err != nil {
			return nil, err
		}
		row = append(row, converted)
	}
	return row, nil
}
