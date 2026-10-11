package typhon

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func sourceFile(t *testing.T, source string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "regression.ty")
	if err := os.WriteFile(path, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAuditStaticRegressions(t *testing.T) {
	cases := []struct{ name, source, diagnostic string }{
		{"P0_01_missing_return", "def f() -> int:\n    print(1)\n", "must return int"},
		{"conditional_return", "def f(x: bool) -> int:\n    if x:\n        return 1\n", "every path"},
		{"P0_02_uninitialized", "def f() -> int:\n    x: int\n    return x\n", "not initialized"},
		{"branch_initialization", "def f(b: bool) -> int:\n    x: int\n    if b:\n        x = 1\n    return x\n", "not initialized"},
		{"shadow_global", "x: int = 1\ndef f() -> int:\n    print(x)\n    x: int = 2\n    return x\n", "not initialized"},
		{"P0_03_heterogeneous_list", "x: list[int] = [1, 'wrong']\n", "list element expected int"},
		{"nested_list", "x: list[list[int]] = [[1], ['wrong']]\n", "list element expected int"},
		{"P0_04_string_subtraction", "x: str = 'a' - 'b'\n", "operator -"},
		{"invalid_string_addition", "x: str = 'a' + 1\n", "operator +"},
		{"literal_alias", "type State = 'open' | 'closed'\nx: State = 'wrong'\n", "assignment expected"},
		{"list_invariance", "a: list[int] = [1]\nb: list[float] = a\n", "assignment expected"},
		{"unknown_attribute", "x: int = 1\nx.missing()\n", "has no attribute"},
		{"fstring_check", "x: str = f'{unknown}'\n", "unknown name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckFile(sourceFile(t, tc.source))
			if err == nil || !strings.Contains(err.Error(), tc.diagnostic) {
				t.Fatalf("want %q, got %v", tc.diagnostic, err)
			}
		})
	}
}

func TestAuditRuntimeRegressions(t *testing.T) {
	cases := []struct{ name, source, output, diagnostic string }{
		{"initialized_local", "def f() -> int:\n    x: int\n    x = 42\n    return x\nprint(f())\n", "42\n", ""},
		{"P0_08_repeated_join", "def f() -> int:\n    return 42\nt: TyphonTask = spawn(f)\nprint(join(t))\nprint(join(t))\n", "42\n42\n", ""},
		{"P0_09_negative_repeat", "count: int = 0 - 1\nprint('x' * count)\n", "", "must not be negative"},
		{"list_identity_equality", "a: list[int] = [1]\nb: list[int] = [1]\nprint(a is a, a is b, a == b)\n", "True False True\n", ""},
		{"overflow_add", "print(9223372036854775807 + 1)\n", "", "int64 overflow"},
		{"overflow_sub", "print((0 - 9223372036854775807 - 1) - 1)\n", "", "int64 overflow"},
		{"overflow_mul", "print(9223372036854775807 * 2)\n", "", "int64 overflow"},
		{"range_wrap", "print(range(9223372036854775806, 9223372036854775807, 2))\n", "[9223372036854775806]\n", ""},
		{"list_dynamic_alias", "a: list[int] = [1]\nb: Any = a\nb.append('wrong')\n", "", "expected int"},
		{"dynamic_argument", "def f(x: int) -> int:\n    return x\nv: Any = 'wrong'\nf(v)\n", "", "argument x"},
		{"dynamic_return", "def f(x: Any) -> int:\n    return x\nf('wrong')\n", "", "expected int"},
		{"depth_budget", "def f() -> int:\n    return f()\nf()\n", "", "call depth limit"},
		{"child_error", "def f() -> int:\n    return 9223372036854775807 + 1\nt: TyphonTask = spawn(f)\n", "", "child task"},
		{"wait_for_child", "def f() -> None:\n    print('child completed')\nt: TyphonTask = spawn(f)\n", "child completed\n", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			output, err := captureRun(t, sourceFile(t, tc.source))
			if tc.diagnostic != "" {
				if err == nil || !strings.Contains(err.Error(), tc.diagnostic) {
					t.Fatalf("want %q, got %v", tc.diagnostic, err)
				}
				return
			}
			if err != nil || output != tc.output {
				t.Fatalf("output %q, error %v; want %q", output, err, tc.output)
			}
		})
	}
}

func TestTaskSharedListSerialized(t *testing.T) {
	source := "def add(items: list[int]) -> None:\n    for i: int in range(100):\n        items.append(i)\na: list[int] = []\nt: TyphonTask = spawn(add, a)\nu: TyphonTask = spawn(add, a)\njoin(t)\njoin(u)\n"
	e := NewEngine()
	defer e.Close()
	m, err := e.LoadModule(sourceFile(t, source))
	if err != nil {
		t.Fatal(err)
	}
	if err = e.ExecuteModule(m, true); err != nil {
		t.Fatal(err)
	}
	if len(*m.Globals["a"].List) != 200 {
		t.Fatal("lost concurrent appends")
	}
}

func TestInvalidLocalModuleDoesNotFallBackToPython(t *testing.T) {
	for _, cyclic := range []bool{false, true} {
		t.Run(fmt.Sprint(cyclic), func(t *testing.T) {
			path := sourceFile(t, "from helper import f\n")
			helper := "def f() -> int:\n    print(1)\n"
			want := "must return int"
			if cyclic {
				helper = "from regression import f\n"
				want = "cyclic Typhon import"
			}
			if err := os.WriteFile(filepath.Join(filepath.Dir(path), "helper.ty"), []byte(helper), 0600); err != nil {
				t.Fatal(err)
			}
			err := CheckFile(path)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("want %s, got %v", want, err)
			}
		})
	}
}

func privateBridge(t *testing.T) *PythonBridge {
	t.Helper()
	b, err := newPythonBridge(repoRoot(t), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

func pythonFunction(t *testing.T, b *PythonBridge, module, name string) *PyObject {
	t.Helper()
	m, err := b.ImportModule(module)
	if err != nil {
		t.Fatal(err)
	}
	f, err := b.GetAttr(m.PyObject, name)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Release(m.PyObject); err != nil {
		t.Fatal(err)
	}
	return f.PyObject
}

func TestAuditPythonPrecisionAndConversions(t *testing.T) {
	b := privateBridge(t)
	add := pythonFunction(t, b, "operator", "add")
	for _, integer := range []int64{9007199254740993, math.MaxInt64, math.MinInt64} {
		result, err := b.Call(add, []Value{IntValue(integer), IntValue(0)}, nil)
		if err != nil || result.Kind != ValueInt || result.Int != integer {
			t.Fatalf("integer %d: %v, %v", integer, result, err)
		}
	}
	_, err := b.Call(add, []Value{IntValue(math.MaxInt64), IntValue(1)}, nil)
	var pythonErr *PythonError
	if !errors.As(err, &pythonErr) || pythonErr.Type != "OverflowError" || pythonErr.Traceback == "" {
		t.Fatalf("missing structured overflow: %v", err)
	}
	if err := b.Ping(); err != nil {
		t.Fatal("session unusable after Python error:", err)
	}
	path := sourceFile(t, "from py.builtins import str\nx: int = str(12)\nprint(x + 1)\n")
	err = RunFile(path)
	if err == nil || !strings.Contains(err.Error(), "expected int, got str") {
		t.Fatalf("dynamic boundary accepted wrong result: %v", err)
	}
}

func TestAuditPythonOutputIsolation(t *testing.T) {
	b := privateBridge(t)
	printer := pythonFunction(t, b, "builtins", "print")
	if _, err := b.Call(printer, []Value{StringValue("library stdout")}, nil); err != nil {
		t.Fatal(err)
	}
	exec := pythonFunction(t, b, "builtins", "exec")
	code := "import os, sys; os.write(1, b'native stdout\\n'); sys.__stdout__.write('original stdout\\n'); sys.__stdout__.flush()"
	if _, err := b.Call(exec, []Value{StringValue(code)}, nil); err != nil {
		t.Fatal(err)
	}
	if err := b.Ping(); err != nil {
		t.Fatal(err)
	}
}

func TestAuditPythonTimeoutAndRelease(t *testing.T) {
	t.Run("release", func(t *testing.T) {
		b := privateBridge(t)
		for i := 0; i < 200; i++ {
			m, err := b.ImportModule("math")
			if err != nil {
				t.Fatal(err)
			}
			if err = b.Release(m.PyObject); err != nil {
				t.Fatal(err)
			}
			if _, err = b.GetAttr(m.PyObject, "pi"); err == nil {
				t.Fatal("released handle accepted")
			}
		}
		count, err := b.roundTrip(pyRequest{Op: "handle_count"})
		if err != nil || count.Int != 0 {
			t.Fatalf("retained handles: %v, %v", count, err)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		b := privateBridge(t)
		sleep := pythonFunction(t, b, "time", "sleep")
		b.timeout = 50 * time.Millisecond
		start := time.Now()
		_, err := b.Call(sleep, []Value{IntValue(10)}, nil)
		if err == nil || !strings.Contains(err.Error(), "timed out") || time.Since(start) > 3*time.Second {
			t.Fatalf("timeout failed: %v (%s)", err, time.Since(start))
		}
		if err = b.Ping(); err == nil {
			t.Fatal("timed-out session reused")
		}
	})
}

func TestPythonTemporaryHandlesAreCollected(t *testing.T) {
	b := privateBridge(t)
	for i := 0; i < 500; i++ {
		if _, err := b.ImportModule("math"); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		runtime.GC()
		count, err := b.roundTrip(pyRequest{Op: "handle_count"})
		if err != nil {
			t.Fatal(err)
		}
		if count.Int == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("temporary Python handles remain retained after GC")
}

func TestTaskCancellationAndTimeout(t *testing.T) {
	source := "def f() -> None:\n    print('must not run')\nt: TyphonTask = spawn(f)\ncancel(t)\n"
	output, err := captureRun(t, sourceFile(t, source))
	if err != nil || output != "" {
		t.Fatalf("cancelled task executed: %q, %v", output, err)
	}
	source = "def f() -> None:\n    while True:\n        print('')\nt: TyphonTask = spawn(f)\njoin(t, 0.01)\n"
	// No output in the busy child; repeated assignments exercise cancellation.
	source = strings.Replace(source, "        print('')", "        x: int = 1", 1)
	start := time.Now()
	err = RunFile(sourceFile(t, source))
	if err == nil || !strings.Contains(err.Error(), "join timed out") || time.Since(start) > 3*time.Second {
		t.Fatalf("unbounded join: %v (%s)", err, time.Since(start))
	}
}

func TestFailedListValidationDoesNotMutateAliases(t *testing.T) {
	inner := ListValue([]Value{IntValue(1)})
	outer := ListValue([]Value{inner, StringValue("wrong")})
	listFloat := Type{Kind: TypeList, Elem: typePtr(FloatType())}
	_, err := validateValue(outer, Type{Kind: TypeList, Elem: &listFloat})
	if err == nil || inner.ListGuard.elem != nil || (*inner.List)[0].Kind != ValueInt {
		t.Fatal("failed validation mutated input")
	}
}

func TestCyclesDoNotEscapeHost(t *testing.T) {
	value := ListValue(nil)
	*value.List = append(*value.List, value)
	if value.String() != "[[...]]" {
		t.Fatal(value.String())
	}
	if _, err := renderPythonJSON(value); err == nil {
		t.Fatal("circular JSON accepted")
	}
	b := &PythonBridge{}
	if _, err := b.valueToWire(value, 0); err == nil {
		t.Fatal("circular wire value accepted")
	}
}

func TestMalformedBridgeResponsesInvalidateSession(t *testing.T) {
	for _, response := range []string{
		"library output\n",
		`{"ok":true,"sequence":1`,
		"{\"ok\":true,\"sequence\":99,\"value\":{\"kind\":\"none\"}}\n",
		strings.Repeat("x", maxBridgeMessageBytes+1) + "\n",
	} {
		b := &PythonBridge{encoder: json.NewEncoder(io.Discard), reader: bufio.NewReader(strings.NewReader(response))}
		if err := b.Ping(); err == nil {
			t.Fatal("malformed response accepted")
		}
		if !b.closed.Load() {
			t.Fatal("malformed session remains open")
		}
	}
}

func TestDynamicLoopAndAnyArity(t *testing.T) {
	path := sourceFile(t, "def f(value: Any) -> None:\n    print(value)\nf()\n")
	if err := CheckFile(path); err == nil {
		t.Fatal("Any parameter bypassed arity")
	}
	path = sourceFile(t, "items: Any = ['wrong']\nfor item: int in items:\n    print(item)\n")
	err := RunFile(path)
	if err == nil || !strings.Contains(err.Error(), "expected int") {
		t.Fatalf("typed loop boundary: %v", err)
	}
	if err := CheckFile(sourceFile(t, "x: int = []\n")); err == nil {
		t.Fatal("empty list bypassed scalar type")
	}
}
