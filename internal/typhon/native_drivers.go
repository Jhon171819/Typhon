package typhon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/redis/go-redis/v9"
)

func nativeFuncType(name string, params []Type, result Type, optional int) Type {
	return Type{Kind: TypeFunc, Name: name, Params: params, Return: typePtr(result), Optional: optional}
}

func driverFunc(typ Type, fn func(*VM, []Value) (Value, error)) Value {
	return Value{Kind: ValueNative, Native: &NativeFunc{Name: typ.Name, Signature: typ, Fn: fn}}
}

func listType(elem Type) Type { return Type{Kind: TypeList, Name: "list", Elem: typePtr(elem)} }

func nativeDriverInfo(name string) (*ModuleInfo, bool) {
	var exports map[string]Type
	switch name {
	case "typhon.http":
		exports = httpDriverTypes()
	case "typhon.postgres":
		exports = postgresDriverTypes()
	case "typhon.redis":
		exports = redisDriverTypes()
	default:
		return nil, false
	}
	return &ModuleInfo{Path: "<" + name + ">", Exports: exports}, true
}

func nativeDriverModule(name string) (Value, bool) {
	info, ok := nativeDriverInfo(name)
	if !ok {
		return NoneValue(), false
	}
	var exports map[string]Value
	switch name {
	case "typhon.http":
		exports = httpDriverExports(info.Exports)
	case "typhon.postgres":
		exports = postgresDriverExports(info.Exports)
	case "typhon.redis":
		exports = redisDriverExports(info.Exports)
	}
	return Value{Kind: ValueNativeModule, Module: &NativeModule{Name: name, Exports: exports}}, true
}

func nativeObject(vm *VM, typ Type, attrs map[string]Value) Value {
	return Value{Kind: ValueNativeObject, NativeObject: &NativeObject{Type: typ, Owner: vm.engine, Attrs: attrs}}
}

func driverTypeSymbols(types map[string]Type) map[string]Value {
	exports := map[string]Value{}
	for name, typ := range types {
		if typ.Kind == TypeNative {
			exports[name] = Value{Kind: ValueNativeObject, NativeObject: &NativeObject{Type: Type{Kind: TypeNative, Name: "type " + typ.Name}, Attrs: map[string]Value{}}}
		}
	}
	return exports
}

func callNativeDriver(vm *VM, fn *NativeFunc, args []Value) (Value, error) {
	typ := fn.Signature
	if len(args) < len(typ.Params)-typ.Optional || len(args) > len(typ.Params) {
		return NoneValue(), &Exception{kind: validationKind, message: fmt.Sprintf("%s expected %d to %d arguments, got %d", fn.Name, len(typ.Params)-typ.Optional, len(typ.Params), len(args))}
	}
	converted := make([]Value, len(args))
	for i, arg := range args {
		if arg.Kind == ValueNativeObject && arg.NativeObject.Owner != nil && arg.NativeObject.Owner != vm.engine {
			return NoneValue(), fmt.Errorf("native object belongs to another Engine")
		}
		value, err := validateValue(arg, typ.Params[i])
		if err != nil {
			return NoneValue(), &Exception{kind: validationKind, message: fmt.Sprintf("%s argument %d: %s", fn.Name, i+1, err)}
		}
		converted[i] = value
	}
	value, err := fn.Fn(vm, converted)
	if stop := vm.executionError(); stop != nil {
		return NoneValue(), stop
	}
	if err != nil {
		var exception *Exception
		if errors.As(err, &exception) {
			return NoneValue(), fmt.Errorf("%s: %w", fn.Name, err)
		}
		// Driver implementations sanitize third-party failures at the call site.
		// Remaining errors are local diagnostics with controlled text.
		return NoneValue(), &Exception{kind: validationKind, message: fmt.Sprintf("%s: %s", fn.Name, err)}
	}
	return validateValue(value, *typ.Return)
}

// NativeCause keeps only structured diagnostics safe to pass across the
// driver boundary. Raw third-party errors may contain credentials or data.
type NativeCause struct {
	driver string
	code   string
	detail string
	marker error
}

func (e *NativeCause) Driver() string { return e.driver }
func (e *NativeCause) Code() string   { return e.code }
func (e *NativeCause) Unwrap() error  { return e.marker }
func (e *NativeCause) Error() string {
	if e.code != "" {
		return e.driver + ": " + e.detail + " (SQLSTATE " + e.code + ")"
	}
	return e.driver + ": " + e.detail
}

func driverFailure(driver, kind string, err error) error {
	if err == nil {
		return nil
	}
	code := ""
	if kind != validationKind {
		var network net.Error
		var postgres *pgconn.PgError
		var connect *pgconn.ConnectError
		switch {
		case errors.Is(err, context.DeadlineExceeded), errors.Is(err, redis.ErrPoolTimeout),
			pgconn.Timeout(err), errors.As(err, &network) && network.Timeout():
			kind = timeoutKind
		case errors.As(err, &postgres):
			kind = databaseKind
			code = postgres.Code
			if len(code) != 5 || !validSQLState(code) {
				code = ""
			}
		case errors.As(err, &network), errors.As(err, &connect),
			errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, net.ErrClosed):
			kind = connectionKind
		}
	}
	var detail string
	switch kind {
	case timeoutKind:
		detail = "operation deadline exceeded"
	case connectionKind:
		detail = "connection or transport failed"
	case databaseKind:
		detail = "database operation failed"
	case validationKind:
		detail = "invalid request or connection configuration"
	}
	cause := &NativeCause{driver: driver, code: code, detail: detail}
	for _, sentinel := range []error{context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, sentinel) {
			cause.marker = sentinel
			break
		}
	}
	return &Exception{kind: kind, message: cause.Error(), cause: cause}
}

func validSQLState(code string) bool {
	for _, ch := range code {
		if (ch < '0' || ch > '9') && (ch < 'A' || ch > 'Z') {
			return false
		}
	}
	return true
}

func driverTimeout(args []Value, index int) (time.Duration, error) {
	if len(args) <= index {
		return 10 * time.Second, nil
	}
	seconds := numberValue(args[index])
	if seconds <= 0 || seconds > 300 || math.IsNaN(seconds) || math.IsInf(seconds, 0) {
		return 0, fmt.Errorf("timeout must be greater than 0 and at most 300 seconds")
	}
	return time.Duration(seconds * float64(time.Second)), nil
}

func driverContext(vm *VM, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(vm.context(), timeout)
}

// Resources are owned by the Engine. Explicit close removes the registration.
// HTTP callbacks use a separate scope so an abandoned transaction rolls back
// after the request, rather than retaining a pool connection until shutdown.
type resourceScope struct {
	mu      sync.Mutex
	closed  bool
	closers map[any]func() error
}

func (scope *resourceScope) add(key any, close func() error) error {
	scope.mu.Lock()
	if scope.closed {
		scope.mu.Unlock()
		_ = close()
		return fmt.Errorf("native resource scope is closed")
	}
	if scope.closers == nil {
		scope.closers = map[any]func() error{}
	}
	scope.closers[key] = close
	scope.mu.Unlock()
	return nil
}

func (scope *resourceScope) remove(key any) {
	scope.mu.Lock()
	defer scope.mu.Unlock()
	delete(scope.closers, key)
}

func (scope *resourceScope) close() error {
	scope.mu.Lock()
	if scope.closed {
		scope.mu.Unlock()
		return nil
	}
	scope.closed = true
	closers := scope.closers
	scope.closers = nil
	scope.mu.Unlock()
	var first error
	for _, close := range closers {
		if err := close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (vm *VM) resourceScope() *resourceScope {
	if vm.resources != nil {
		return vm.resources
	}
	return &vm.engine.resources
}

func driverEnv(vm *VM, args []Value) (Value, error) {
	return StringValue(os.Getenv(args[0].Str)), nil
}
