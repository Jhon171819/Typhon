package typhon

import (
	"context"
	"errors"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const maxPostgresFetchSize = 1024
const maxPostgresBatchBytes = 4 << 20

type postgresCursor struct {
	mu        sync.Mutex
	scope     *resourceScope
	owner     *resourceScope
	rows      pgx.Rows
	conn      *pgxpool.Conn
	tx        *postgresTransaction
	cancel    context.CancelFunc
	typed     bool
	fetchSize int
	pending   bool
	closed    bool
	drained   bool
}

func postgresCursorType() Type {
	row := listType(AnyType())
	return Type{Kind: TypeNative, Name: "typhon.postgres.Cursor", Members: map[string]Type{
		"columns":    listType(StrType()),
		"fetch_size": IntType(),
		"next":       nativeFuncType("postgres.cursor.next", nil, unionType(row, NoneType()), 0),
		"batch":      nativeFuncType("postgres.cursor.batch", nil, listType(row), 0),
		"close":      nativeFuncType("postgres.cursor.close", nil, NoneType(), 0),
	}}
}

func postgresFetchSize(args []Value) (int, error) {
	if len(args) < 3 {
		return 128, nil
	}
	size := args[2].Int
	if size < 1 || size > maxPostgresFetchSize {
		return 0, errors.New("PostgreSQL cursor fetch_size must be between 1 and 1024")
	}
	return int(size), nil
}

func (db *postgresConnection) cursor(vm *VM, args []Value) (Value, error) {
	fetch, err := postgresFetchSize(args)
	if err != nil {
		return NoneValue(), err
	}
	params, err := postgresParams(vm, args)
	if err != nil {
		return NoneValue(), err
	}
	conn, ctx, cancel, err := db.acquireConn(vm)
	if err != nil {
		return NoneValue(), err
	}
	rows, err := conn.Query(ctx, args[0].Str, params...)
	if err != nil {
		cancel()
		conn.Release()
		return NoneValue(), driverFailure("PostgreSQL", databaseKind, err)
	}
	cursor := &postgresCursor{
		scope: vm.resourceScope(), owner: &db.cursors, rows: rows, conn: conn,
		cancel: cancel, typed: db.typed, fetchSize: fetch,
	}
	return cursor.bind(vm)
}

func (tx *postgresTransaction) openCursor(vm *VM, args []Value) (Value, error) {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if tx.scope != vm.resourceScope() {
		return NoneValue(), errors.New("PostgreSQL cursor must use the transaction's own execution scope")
	}
	if err := tx.commandAllowed(); err != nil {
		return NoneValue(), err
	}
	fetch, err := postgresFetchSize(args)
	if err != nil {
		return NoneValue(), err
	}
	params, err := postgresParams(vm, args)
	if err != nil {
		return NoneValue(), err
	}
	ctx, cancel := tx.operation(vm)
	rows, err := tx.tx.Query(ctx, args[0].Str, params...)
	if err != nil {
		cancel()
		return NoneValue(), driverFailure("PostgreSQL", databaseKind, err)
	}
	cursor := &postgresCursor{
		scope: vm.resourceScope(), owner: &tx.cursors, rows: rows, tx: tx,
		cancel: cancel, typed: tx.parent.typed, fetchSize: fetch,
	}
	tx.cursor.Store(cursor)
	return cursor.bind(vm)
}

func (c *postgresCursor) bind(vm *VM) (Value, error) {
	if err := c.owner.add(vm.context(), c, "PostgreSQL cursor close", c.close); err != nil {
		return NoneValue(), err
	}
	if err := c.scope.add(vm.context(), c, "PostgreSQL cursor close", c.close); err != nil {
		return NoneValue(), err
	}
	fields := c.rows.FieldDescriptions()
	columns := make([]Value, 0, len(fields))
	for _, field := range fields {
		if err := vm.chargeBytes(int64(len(field.Name)) + valueSlotBytes); err != nil {
			_ = c.close(vm.context())
			return NoneValue(), err
		}
		columns = append(columns, StringValue(field.Name))
	}
	typ := postgresCursorType()
	value := scopedNativeObject(vm, typ, map[string]Value{
		"columns": ListValue(columns), "fetch_size": IntValue(int64(c.fetchSize)),
		"next": driverFunc(typ.Members["next"], func(vm *VM, args []Value) (Value, error) {
			result, err := c.take(vm, 1)
			if err != nil || len(result) == 0 {
				return NoneValue(), err
			}
			return result[0], nil
		}),
		"batch": driverFunc(typ.Members["batch"], func(vm *VM, args []Value) (Value, error) {
			rows, err := c.take(vm, c.fetchSize)
			if err != nil {
				return NoneValue(), err
			}
			return ListValue(rows), nil
		}),
		"close": postgresCursorCloser(typ, c),
	})
	value.NativeObject.data = c
	return value, nil
}

func postgresCursorCloser(typ Type, c *postgresCursor) Value {
	closer := driverFunc(typ.Members["close"], func(vm *VM, args []Value) (Value, error) {
		if c.scope != vm.resourceScope() {
			return NoneValue(), errors.New("PostgreSQL cursor belongs to another execution scope")
		}
		return NoneValue(), c.close(vm.context())
	})
	closer.Native.cleanup = true
	return closer
}

func (c *postgresCursor) take(vm *VM, limit int) ([]Value, error) {
	if c.scope != vm.resourceScope() {
		return nil, errors.New("PostgreSQL cursor belongs to another execution scope")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		if c.drained {
			return nil, nil
		}
		return nil, errors.New("PostgreSQL cursor is closed")
	}
	stop := context.AfterFunc(vm.context(), c.cancel)
	defer stop()
	var batch []Value
	var bytes int64
	for len(batch) < limit {
		if err := vm.executionError(); err != nil {
			c.finish(false)
			return nil, err
		}
		if !c.pending && !c.rows.Next() {
			err := c.rows.Err()
			c.finish(err == nil)
			return batch, driverFailure("PostgreSQL", databaseKind, err)
		}
		c.pending = true
		var size int64
		for _, raw := range c.rows.RawValues() {
			size += int64(len(raw)) + valueSlotBytes
		}
		if size > maxPostgresBatchBytes {
			c.finish(false)
			return nil, quotaStop("PostgreSQL cursor row byte", maxPostgresBatchBytes)
		}
		if bytes+size > maxPostgresBatchBytes {
			break
		}
		if err := vm.chargeSQLCursorRow(); err != nil {
			c.finish(false)
			return nil, err
		}
		row, err := postgresDecodedRow(vm, c.rows, c.typed)
		if err != nil {
			c.finish(false)
			return nil, err
		}
		if err := vm.chargeBytes(valueSlotBytes); err != nil {
			c.finish(false)
			return nil, err
		}
		c.pending = false
		bytes += size
		batch = append(batch, ListValue(row))
	}
	return batch, nil
}

func (c *postgresCursor) close(context.Context) error {
	c.cancel()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.finish(false)
	return nil
}

func (c *postgresCursor) finish(drained bool) {
	if c.closed {
		return
	}
	c.closed, c.drained = true, drained
	c.cancel()
	c.rows.Close()
	c.owner.remove(c)
	c.scope.remove(c)
	if c.tx != nil {
		if !drained {
			c.tx.cursorRollback.Store(true)
		}
		c.tx.cursor.CompareAndSwap(c, nil)
	}
	if c.conn != nil {
		c.conn.Release()
	}
}
