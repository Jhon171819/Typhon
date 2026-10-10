package typhon

import (
	"context"
	"fmt"
	"math"
	"os"
	"sync"
	"time"
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
		return NoneValue(), fmt.Errorf("%s expected %d to %d arguments, got %d", fn.Name, len(typ.Params)-typ.Optional, len(typ.Params), len(args))
	}
	converted := make([]Value, len(args))
	for i, arg := range args {
		if arg.Kind == ValueNativeObject && arg.NativeObject.Owner != nil && arg.NativeObject.Owner != vm.engine {
			return NoneValue(), fmt.Errorf("native object belongs to another Engine")
		}
		value, err := validateValue(arg, typ.Params[i])
		if err != nil {
			return NoneValue(), fmt.Errorf("%s argument %d: %w", fn.Name, i+1, err)
		}
		converted[i] = value
	}
	value, err := fn.Fn(vm, converted)
	if err != nil {
		return NoneValue(), fmt.Errorf("%s: %w", fn.Name, err)
	}
	return validateValue(value, *typ.Return)
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
