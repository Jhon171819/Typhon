package typhon

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func postgresTxOptions(args []Value) (pgx.TxOptions, error) {
	var options pgx.TxOptions
	if len(args) > 0 {
		switch args[0].Str {
		case "read_committed":
			options.IsoLevel = pgx.ReadCommitted
		case "repeatable_read":
			options.IsoLevel = pgx.RepeatableRead
		case "serializable":
			options.IsoLevel = pgx.Serializable
		default:
			return options, errors.New("PostgreSQL isolation must be read_committed, repeatable_read, or serializable")
		}
	}
	if len(args) > 1 && args[1].Bool {
		options.AccessMode = pgx.ReadOnly
	}
	return options, nil
}

func (db *postgresConnection) beginTransaction(vm *VM, options pgx.TxOptions) (*postgresTransaction, error) {
	conn, ctx, cancel, err := db.acquireConn(vm)
	if err != nil {
		return nil, err
	}
	defer cancel()
	raw, err := conn.BeginTx(ctx, options)
	if err != nil {
		conn.Release()
		return nil, driverFailure("PostgreSQL", databaseKind, err)
	}
	lifetime, stop := context.WithCancel(vm.context())
	tx := &postgresTransaction{
		scope: vm.resourceScope(), parent: db, tx: raw, conn: conn, ctx: lifetime, cancel: stop, done: make(chan struct{}),
	}
	if err := tx.scope.add(vm.context(), tx, "PostgreSQL transaction rollback", tx.rollback); err != nil {
		return nil, err
	}
	if err := db.txs.add(vm.context(), tx, "PostgreSQL transaction rollback", tx.rollback); err != nil {
		return nil, err
	}
	db.activeMu.Lock()
	if db.activeTxs == nil {
		db.activeTxs = make(map[*postgresTransaction]bool)
	}
	db.activeTxs[tx] = true
	db.activeMu.Unlock()
	return tx, nil
}

func postgresRetryable(err error) bool {
	var cause *NativeCause
	if !errors.As(err, &cause) || cause.Driver() != "PostgreSQL" {
		return false
	}
	return cause.Code() == "40001" || cause.Code() == "40P01"
}

func postgresCommitError(err error) error {
	converted := driverFailure("PostgreSQL", databaseKind, err)
	var server *pgconn.PgError
	if err == nil || errors.Is(err, pgx.ErrTxCommitRollback) || errors.Is(err, pgx.ErrTxClosed) || errors.As(err, &server) {
		return converted
	}
	var failure *Exception
	if errors.As(converted, &failure) {
		return &Exception{kind: failure.kind, message: "PostgreSQL commit outcome is unknown", cause: failure}
	}
	return converted
}

func (db *postgresConnection) transaction(vm *VM, args []Value) (Value, error) {
	return db.scopedTransaction(vm, args[0], args[1:], nil)
}

func (db *postgresConnection) scopedTransaction(vm *VM, callback Value, args []Value, prepare func(*postgresTransaction) error, extras ...Value) (Value, error) {
	options, err := postgresTxOptions(args)
	if err != nil {
		return NoneValue(), err
	}
	retries := int64(0)
	if len(args) > 2 {
		retries = args[2].Int
		if retries < 0 || retries > 4 {
			return NoneValue(), errors.New("PostgreSQL transaction retries must be between 0 and 4")
		}
	}
	for attempt := int64(0); ; attempt++ {
		tx, err := db.beginTransaction(vm, options)
		if err != nil {
			return NoneValue(), err
		}
		value := NoneValue()
		if prepare != nil {
			err = prepare(tx)
		}
		if err == nil {
			value, err = vm.callValue(callback, append([]Value{postgresTxValue(vm, tx)}, extras...), nil)
		}
		tx.mu.Lock()
		canRetry := !tx.committed && (tx.commitErr == nil || postgresRetryable(tx.commitErr))
		if !tx.ended && tx.tx.Conn().PgConn().TxStatus() == 'I' {
			// Raw transaction control may have already persisted changes.
			canRetry = false
		}
		tx.mu.Unlock()
		cleanup, stop := context.WithTimeout(context.WithoutCancel(vm.context()), cleanupTimeout)
		rollbackErr := tx.rollback(cleanup)
		stop()
		if rollbackErr != nil {
			if err == nil {
				return NoneValue(), rollbackErr
			}
			reportCleanupError("PostgreSQL scoped transaction", rollbackErr)
			return NoneValue(), err
		}
		if err == nil || !canRetry || attempt >= retries || !postgresRetryable(err) {
			return value, err
		}
		if err := vm.executionError(); err != nil {
			return NoneValue(), err
		}
		base := (10 * time.Millisecond) << attempt
		timer := time.NewTimer(base + rand.N(base))
		vm.engine.execMu.Unlock()
		select {
		case <-timer.C:
		case <-vm.context().Done():
		}
		timer.Stop()
		vm.engine.execMu.Lock()
		if err := vm.executionError(); err != nil {
			return NoneValue(), err
		}
	}
}
