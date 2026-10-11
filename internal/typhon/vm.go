package typhon

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

type VM struct {
	resources    *resourceScope
	ctx          context.Context
	depth        int
	instructions int
	engine       *Engine
	module       *LoadedModule
}

type frame struct {
	fn     *Function
	locals []Value
	stack  []Value
	pc     int
	module *LoadedModule
}

func runtimeGlobals() map[string]Value {
	globals := map[string]Value{}
	globals["print"] = nativeValue("print", nativePrint)
	globals["len"] = nativeValue("len", nativeLen)
	globals["dict"] = nativeValue("dict", nativeDict)
	globals["spawn"] = nativeValue("spawn", nativeSpawn)
	globals["join"] = nativeValue("join", nativeJoin)
	globals["cancel"] = nativeValue("cancel", nativeCancel)
	globals["enumerate"] = nativeValue("enumerate", nativeEnumerate)
	globals["range"] = nativeValue("range", nativeRange)
	globals["TyphonTask"] = Value{Kind: ValueClass, Class: &RuntimeClass{Spec: &ClassSpec{Name: "TyphonTask"}, FieldIndex: map[string]int{}}}
	globals["None"] = NoneValue()
	globals["True"] = BoolValue(true)
	globals["False"] = BoolValue(false)
	globals["__name__"] = StringValue("")
	globals["__file__"] = StringValue("")
	return globals
}

func nativeValue(name string, fn func(*VM, []Value) (Value, error)) Value {
	return Value{Kind: ValueNative, Native: &NativeFunc{Name: name, Fn: fn}}
}

func (vm *VM) runFunction(fn *Function, args []Value) (result Value, runErr error) {
	if vm.depth == 0 {
		vm.engine.execMu.Lock()
		defer vm.engine.execMu.Unlock()
	}
	vm.depth++
	defer func() { vm.depth-- }()
	if vm.depth > 256 {
		return NoneValue(), fmt.Errorf("Typhon call depth limit exceeded")
	}
	previousModule := vm.module
	line := 0
	defer func() {
		vm.module = previousModule
		if runErr != nil {
			runErr = fmt.Errorf("%s:%d in %s: %w", fn.ModulePath, line, fn.Name, runErr)
		}
	}()
	if fn.Module != nil {
		vm.module = fn.Module
	}
	if vm.module == nil {
		vm.module = fn.Module
	}
	if len(args) != len(fn.Params) {
		return NoneValue(), fmt.Errorf("%s expected %d arguments, got %d", fn.Name, len(fn.Params), len(args))
	}
	locals := make([]Value, len(fn.LocalNames))
	for i, arg := range args {
		if i < len(fn.Signature.Params) {
			var err error
			arg, err = validateValue(arg, fn.Signature.Params[i])
			if err != nil {
				return NoneValue(), fmt.Errorf("argument %s: %w", fn.Params[i], err)
			}
		}
		locals[i] = arg
	}
	f := &frame{fn: fn, locals: locals, module: vm.module}

	for f.pc < len(fn.Code) {
		if err := vm.context().Err(); err != nil {
			return NoneValue(), err
		}
		vm.instructions++
		if vm.instructions > 10_000_000 {
			return NoneValue(), fmt.Errorf("instruction budget exceeded")
		}
		instr := fn.Code[f.pc]
		line = instr.Line
		f.pc++
		switch instr.Op {
		case OpDup:
			f.push(f.peek())
		case OpJumpIfTrue:
			if truthy(f.pop()) {
				f.pc = instr.A
			}
		case OpUnary:
			value := f.pop()
			if instr.S == "not" {
				f.push(BoolValue(!truthy(value)))
				continue
			}
			if value.Kind != ValueInt && value.Kind != ValueFloat {
				return NoneValue(), fmt.Errorf("unary sign requires a number")
			}
			if instr.S == "-" {
				if value.Kind == ValueInt {
					if value.Int == math.MinInt64 {
						return NoneValue(), fmt.Errorf("int64 overflow in unary -")
					}
					value.Int = -value.Int
				} else {
					value.Float = -value.Float
				}
			}
			f.push(value)
		case OpCompare:
			right, left := f.pop(), f.pop()
			cmp, err := compareValues(left, right)
			if err != nil {
				return NoneValue(), err
			}
			result := false
			switch instr.S {
			case "<":
				result = cmp < 0
			case "<=":
				result = cmp <= 0
			case ">":
				result = cmp > 0
			case ">=":
				result = cmp >= 0
			}
			f.push(BoolValue(result))
		case OpCheckType:
			value, err := validateValue(f.pop(), instr.Type)
			if err != nil {
				return NoneValue(), err
			}
			f.push(value)
		case OpConst:
			f.push(instr.Val)
		case OpLoadGlobal:
			value, ok := f.module.Globals[instr.S]
			if !ok {
				return NoneValue(), fmt.Errorf("%s: unknown global %q", fn.Name, instr.S)
			}
			f.push(value)
		case OpStoreGlobal:
			f.module.Globals[instr.S] = f.pop()
		case OpLoadLocal:
			if f.locals[instr.A].Kind == "" {
				return NoneValue(), fmt.Errorf("variable %q is not initialized", fn.LocalNames[instr.A])
			}
			f.push(f.locals[instr.A])
		case OpStoreLocal:
			f.locals[instr.A] = f.pop()
		case OpPop:
			_ = f.pop()
		case OpAdd, OpSub, OpMul, OpDiv, OpFloorDiv, OpMod:
			right := f.pop()
			left := f.pop()
			value, err := arithmetic(instr.Op, left, right)
			if err != nil {
				return NoneValue(), err
			}
			f.push(value)
		case OpEqual:
			right := f.pop()
			left := f.pop()
			f.push(BoolValue(valuesEqual(left, right)))
		case OpNotEqual:
			right := f.pop()
			left := f.pop()
			f.push(BoolValue(!valuesEqual(left, right)))
		case OpIs:
			right := f.pop()
			left := f.pop()
			f.push(BoolValue(valuesIdentical(left, right)))
		case OpIsNot:
			right := f.pop()
			left := f.pop()
			f.push(BoolValue(!valuesIdentical(left, right)))
		case OpJumpIfFalse:
			if !truthy(f.pop()) {
				f.pc = instr.A
			}
		case OpJump:
			f.pc = instr.A
		case OpCall:
			totalArgs := instr.A
			args := make([]Value, totalArgs)
			for i := totalArgs - 1; i >= 0; i-- {
				args[i] = f.pop()
			}
			keywordCount := len(instr.Keywords)
			positionalCount := totalArgs - keywordCount
			kwargs := map[string]Value{}
			for i, name := range instr.Keywords {
				kwargs[name] = args[positionalCount+i]
			}
			callee := f.pop()
			result, err := vm.callValue(callee, args[:positionalCount], kwargs)
			if err != nil {
				return NoneValue(), err
			}
			f.push(result)
		case OpReturn:
			value := f.pop()
			if fn.Signature.Return != nil {
				return validateValue(value, *fn.Signature.Return)
			}
			return value, nil
		case OpMakeList:
			items := make([]Value, instr.A)
			for i := instr.A - 1; i >= 0; i-- {
				items[i] = f.pop()
			}
			f.push(ListValue(items))
		case OpMakeDict:
			if instr.A > maxCollectionItems {
				return NoneValue(), fmt.Errorf("dictionary size limit exceeded")
			}
			entries := make([]DictEntry, instr.A)
			for i := instr.A - 1; i >= 0; i-- {
				entries[i].Value = f.pop()
				entries[i].Key = f.pop()
			}
			dict := DictValue()
			for _, entry := range entries {
				if err := dict.Dict.put(entry.Key, entry.Value); err != nil {
					return NoneValue(), err
				}
			}
			f.push(dict)
		case OpGetIndex:
			key, obj := f.pop(), f.pop()
			value, err := getIndex(obj, key)
			if err != nil {
				return NoneValue(), err
			}
			f.push(value)
		case OpSetIndex:
			key, obj, value := f.pop(), f.pop(), f.pop()
			if err := setIndex(obj, key, value); err != nil {
				return NoneValue(), err
			}
		case OpDelIndex:
			key, obj := f.pop(), f.pop()
			if obj.Kind != ValueDict || obj.Dict == nil {
				return NoneValue(), fmt.Errorf("del requires a dictionary")
			}
			key, err := obj.Dict.checkKey(key)
			if err != nil {
				return NoneValue(), err
			}
			i, ok, err := obj.Dict.lookup(key)
			if err != nil {
				return NoneValue(), err
			}
			if !ok {
				return NoneValue(), fmt.Errorf("KeyError: %s", key.String())
			}
			obj.Dict.remove(i)
		case OpContains:
			obj, key := f.pop(), f.pop()
			ok, err := containsValue(obj, key)
			if err != nil {
				return NoneValue(), err
			}
			if instr.S == "not in" {
				ok = !ok
			}
			f.push(BoolValue(ok))
		case OpGetAttr:
			value, err := vm.getAttr(f.pop(), instr.S)
			if err != nil {
				return NoneValue(), err
			}
			f.push(value)
		case OpSetAttr:
			value := f.pop()
			object := f.pop()
			if err := vm.setAttr(object, instr.S, value); err != nil {
				return NoneValue(), err
			}
		case OpIter:
			iterable := f.pop()
			items, err := collectionValues(iterable)
			if err != nil {
				return NoneValue(), err
			}
			f.push(Value{Kind: ValueIterator, Iterator: &Iterator{values: items}})
		case OpIterNext:
			iter := f.peek()
			if iter.Kind != ValueIterator || iter.Iterator == nil {
				return NoneValue(), fmt.Errorf("invalid iterator")
			}
			if iter.Iterator.index >= len(iter.Iterator.values) {
				f.pc = instr.A
				continue
			}
			item := iter.Iterator.values[iter.Iterator.index]
			iter.Iterator.index++
			f.push(item)
		case OpUnpackStore:
			value := f.pop()
			items, err := unpackValues(value, len(instr.Names))
			if err != nil {
				return NoneValue(), err
			}
			for i, name := range instr.Names {
				if i < len(instr.Types) {
					items[i], err = validateValue(items[i], instr.Types[i])
					if err != nil {
						return NoneValue(), err
					}
				}
				if idx, ok := f.fn.LocalIndex[name]; ok && f.fn.Name != "<module>" {
					f.locals[idx] = items[i]
				} else {
					f.module.Globals[name] = items[i]
				}
			}
		case OpDefineFunc:
			fnCopy := *instr.Fn
			fnCopy.Module = f.module
			f.module.Globals[instr.S] = Value{Kind: ValueFunction, Function: &fnCopy}
		case OpDefineClass:
			class := runtimeClass(instr.Class, f.module)
			f.module.Globals[instr.Class.Name] = Value{Kind: ValueClass, Class: class}
		case OpImportModule:
			if module, ok := nativePythonModule(instr.S); ok {
				f.module.Globals[instr.Names[0]] = module
				continue
			}
			bridge, err := vm.engine.PythonBridge()
			if err != nil {
				return NoneValue(), err
			}
			module, err := bridge.ImportModule(instr.S)
			if err != nil {
				return NoneValue(), err
			}
			f.module.Globals[instr.Names[0]] = module
		case OpImportFrom:
			if module, ok := nativePythonModule(instr.S); ok {
				for _, name := range instr.Names {
					value, ok := module.Module.Exports[name]
					if !ok {
						return NoneValue(), fmt.Errorf("module %q has no export %q", instr.S, name)
					}
					f.module.Globals[name] = value
				}
				continue
			}
			if isPythonImport(instr.S) {
				bridge, err := vm.engine.PythonBridge()
				if err != nil {
					return NoneValue(), err
				}
				module, err := bridge.ImportModule(instr.S)
				if err != nil {
					return NoneValue(), err
				}
				pyModule := module.PyObject
				for _, name := range instr.Names {
					value, err := bridge.GetAttr(pyModule, name)
					if err != nil {
						return NoneValue(), err
					}
					f.module.Globals[name] = value
				}
				continue
			}
			imported, err := vm.engine.loadModuleByNameLocked(instr.S, filepathDir(f.module.Path))
			if err != nil {
				if !errors.Is(err, ErrModuleNotFound) {
					return NoneValue(), err
				}
				bridge, bridgeErr := vm.engine.PythonBridge()
				if bridgeErr != nil {
					return NoneValue(), err
				}
				module, importErr := bridge.ImportModule(instr.S)
				if importErr != nil {
					return NoneValue(), err
				}
				pyModule := module.PyObject
				for _, name := range instr.Names {
					value, attrErr := bridge.GetAttr(pyModule, name)
					if attrErr != nil {
						return NoneValue(), attrErr
					}
					f.module.Globals[name] = value
				}
				continue
			}
			for _, name := range instr.Names {
				value, ok := imported.Globals[name]
				if !ok {
					return NoneValue(), fmt.Errorf("module %q has no export %q", instr.S, name)
				}
				f.module.Globals[name] = value
			}
		case OpString:
			value := f.pop()
			if instr.S == ".2f" && value.Kind != ValueInt && value.Kind != ValueFloat {
				return NoneValue(), fmt.Errorf(".2f format requires a number")
			}
			f.push(StringValue(formatValue(value, instr.S)))
		}
	}
	return NoneValue(), nil
}

func filepathDir(path string) string {
	idx := strings.LastIndexAny(path, `/\`)
	if idx < 0 {
		return "."
	}
	return path[:idx]
}

func runtimeClass(spec *ClassSpec, module *LoadedModule) *RuntimeClass {
	fieldIndex := map[string]int{}
	for i, field := range spec.Fields {
		fieldIndex[field] = i
	}
	for _, method := range spec.Methods {
		method.Module = module
	}
	return &RuntimeClass{Spec: spec, FieldIndex: fieldIndex}
}

func (vm *VM) callValue(callee Value, args []Value, kwargs map[string]Value) (Value, error) {
	if err := vm.context().Err(); err != nil {
		return NoneValue(), err
	}
	switch callee.Kind {
	case ValueFunction:
		if len(kwargs) > 0 {
			return NoneValue(), fmt.Errorf("%s does not accept keyword arguments", callee.Function.Name)
		}
		return vm.runFunction(callee.Function, args)
	case ValueNative:
		if len(kwargs) > 0 {
			return NoneValue(), fmt.Errorf("%s does not accept keyword arguments", callee.Native.Name)
		}
		if callee.Native.Signature.Kind == TypeFunc {
			return callNativeDriver(vm, callee.Native, args)
		}
		return callee.Native.Fn(vm, args)
	case ValueClass:
		if len(kwargs) > 0 {
			return NoneValue(), fmt.Errorf("%s does not accept keyword arguments", callee.Class.Spec.Name)
		}
		return vm.callClass(callee.Class, args)
	case ValueBoundMethod:
		if len(kwargs) > 0 {
			return NoneValue(), fmt.Errorf("Typhon methods do not accept keyword arguments")
		}
		if callee.Bound.Native != nil {
			nativeArgs := append([]Value{callee.Bound.Receiver}, args...)
			return callee.Bound.Native.Fn(vm, nativeArgs)
		}
		methodArgs := append([]Value{callee.Bound.Receiver}, args...)
		return vm.runFunction(callee.Bound.Function, methodArgs)
	case ValuePyObject:
		return callee.PyObject.Bridge.Call(callee.PyObject, args, kwargs)
	default:
		return NoneValue(), fmt.Errorf("%s is not callable", callee.String())
	}
}

func (vm *VM) callClass(class *RuntimeClass, args []Value) (Value, error) {
	fields := make([]Value, len(class.Spec.Fields))
	object := Value{Kind: ValueObject, Object: &Object{Class: class, Fields: fields}}
	if initFn, ok := class.Spec.Methods["__init__"]; ok {
		_, err := vm.runFunction(initFn, append([]Value{object}, args...))
		if err != nil {
			return NoneValue(), err
		}
	}
	return object, nil
}

func (vm *VM) getAttr(value Value, name string) (Value, error) {
	switch value.Kind {
	case ValueNativeObject:
		if value.NativeObject.Owner != nil && value.NativeObject.Owner != vm.engine {
			return NoneValue(), fmt.Errorf("native object belongs to another Engine")
		}
		if attr, ok := value.NativeObject.Attrs[name]; ok {
			return attr, nil
		}
		return NoneValue(), fmt.Errorf("%s has no attribute %q", value.NativeObject.Type.Name, name)
	case ValueObject:
		if idx, ok := value.Object.Class.FieldIndex[name]; ok {
			if value.Object.Fields[idx].Kind == "" {
				return NoneValue(), fmt.Errorf("field %s.%s is not initialized", value.Object.Class.Spec.Name, name)
			}
			return value.Object.Fields[idx], nil
		}
		if method, ok := value.Object.Class.Spec.Methods[name]; ok {
			return Value{Kind: ValueBoundMethod, Bound: &BoundMethod{Receiver: value, Function: method}}, nil
		}
		return NoneValue(), fmt.Errorf("%s has no attribute %q", value.Object.Class.Spec.Name, name)
	case ValueString:
		switch name {
		case "strip":
			return boundNative(value, "str.strip", nativeStringStrip), nil
		case "title":
			return boundNative(value, "str.title", nativeStringTitle), nil
		case "upper":
			return boundNative(value, "str.upper", nativeStringUpper), nil
		}
	case ValueList:
		if name == "append" {
			return boundNative(value, "list.append", nativeListAppend), nil
		}
	case ValueDict:
		if method, ok := dictMethod(value, name); ok {
			return method, nil
		}
	case ValueNativeModule:
		if exported, ok := value.Module.Exports[name]; ok {
			return exported, nil
		}
		return NoneValue(), fmt.Errorf("module %q has no attribute %q", value.Module.Name, name)
	case ValuePyObject:
		return value.PyObject.Bridge.GetAttr(value.PyObject, name)
	}
	return NoneValue(), fmt.Errorf("%s has no attribute %q", value.String(), name)
}

func (vm *VM) setAttr(value Value, name string, newValue Value) error {
	if value.Kind != ValueObject {
		return fmt.Errorf("cannot set attribute %q on %s", name, value.String())
	}
	idx, ok := value.Object.Class.FieldIndex[name]
	if !ok {
		return fmt.Errorf("%s has no field %q", value.Object.Class.Spec.Name, name)
	}
	if info := value.Object.Class.Spec.Info; info != nil {
		var err error
		newValue, err = validateValue(newValue, info.Fields[name])
		if err != nil {
			return fmt.Errorf("field %s: %w", name, err)
		}
	}
	value.Object.Fields[idx] = newValue
	return nil
}

func boundNative(receiver Value, name string, fn func(*VM, []Value) (Value, error)) Value {
	native := &NativeFunc{Name: name, Receiver: &receiver, Fn: fn}
	return Value{Kind: ValueBoundMethod, Bound: &BoundMethod{Receiver: receiver, Native: native}}
}

func nativePrint(vm *VM, args []Value) (Value, error) {
	parts := make([]string, 0, len(args))
	for _, arg := range args {
		parts = append(parts, arg.String())
	}
	fmt.Println(strings.Join(parts, " "))
	return NoneValue(), nil
}

func nativeSpawn(vm *VM, args []Value) (Value, error) {
	if len(args) == 0 {
		return NoneValue(), fmt.Errorf("spawn expected a function")
	}
	if len(vm.engine.tasks) >= 256 {
		return NoneValue(), fmt.Errorf("task quota exceeded")
	}
	callee := args[0]
	module := vm.module
	callArgs := append([]Value(nil), args[1:]...)
	ctx, cancel := context.WithCancel(vm.context())
	task := &Task{done: make(chan struct{}), cancel: cancel}
	vm.engine.tasks = append(vm.engine.tasks, task)
	go func() {
		defer cancel()
		vm.engine.execMu.Lock()
		defer vm.engine.execMu.Unlock()
		child := &VM{engine: vm.engine, module: module, depth: 1, ctx: ctx, resources: vm.resources}
		value, err := child.callValue(callee, callArgs, nil)
		task.result = taskResult{value: value, err: err}
		close(task.done)
	}()
	return Value{Kind: ValueTask, Task: task}, nil
}

func nativeJoin(vm *VM, args []Value) (Value, error) {
	if len(args) < 1 || len(args) > 2 || args[0].Kind != ValueTask || args[0].Task == nil {
		return NoneValue(), fmt.Errorf("join expected a task")
	}
	timeout := 30 * time.Second
	if len(args) == 2 {
		if args[1].Kind != ValueInt && args[1].Kind != ValueFloat {
			return NoneValue(), fmt.Errorf("join timeout must be a positive number of seconds")
		}
		seconds := numberValue(args[1])
		if seconds <= 0 || math.IsInf(seconds, 0) || math.IsNaN(seconds) || seconds > 86400 {
			return NoneValue(), fmt.Errorf("join timeout must be between 0 and 86400 seconds")
		}
		timeout = time.Duration(seconds * float64(time.Second))
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	vm.engine.execMu.Unlock()
	var waitErr error
	select {
	case <-args[0].Task.done:
	case <-vm.context().Done():
		waitErr = vm.context().Err()
	case <-timer.C:
		waitErr = fmt.Errorf("join timed out after %s; execution cancelled", timeout)
		vm.engine.cancel()
		vm.engine.bridgeMu.Lock()
		bridge := vm.engine.bridge
		vm.engine.bridgeMu.Unlock()
		if bridge != nil {
			_ = bridge.Close()
		}
	}
	vm.engine.execMu.Lock()
	if waitErr != nil {
		return NoneValue(), waitErr
	}
	result := args[0].Task.result
	return result.value, result.err
}

func (vm *VM) context() context.Context {
	if vm.ctx != nil {
		return vm.ctx
	}
	return vm.engine.ctx
}

func nativeCancel(vm *VM, args []Value) (Value, error) {
	if len(args) != 1 || args[0].Kind != ValueTask || args[0].Task == nil {
		return NoneValue(), fmt.Errorf("cancel expected a task")
	}
	task := args[0].Task
	task.cancelled = true
	if task.cancel != nil {
		task.cancel()
	}
	return NoneValue(), nil
}

func nativeEnumerate(vm *VM, args []Value) (Value, error) {
	if len(args) != 1 {
		return NoneValue(), fmt.Errorf("enumerate expected 1 argument")
	}
	values, err := collectionValues(args[0])
	if err != nil {
		return NoneValue(), err
	}
	items := make([]Value, 0, len(values))
	for index, item := range values {
		items = append(items, ListValue([]Value{IntValue(int64(index)), item}))
	}
	return ListValue(items), nil
}

func nativeRange(vm *VM, args []Value) (Value, error) {
	if len(args) < 1 || len(args) > 3 {
		return NoneValue(), fmt.Errorf("range expected 1 to 3 arguments, got %d", len(args))
	}
	for _, arg := range args {
		if arg.Kind != ValueInt {
			return NoneValue(), fmt.Errorf("range expected int arguments")
		}
	}

	start := int64(0)
	stop := args[0].Int
	step := int64(1)
	if len(args) >= 2 {
		start = args[0].Int
		stop = args[1].Int
	}
	if len(args) == 3 {
		step = args[2].Int
	}
	if step == 0 {
		return NoneValue(), fmt.Errorf("range step must not be zero")
	}

	items := []Value{}
	if step > 0 {
		for value := start; value < stop; value += step {
			if len(items) >= maxCollectionItems {
				return NoneValue(), fmt.Errorf("range collection size limit exceeded")
			}
			items = append(items, IntValue(value))
			if value > math.MaxInt64-step {
				break
			}
		}
	} else {
		for value := start; value > stop; value += step {
			if len(items) >= maxCollectionItems {
				return NoneValue(), fmt.Errorf("range collection size limit exceeded")
			}
			items = append(items, IntValue(value))
			if value < math.MinInt64-step {
				break
			}
		}
	}
	return ListValue(items), nil
}

func nativeStringStrip(vm *VM, args []Value) (Value, error) {
	if len(args) != 1 {
		return NoneValue(), fmt.Errorf("str.strip expected no arguments")
	}
	receiver := vmReceiver(args)
	return StringValue(strings.TrimSpace(receiver.Str)), nil
}

func nativeStringTitle(vm *VM, args []Value) (Value, error) {
	if len(args) != 1 {
		return NoneValue(), fmt.Errorf("str.title expected no arguments")
	}
	receiver := vmReceiver(args)
	return StringValue(titleString(receiver.Str)), nil
}

func nativeStringUpper(vm *VM, args []Value) (Value, error) {
	if len(args) != 1 {
		return NoneValue(), fmt.Errorf("str.upper expected no arguments")
	}
	receiver := vmReceiver(args)
	return StringValue(strings.ToUpper(receiver.Str)), nil
}

func nativeListAppend(vm *VM, args []Value) (Value, error) {
	if len(args) != 2 {
		return NoneValue(), fmt.Errorf("list.append expected 1 argument, got %d", len(args)-1)
	}
	receiver := vmReceiver(args)
	if receiver.List == nil {
		return NoneValue(), fmt.Errorf("list.append receiver is nil")
	}
	if len(*receiver.List) >= maxCollectionItems {
		return NoneValue(), fmt.Errorf("list size limit exceeded")
	}
	item := args[1]
	if receiver.ListGuard != nil && receiver.ListGuard.elem != nil {
		var err error
		item, err = validateValue(item, *receiver.ListGuard.elem)
		if err != nil {
			return NoneValue(), err
		}
	}
	*receiver.List = append(*receiver.List, item)
	return NoneValue(), nil
}

func vmReceiver(args []Value) Value {
	if len(args) > 0 {
		return args[0]
	}
	return Value{}
}

func titleString(value string) string {
	words := strings.Fields(value)
	for i, word := range words {
		if word == "" {
			continue
		}
		runes := []rune(strings.ToLower(word))
		runes[0] = []rune(strings.ToUpper(string(runes[0])))[0]
		words[i] = string(runes)
	}
	return strings.Join(words, " ")
}

func numberValue(value Value) float64 {
	if value.Kind == ValueFloat {
		return value.Float
	}
	return float64(value.Int)
}

func unpackValues(value Value, expected int) ([]Value, error) {
	if value.Kind != ValueList || value.List == nil {
		return nil, fmt.Errorf("cannot unpack %s", value.String())
	}
	if len(*value.List) != expected {
		return nil, fmt.Errorf("unpack expected %d values, got %d", expected, len(*value.List))
	}
	return *value.List, nil
}

func formatValue(value Value, format string) string {
	if format == ".2f" {
		return fmt.Sprintf("%.2f", numberValue(value))
	}
	return value.String()
}

func (f *frame) push(value Value) {
	f.stack = append(f.stack, value)
}

func (f *frame) pop() Value {
	if len(f.stack) == 0 {
		return NoneValue()
	}
	value := f.stack[len(f.stack)-1]
	f.stack = f.stack[:len(f.stack)-1]
	return value
}

func (f *frame) peek() Value {
	if len(f.stack) == 0 {
		return NoneValue()
	}
	return f.stack[len(f.stack)-1]
}
