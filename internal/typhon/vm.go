package typhon

import (
	"fmt"
	"strings"
)

type VM struct {
	engine *Engine
	module *LoadedModule
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
	globals["spawn"] = nativeValue("spawn", nativeSpawn)
	globals["join"] = nativeValue("join", nativeJoin)
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

func (vm *VM) runFunction(fn *Function, args []Value) (Value, error) {
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
	for i := range locals {
		locals[i] = NoneValue()
	}
	for i, arg := range args {
		locals[i] = arg
	}
	f := &frame{fn: fn, locals: locals, module: vm.module}

	for f.pc < len(fn.Code) {
		instr := fn.Code[f.pc]
		f.pc++
		switch instr.Op {
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
			f.push(f.locals[instr.A])
		case OpStoreLocal:
			f.locals[instr.A] = f.pop()
		case OpPop:
			_ = f.pop()
		case OpAdd:
			right := f.pop()
			left := f.pop()
			if left.Kind == ValueString || right.Kind == ValueString {
				f.push(StringValue(left.String() + right.String()))
			} else if left.Kind == ValueFloat || right.Kind == ValueFloat {
				f.push(FloatValue(numberValue(left) + numberValue(right)))
			} else {
				f.push(IntValue(left.Int + right.Int))
			}
		case OpSub:
			right := f.pop()
			left := f.pop()
			if left.Kind == ValueFloat || right.Kind == ValueFloat {
				f.push(FloatValue(numberValue(left) - numberValue(right)))
			} else {
				f.push(IntValue(left.Int - right.Int))
			}
		case OpMul:
			right := f.pop()
			left := f.pop()
			if left.Kind == ValueString && right.Kind == ValueInt {
				f.push(StringValue(strings.Repeat(left.Str, int(right.Int))))
			} else if left.Kind == ValueInt && right.Kind == ValueString {
				f.push(StringValue(strings.Repeat(right.Str, int(left.Int))))
			} else if left.Kind == ValueFloat || right.Kind == ValueFloat {
				f.push(FloatValue(numberValue(left) * numberValue(right)))
			} else {
				f.push(IntValue(left.Int * right.Int))
			}
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
			f.push(BoolValue(valuesEqual(left, right)))
		case OpIsNot:
			right := f.pop()
			left := f.pop()
			f.push(BoolValue(!valuesEqual(left, right)))
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
			return f.pop(), nil
		case OpMakeList:
			items := make([]Value, instr.A)
			for i := instr.A - 1; i >= 0; i-- {
				items[i] = f.pop()
			}
			f.push(ListValue(items))
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
			if iterable.Kind != ValueList || iterable.List == nil {
				return NoneValue(), fmt.Errorf("for-loop expected list, got %s", iterable.Kind)
			}
			f.push(Value{Kind: ValueIterator, Iterator: &Iterator{values: *iterable.List}})
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
			imported, err := vm.engine.LoadModuleByName(instr.S, filepathDir(f.module.Path))
			if err != nil {
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
			f.push(StringValue(formatValue(f.pop(), instr.S)))
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
		return callee.Native.Fn(vm, args)
	case ValueClass:
		if len(kwargs) > 0 {
			return NoneValue(), fmt.Errorf("%s does not accept keyword arguments", callee.Class.Spec.Name)
		}
		return vm.callClass(callee.Class, args)
	case ValueBoundMethod:
		if len(kwargs) > 0 && callee.Bound.Native == nil {
			return NoneValue(), fmt.Errorf("%s does not accept keyword arguments", callee.Bound.Function.Name)
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
	for i := range fields {
		fields[i] = NoneValue()
	}
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
	case ValueObject:
		if idx, ok := value.Object.Class.FieldIndex[name]; ok {
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
	callee := args[0]
	callArgs := append([]Value(nil), args[1:]...)
	task := &Task{done: make(chan taskResult, 1)}
	go func() {
		child := &VM{engine: vm.engine, module: vm.module}
		value, err := child.callValue(callee, callArgs, nil)
		task.done <- taskResult{value: value, err: err}
	}()
	return Value{Kind: ValueTask, Task: task}, nil
}

func nativeJoin(vm *VM, args []Value) (Value, error) {
	if len(args) != 1 || args[0].Kind != ValueTask || args[0].Task == nil {
		return NoneValue(), fmt.Errorf("join expected a task")
	}
	result := <-args[0].Task.done
	return result.value, result.err
}

func nativeEnumerate(vm *VM, args []Value) (Value, error) {
	if len(args) != 1 || args[0].Kind != ValueList || args[0].List == nil {
		return NoneValue(), fmt.Errorf("enumerate expected a list")
	}
	items := make([]Value, 0, len(*args[0].List))
	for index, item := range *args[0].List {
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
			items = append(items, IntValue(value))
		}
	} else {
		for value := start; value > stop; value += step {
			items = append(items, IntValue(value))
		}
	}
	return ListValue(items), nil
}

func nativeStringStrip(vm *VM, args []Value) (Value, error) {
	receiver := vmReceiver(args)
	return StringValue(strings.TrimSpace(receiver.Str)), nil
}

func nativeStringTitle(vm *VM, args []Value) (Value, error) {
	receiver := vmReceiver(args)
	return StringValue(titleString(receiver.Str)), nil
}

func nativeStringUpper(vm *VM, args []Value) (Value, error) {
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
	*receiver.List = append(*receiver.List, args[1])
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
