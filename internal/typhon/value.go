package typhon

import (
	"context"
	"fmt"
	"strings"
)

type ValueKind string

const (
	ValueNone         ValueKind = "none"
	ValueInt          ValueKind = "int"
	ValueFloat        ValueKind = "float"
	ValueString       ValueKind = "str"
	ValueBool         ValueKind = "bool"
	ValueList         ValueKind = "list"
	ValueDict         ValueKind = "dict"
	ValueFunction     ValueKind = "function"
	ValueNative       ValueKind = "native"
	ValueNativeModule ValueKind = "native_module"
	ValueClass        ValueKind = "class"
	ValueObject       ValueKind = "object"
	ValueBoundMethod  ValueKind = "bound_method"
	ValueTask         ValueKind = "task"
	ValueIterator     ValueKind = "iterator"
	ValuePyObject     ValueKind = "pyobject"
	ValueNativeObject ValueKind = "native_object"
)

type Value struct {
	NativeObject *NativeObject
	ListGuard    *listGuard
	Kind         ValueKind
	Int          int64
	Float        float64
	Str          string
	Bool         bool
	List         *[]Value
	Dict         *Dictionary
	Function     *Function
	Native       *NativeFunc
	Module       *NativeModule
	Class        *RuntimeClass
	Object       *Object
	Bound        *BoundMethod
	Task         *Task
	Iterator     *Iterator
	PyObject     *PyObject
}

type NativeFunc struct {
	Signature Type
	Name      string
	Receiver  *Value
	Fn        func(*VM, []Value) (Value, error)
}

type NativeObject struct {
	data  any
	Type  Type
	Owner *Engine
	Attrs map[string]Value
}

type NativeModule struct {
	Name    string
	Exports map[string]Value
}

type RuntimeClass struct {
	Spec       *ClassSpec
	FieldIndex map[string]int
}

type Object struct {
	Class  *RuntimeClass
	Fields []Value
}

type BoundMethod struct {
	Receiver Value
	Function *Function
	Native   *NativeFunc
}

type Task struct {
	cancel    context.CancelFunc
	cancelled bool
	done      chan struct{}
	result    taskResult
}

type listGuard struct{ elem *Type }

type taskResult struct {
	value Value
	err   error
}

type Iterator struct {
	values []Value
	index  int
}

func NoneValue() Value {
	return Value{Kind: ValueNone}
}

func IntValue(value int64) Value {
	return Value{Kind: ValueInt, Int: value}
}

func FloatValue(value float64) Value {
	return Value{Kind: ValueFloat, Float: value}
}

func StringValue(value string) Value {
	return Value{Kind: ValueString, Str: value}
}

func BoolValue(value bool) Value {
	return Value{Kind: ValueBool, Bool: value}
}

func ListValue(values []Value) Value {
	items := append([]Value(nil), values...)
	return Value{Kind: ValueList, List: &items, ListGuard: &listGuard{}}
}

func (v Value) String() string {
	return v.stringValue(0, map[any]bool{})
}

func (v Value) stringValue(depth int, seen map[any]bool) string {
	if depth > 64 {
		return "<depth limit>"
	}
	switch v.Kind {
	case ValueNativeObject:
		return "<" + v.NativeObject.Type.Name + ">"
	case ValueNone:
		return "None"
	case ValueInt:
		return fmt.Sprintf("%d", v.Int)
	case ValueFloat:
		return fmt.Sprintf("%g", v.Float)
	case ValueString:
		return v.Str
	case ValueBool:
		if v.Bool {
			return "True"
		}
		return "False"
	case ValueList:
		if v.List == nil {
			return "[]"
		}
		if seen[v.List] {
			return "[...]"
		}
		seen[v.List] = true
		defer delete(seen, v.List)
		parts := make([]string, 0, len(*v.List))
		for _, item := range *v.List {
			parts = append(parts, item.stringValue(depth+1, seen))
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case ValueDict:
		if v.Dict == nil {
			return "{}"
		}
		if seen[v.Dict] {
			return "{...}"
		}
		seen[v.Dict] = true
		defer delete(seen, v.Dict)
		parts := make([]string, len(v.Dict.Entries))
		for i, entry := range v.Dict.Entries {
			key, value := entry.Key.stringValue(depth+1, seen), entry.Value.stringValue(depth+1, seen)
			if entry.Key.Kind == ValueString {
				key = fmt.Sprintf("%q", entry.Key.Str)
			}
			if entry.Value.Kind == ValueString {
				value = fmt.Sprintf("%q", entry.Value.Str)
			}
			parts[i] = key + ": " + value
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case ValueFunction:
		return "<function " + v.Function.Name + ">"
	case ValueNative:
		return "<native " + v.Native.Name + ">"
	case ValueNativeModule:
		return "<module " + v.Module.Name + ">"
	case ValueClass:
		return "<class " + v.Class.Spec.Name + ">"
	case ValueObject:
		return "<" + v.Object.Class.Spec.Name + " object>"
	case ValueBoundMethod:
		if v.Bound.Function != nil {
			return "<bound method " + v.Bound.Function.Name + ">"
		}
		return "<bound native " + v.Bound.Native.Name + ">"
	case ValueTask:
		return "<task>"
	case ValueIterator:
		return "<iterator>"
	case ValuePyObject:
		if v.PyObject == nil || v.PyObject.Repr == "" {
			return "<pyobject>"
		}
		return v.PyObject.Repr
	default:
		return "<value>"
	}
}

func truthy(value Value) bool {
	switch value.Kind {
	case ValueNone:
		return false
	case ValueBool:
		return value.Bool
	case ValueInt:
		return value.Int != 0
	case ValueFloat:
		return value.Float != 0
	case ValueString:
		return value.Str != ""
	case ValueList:
		return value.List != nil && len(*value.List) > 0
	case ValueDict:
		return value.Dict != nil && len(value.Dict.Entries) > 0
	default:
		return true
	}
}

func valuesEqual(left Value, right Value) bool {
	return equalValues(left, right, map[[2]any]bool{})
}

func equalValues(left Value, right Value, seen map[[2]any]bool) bool {
	if left.Kind == ValueInt && right.Kind == ValueFloat || left.Kind == ValueFloat && right.Kind == ValueInt {
		cmp, err := compareValues(left, right)
		return err == nil && cmp == 0
	}
	if left.Kind != right.Kind {
		return false
	}
	switch left.Kind {
	case ValueDict:
		if left.Dict == right.Dict {
			return true
		}
		if left.Dict == nil || right.Dict == nil || len(left.Dict.Entries) != len(right.Dict.Entries) {
			return false
		}
		pair := [2]any{left.Dict, right.Dict}
		if seen[pair] {
			return true
		}
		seen[pair] = true
		for _, entry := range left.Dict.Entries {
			i, ok, err := right.Dict.lookup(entry.Key)
			if err != nil || !ok || !equalValues(entry.Value, right.Dict.Entries[i].Value, seen) {
				return false
			}
		}
		return true
	case ValueList:
		if left.List == right.List {
			return true
		}
		if left.List == nil || right.List == nil {
			return false
		}
		pair := [2]any{left.List, right.List}
		if seen[pair] {
			return true
		}
		seen[pair] = true
		if len(*left.List) != len(*right.List) {
			return false
		}
		for i, item := range *left.List {
			if !equalValues(item, (*right.List)[i], seen) {
				return false
			}
		}
		return true
	case ValueNone:
		return true
	case ValueInt:
		return left.Int == right.Int
	case ValueFloat:
		return left.Float == right.Float
	case ValueString:
		return left.Str == right.Str
	case ValueBool:
		return left.Bool == right.Bool
	case ValueObject:
		return left.Object == right.Object
	case ValueNativeObject:
		return left.NativeObject == right.NativeObject
	case ValueClass:
		return left.Class == right.Class
	default:
		return false
	}
}

func valuesIdentical(left, right Value) bool {
	if left.Kind != right.Kind {
		return false
	}
	switch left.Kind {
	case ValueDict:
		return left.Dict == right.Dict
	case ValueList:
		return left.List == right.List
	case ValueTask:
		return left.Task == right.Task
	case ValueFunction:
		return left.Function == right.Function
	case ValueNative:
		return left.Native == right.Native
	case ValueNativeModule:
		return left.Module == right.Module
	case ValuePyObject:
		return left.PyObject != nil && right.PyObject != nil && left.PyObject.Bridge == right.PyObject.Bridge && left.PyObject.ID == right.PyObject.ID
	default:
		return valuesEqual(left, right)
	}
}
