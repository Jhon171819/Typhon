package typhon

import (
	"context"
	"errors"
	"fmt"
	"strconv"
)

const (
	errorKind      = "Error"
	validationKind = "ValidationError"
	timeoutKind    = "TimeoutError"
	connectionKind = "ConnectionError"
	databaseKind   = "DatabaseError"
)

var exceptionNames = [...]string{errorKind, validationKind, timeoutKind, connectionKind, databaseKind}

func exceptionType(kind string) Type { return Type{Kind: TypeException, Name: kind} }

func exceptionMatches(actual, expected string) bool {
	return actual == expected || expected == errorKind
}

// Exception is a Typhon error value. Go callers can retrieve it with errors.As.
type Exception struct {
	kind    string
	message string
	cause   error
}

func (e *Exception) Kind() string    { return e.kind }
func (e *Exception) Message() string { return e.message }
func (e *Exception) Unwrap() error   { return e.cause }

func (e *Exception) Error() string {
	// Escape control characters before using application messages in diagnostics.
	message := strconv.QuoteToGraphic(e.message)
	return e.kind + ": " + message[1:len(message)-1]
}

func exceptionConstructor(kind string) Value {
	return nativeValue(kind, func(vm *VM, args []Value) (Value, error) {
		if len(args) != 1 || args[0].Kind != ValueString {
			return NoneValue(), &Exception{kind: validationKind, message: kind + " expects one str argument"}
		}
		return Value{Kind: ValueException, Exception: &Exception{kind: kind, message: args[0].Str}}, nil
	})
}

// Cancellation can enter finally blocks. Hard VM limits end bytecode execution.
type executionStop struct {
	cause     error
	cancelled bool
}

func (e *executionStop) Error() string { return e.cause.Error() }
func (e *executionStop) Unwrap() error { return e.cause }

func (vm *VM) executionError() error {
	if err := vm.context().Err(); err != nil {
		return &executionStop{cause: err, cancelled: vm.cleanupCtx == nil}
	}
	return nil
}

func traceFrame(f *Function, line int, err error) error {
	return fmt.Errorf("%s:%d in %s:\n  %w", f.ModulePath, line, f.Name, err)
}

// A completion leaves a region or returns from the function. A negative
// target means a function exit. Errors and returns unwind to depth zero.
type completion struct {
	target, depth int
	line          int
	err           error
	value         Value
}

type activeRegion struct {
	spec      *exceptionRegion
	height    int
	slot      int
	caught    error
	pending   completion
	inFinally bool
}

var errCancellationIO = errors.New("cancellation finally permits native close and rollback, but not new I/O or tasks")

func (vm *VM) unwind(f *frame, exit *completion) bool {
	for len(f.regions) > exit.depth {
		top := &f.regions[len(f.regions)-1]
		clear(f.stack[top.height:])
		f.stack = f.stack[:top.height]
		var stop *executionStop
		errors.As(exit.err, &stop)
		hardStop := stop != nil && !stop.cancelled
		if top.inFinally && top.pending.err != nil {
			var pendingStop *executionStop
			errors.As(top.pending.err, &pendingStop)
			switch {
			case pendingStop != nil || (exit.err != nil && stop == nil):
				reportCleanupError("finally", exit.err)
				*exit = top.pending
				stop = pendingStop
			case stop != nil:
				reportCleanupError("error pending before execution stopped", top.pending.err)
			}
		}
		var exception *Exception
		if stop == nil {
			errors.As(exit.err, &exception)
		}
		// The current PC is just past the opcode that initiated the unwind.
		if exception != nil && f.pc <= top.spec.bodyEnd {
			for _, handler := range top.spec.handlers {
				if exceptionMatches(exception.kind, handler.kind) {
					top.slot, top.caught = handler.slot, exit.err
					if handler.slot >= 0 {
						f.locals[handler.slot] = Value{Kind: ValueException, Exception: exception}
					}
					f.pc = handler.target
					return true
				}
			}
		}
		if top.slot >= 0 {
			f.locals[top.slot] = Value{}
			top.slot = -1
		}
		top.caught = nil
		if hardStop && vm.cleanupCtx != nil {
			vm.cleanupSteps = 0
		}
		if top.spec.cleanup >= 0 && !top.inFinally && !hardStop {
			if stop != nil && vm.cleanupCtx == nil {
				vm.cleanupCtx, vm.cleanupCancel = context.WithTimeout(context.WithoutCancel(vm.context()), cleanupTimeout)
				vm.cleanupSteps = 10_000
				vm.cleanupBytes = 1 << 20
			}
			if vm.cleanupCtx != nil && (vm.cleanupSteps == 0 || vm.cleanupCtx.Err() != nil) {
				f.popRegion()
				continue
			}
			top.inFinally, top.pending = true, *exit
			f.pc = top.spec.cleanup
			return true
		}
		f.popRegion()
	}
	if exit.target < 0 {
		return false
	}
	f.pc = exit.target
	return true
}

func (f *frame) popRegion() {
	f.regions[len(f.regions)-1] = activeRegion{}
	f.regions = f.regions[:len(f.regions)-1]
}

func (f *frame) reraise() error {
	for i := len(f.regions) - 1; i >= 0; i-- {
		if err := f.regions[i].caught; err != nil {
			return err
		}
	}
	return &executionStop{cause: errors.New("no active exception to rethrow")}
}
