package typhon

import "fmt"

func isNativePythonModule(name string) bool {
	if _, ok := nativeDriverInfo(name); ok {
		return true
	}
	switch trimPythonNamespace(name) {
	case "typhon.python":
		return true
	default:
		return false
	}
}

func nativePythonModule(name string) (Value, bool) {
	if module, ok := nativeDriverModule(name); ok {
		return module, true
	}
	switch trimPythonNamespace(name) {
	case "typhon.python":
		return Value{Kind: ValueNativeModule, Module: &NativeModule{Name: name, Exports: map[string]Value{
			"release": nativeValue("python.release", nativePythonRelease),
		}}}, true
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
