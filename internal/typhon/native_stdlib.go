package typhon

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

func isNativePythonModule(name string) bool {
	switch trimPythonNamespace(name) {
	case "json":
		return true
	default:
		return false
	}
}

func nativePythonModule(name string) (Value, bool) {
	switch trimPythonNamespace(name) {
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
	switch value.Kind {
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
			rendered, err := renderPythonJSON(item)
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
