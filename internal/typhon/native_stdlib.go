package typhon

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

func isNativePythonModule(name string) bool {
	switch trimPythonNamespace(name) {
	case "typhon.python":
		return true
	case "json":
		return true
	default:
		return false
	}
}

func nativePythonModule(name string) (Value, bool) {
	switch trimPythonNamespace(name) {
	case "typhon.python":
		return Value{Kind: ValueNativeModule, Module: &NativeModule{Name: name, Exports: map[string]Value{
			"release": nativeValue("python.release", nativePythonRelease),
		}}}, true
	case "json":
		return Value{
			Kind: ValueNativeModule,
			Module: &NativeModule{
				Name: "json",
				Exports: map[string]Value{
					"dumps": nativeValue("json.dumps", nativeJSONDumps),
				},
			},
		}, true
	default:
		return NoneValue(), false
	}
}

func nativePythonRelease(vm *VM, args []Value) (Value, error) {
	if len(args) != 1 || args[0].Kind != ValuePyObject || args[0].PyObject == nil {
		return NoneValue(), fmt.Errorf("python.release expected a Python handle")
	}
	return NoneValue(), args[0].PyObject.Bridge.Release(args[0].PyObject)
}

func nativeJSONDumps(vm *VM, args []Value) (Value, error) {
	if len(args) != 1 {
		return NoneValue(), fmt.Errorf("json.dumps expected 1 argument, got %d", len(args))
	}
	rendered, err := renderPythonJSON(args[0])
	if err != nil {
		return NoneValue(), err
	}
	return StringValue(rendered), nil
}

func renderPythonJSON(value Value) (string, error) {
	return renderJSONDepth(value, 0)
}

func renderJSONDepth(value Value, depth int) (string, error) {
	if depth > 64 {
		return "", fmt.Errorf("JSON depth limit exceeded (possibly circular collection)")
	}
	switch value.Kind {
	case ValueFloat:
		if math.IsInf(value.Float, 0) || math.IsNaN(value.Float) {
			return "", fmt.Errorf("JSON does not support non-finite numbers")
		}
		return strconv.FormatFloat(value.Float, 'f', -1, 64), nil
	case ValueNone:
		return "null", nil
	case ValueBool:
		if value.Bool {
			return "true", nil
		}
		return "false", nil
	case ValueInt:
		return strconv.FormatInt(value.Int, 10), nil
	case ValueString:
		data, err := json.Marshal(value.Str)
		if err != nil {
			return "", err
		}
		return string(data), nil
	case ValueList:
		if value.List == nil {
			return "[]", nil
		}
		parts := make([]string, 0, len(*value.List))
		for _, item := range *value.List {
			rendered, err := renderJSONDepth(item, depth+1)
			if err != nil {
				return "", err
			}
			parts = append(parts, rendered)
		}
		return "[" + strings.Join(parts, ", ") + "]", nil
	default:
		return "", fmt.Errorf("json.dumps does not support %s yet", value.Kind)
	}
}
