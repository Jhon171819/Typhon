package typhon

import (
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
	ValueFunction     ValueKind = "function"
	ValueNative       ValueKind = "native"
	ValueNativeModule ValueKind = "native_module"
	ValueClass        ValueKind = "class"
	ValueObject       ValueKind = "object"
	ValueBoundMethod  ValueKind = "bound_method"
	ValueTask         ValueKind = "task"
	ValueIterator     ValueKind = "iterator"
	ValuePyObject     ValueKind = "pyobject"
)

type Value struct {
	Kind     ValueKind
	Int      int64
	Float    float64
	Str      string
	Bool     bool
	List     *[]Value
	Function *Function
	Native   *NativeFunc
	Module   *NativeModule
	Class    *RuntimeClass
	Object   *Object
	Bound    *BoundMethod
	Task     *Task
	Iterator *Iterator
	PyObject *PyObject
}

type NativeFunc struct {
	Name     string
	Receiver *Value
	Fn       func(*VM, []Value) (Value, error)
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
	done chan taskResult
}

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
	return Value{Kind: ValueList, List: &items}
}

func (v Value) String() string {
	switch v.Kind {
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
		parts := make([]string, 0, len(*v.List))
		for _, item := range *v.List {
			parts = append(parts, item.String())
		}
		return "[" + strings.Join(parts, ", ") + "]"
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
	default:
		return true
	}
}

func valuesEqual(left Value, right Value) bool {
	if left.Kind != right.Kind {
		return false
	}
	switch left.Kind {
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
	case ValueClass:
		return left.Class == right.Class
	default:
		return false
	}
}
