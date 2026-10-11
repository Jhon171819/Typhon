package typhon

import (
	"math"
	"strings"
	"testing"
)

func TestDictionaryExample(t *testing.T) {
	output, err := captureRun(t, examplePath(t, "dictionaries.ty"))
	want := "Ada: 15\nGrace: 20\nLinus: 8\n43\nTrue True\n0\n[Ada, Grace, Linus] 3\n{\"Ada\": 15, \"Grace\": 20}\n"
	if err != nil || output != want {
		t.Fatalf("output %q, error %v; want %q", output, err, want)
	}
}

func TestDictionaryOperations(t *testing.T) {
	source := `
d: dict[str, int] = {'a': 1, 'b': 2, 'a': 3}
print(d['a'], len(d), d.keys(), d.values(), d.items())
d.update({'b': 4, 'c': 5})
print(d.setdefault('a', 99), d.setdefault('z', 6))
print(d.get('absent'), d.get('absent', 0), d.pop('absent', -1))
print(d.pop('b'))
del d['a']
d['a'] = 7
print(d.keys())
copy: dict[str, int] = d.copy()
copy['a'] = 9
print(d['a'], copy['a'], d is copy, d == copy)
clone: dict[str, int] = dict(d)
print(clone == d, clone is d, {'a': 7, 'z': 6, 'c': 5} == d)
for key: str in d:
    print(key, d[key])
for index: int, key: str in enumerate(d):
    print(index, key)
d.clear()
print(len(d), not d, d)
empty: dict[str, int] = dict()
print(empty == d)
nested: dict[str, list[dict[int, float]]] = {'values': [{1: 2}]}
nested['values'][0][2] = 3
print(nested['values'][0][1], nested['values'][0][2])
arr: list[int] = [1, 2]
arr[-1] = 3
print(arr[0], arr[-1], 'ação'[1], len('ação'))
print('a' in ['a'], 'ç' in 'ação', 'a' not in d)
type Status = 'open' | 'closed'
statuses: dict[Status, int] = {'open': 1}
statuses['closed'] = 2
print(statuses['closed'])
d = {'new': 8}
print(d['new'])
`
	want := "3 2 [a, b] [3, 2] [[a, 3], [b, 2]]\n3 6\nNone 0 -1\n4\n[c, z, a]\n7 9 False False\nTrue False True\nc 5\nz 6\na 7\n0 c\n1 z\n2 a\n0 True {}\nTrue\n2 3\n1 3 ç 4\nTrue True True\n2\n8\n"
	output, err := captureRun(t, sourceFile(t, source))
	if err != nil || output != want {
		t.Fatalf("output %q, error %v; want %q", output, err, want)
	}
}

func TestDictionaryStaticErrors(t *testing.T) {
	cases := []struct{ source, diagnostic string }{
		{"d: dict = {}", "key and value types"},
		{"def f(d: dict) -> None:\n    print(d)", "key and value types"},
		{"d: dict[str] = {}", "2 type arguments"},
		{"d: dict[str, int, bool] = {}", "2 type arguments"},
		{"d: dict[list[int], int] = {}", "unhashable"},
		{"d: dict[str, int] = {1: 2}", "dictionary key expected str"},
		{"d: dict[str, int] = {'a': 'wrong'}", "dictionary value expected int"},
		{"d: dict[str, list[int]] = {'a': [1, 'wrong']}", "list element expected int"},
		{"d: dict[str, int] = {}\nd['a'] = 'wrong'", "assignment expected int"},
		{"d: dict[str, int] = {}\nprint(d[1])", "dictionary key expected str"},
		{"d: dict[str, int] = {}\nprint(1 in d)", "dictionary key expected str"},
		{"d: dict[str, int] = {}\nfor key: int in d:\n    print(key)", "for variable expected int"},
		{"a: dict[str, int] = {}\nb: dict[str, float] = a", "assignment expected"},
		{"a: dict[int, int] = {}\nb: dict[float, int] = a", "assignment expected"},
		{"d: dict[str, int] = {}\nd.update({'a': 'wrong'})", "dictionary value expected int"},
		{"d: dict[str, int] = {}\nd.setdefault('a', 'wrong')", "argument 2 expected int"},
		{"d: dict[str, int] = {}\nd.get()", "call expected"},
		{"d: dict[str, int] = {}\nd.missing()", "has no attribute"},
		{"d: dict[str, int] = {'a'}", "requires ':'"},
		{"d: dict[str, int] = {[]: 1}", "unhashable"},
		{"s: str = 'a'\ns[0] = 'b'", "index assignment requires"},
		{"a: list[int] = [1]\ndel a[0]", "del requires a dictionary"},
	}
	for _, tc := range cases {
		err := CheckFile(sourceFile(t, tc.source))
		if err == nil || !strings.Contains(err.Error(), tc.diagnostic) {
			t.Errorf("%s: want %q, got %v", tc.source, tc.diagnostic, err)
		}
	}
}

func TestDictionaryRuntimeErrors(t *testing.T) {
	cases := []struct{ source, diagnostic string }{
		{"d: dict[str, int] = {}\nprint(d['missing'])", "KeyError"},
		{"d: dict[str, int] = {}\ndel d['missing']", "KeyError"},
		{"d: dict[str, int] = {}\nd.pop('missing')", "KeyError"},
		{"d: dict[str, int] = {}\na: Any = d\na['x'] = 'wrong'", "expected int"},
		{"d: dict[str, int] = {}\na: Any = d\na[1] = 1", "expected str"},
		{"d: dict[str, int] = {}\na: Any = d\na.update({'a': 1, 'b': 'wrong'})", "expected int"},
		{"d: dict[str, int] = {}\na: Any = d\na.setdefault('x', 'wrong')", "expected int"},
		{"d: Any = {}\nd[[]] = 1", "unhashable"},
		{"d: Any = {'x': 'wrong'}\nx: dict[str, int] = d", "expected int"},
		{"def f(d: dict[str, int]) -> int:\n    return d['x']\na: Any = {'x': 'wrong'}\nf(a)", "expected int"},
		{"def f() -> dict[str, int]:\n    d: Any = {'x': 'wrong'}\n    return d\nf()", "expected int"},
		{"d: Any = {'x': 1}\nx: dict[str, int] = d\ny: dict[str, float] = d", "incompatible mutable dictionary contract"},
		{"a: list[int] = [1]\nd: dict[str, list[int]] = {'x': a}\nb: Any = d\nb['x'][0] = 'wrong'", "expected int"},
		{"import json\nd: Any = {}\nd['self'] = d\nprint(json.dumps(d))", "depth limit"},
	}
	for _, tc := range cases {
		_, err := captureRun(t, sourceFile(t, tc.source))
		if err == nil || !strings.Contains(err.Error(), tc.diagnostic) {
			t.Errorf("%s: want %q, got %v", tc.source, tc.diagnostic, err)
		}
	}
}

func TestDictionaryValidationIsAtomic(t *testing.T) {
	d := DictValue()
	list := ListValue([]Value{IntValue(1)})
	_ = d.Dict.put(StringValue("good"), list)
	_ = d.Dict.put(StringValue("bad"), StringValue("wrong"))
	_, err := validateValue(d, dictType(StrType(), listType(FloatType())))
	if err == nil || d.Dict.Contract != nil || list.ListGuard.elem != nil || (*list.List)[0].Kind != ValueInt {
		t.Fatalf("failed validation mutated aliases: %v", err)
	}

	receiver := DictValue()
	_ = receiver.Dict.put(StringValue("old"), ListValue([]Value{IntValue(2)}))
	_, err = validateValue(receiver, dictType(StrType(), listType(FloatType())))
	if err != nil {
		t.Fatal(err)
	}
	method, _ := dictMethod(receiver, "update")
	_, err = method.Bound.Native.Fn(nil, []Value{receiver, d})
	if err == nil || len(receiver.Dict.Entries) != 1 || list.ListGuard.elem != nil || (*list.List)[0].Kind != ValueInt {
		t.Fatalf("failed update mutated aliases: %v", err)
	}
}

func TestDictionaryKeysAndCycles(t *testing.T) {
	d := DictValue()
	keys := []Value{IntValue(1), FloatValue(1), BoolValue(true), FloatValue(math.Copysign(0, -1)), IntValue(0), NoneValue(), StringValue("1"), IntValue(math.MaxInt64), FloatValue(9223372036854775808)}
	for i, key := range keys {
		if err := d.Dict.put(key, IntValue(int64(i))); err != nil {
			t.Fatal(err)
		}
	}
	if len(d.Dict.Entries) != 6 {
		t.Fatalf("numeric key equivalence lost: %v", d)
	}
	v, err := getIndex(d, IntValue(1))
	if err != nil || v.Int != 2 {
		t.Fatalf("duplicate key not replaced: %v %v", v, err)
	}
	for _, key := range []Value{ListValue(nil), d, FloatValue(math.NaN()), FloatValue(math.Inf(1))} {
		if err := d.Dict.put(key, NoneValue()); err == nil {
			t.Fatalf("invalid key accepted: %v", key)
		}
	}
	a, b := DictValue(), DictValue()
	_ = a.Dict.put(StringValue("self"), a)
	_ = b.Dict.put(StringValue("self"), b)
	if !valuesEqual(a, b) || valuesIdentical(a, b) || !strings.Contains(a.String(), "{...}") {
		t.Fatal("cycle equality/printing failed")
	}
	if _, err := validateValue(a, dictType(StrType(), AnyType())); err != nil {
		t.Fatal(err)
	}
	// Key promotion must agree between insertion and every lookup operation.
	floating := DictValue()
	key := IntValue(9007199254740993)
	_ = floating.Dict.put(key, IntValue(42))
	if _, err := validateValue(floating, dictType(FloatType(), IntType())); err != nil {
		t.Fatal(err)
	}
	item, err := getIndex(floating, key)
	if err != nil || item.Int != 42 {
		t.Fatalf("promoted key lookup: %v %v", item, err)
	}
	if found, err := containsValue(floating, key); err != nil || !found {
		t.Fatalf("promoted membership: %v %v", found, err)
	}
}

func TestPostgresDictionaryConversion(t *testing.T) {
	v, err := postgresValue(map[string]any{"z": []any{int64(3)}, "a": map[string]any{"ok": true}}, 0)
	if err != nil || v.Kind != ValueDict || v.Dict.Entries[0].Key.Str != "a" {
		t.Fatalf("PostgreSQL JSON conversion: %v %v", v, err)
	}
	nested, err := getIndex(v, StringValue("a"))
	if err != nil {
		t.Fatal(err)
	}
	ok, err := getIndex(nested, StringValue("ok"))
	if err != nil || !ok.Bool {
		t.Fatalf("nested PostgreSQL dictionary: %v %v", ok, err)
	}
}

func TestDictionaryContractsAcrossFunctionsAndFields(t *testing.T) {
	source := `
class Store:
    data: dict[str, list[float]]
    def __init__(self) -> None:
        self.data = {'values': [1, 2]}

def make() -> dict[str, int | str] | None:
    return {'age': 37, 'name': 'Ada'}

def read(data: dict[str, int | str] | None) -> None:
    if data is not None:
        print(data['age'])

optional: dict[str, int | str] | None = {'age': 42}
read(optional)
read({'age': 43})
read(make())
store: Store = Store()
store.data = {'values': [3, 4]}
store.data['values'] = [5, 6]
print(store.data['values'])
value: int = {'age': 44}.pop('age')
fallback: int = {'age': 44}.get('missing', 0)
print(value, fallback)
print('x' in {}, 'x' not in dict(), {}.get('x'), {}.get('x', 7))
`
	output, err := captureRun(t, sourceFile(t, source))
	want := "42\n43\n37\n[5, 6]\n44 0\nFalse True None 7\n"
	if err != nil || output != want {
		t.Fatalf("output %q, error %v; want %q", output, err, want)
	}
}

func TestDictionaryJSONAndPythonBridge(t *testing.T) {
	output, err := captureRun(t, sourceFile(t, `
import json
data: dict[str, Any] = {'name': 'Ada', 'values': [1, {'ok': True}], 'empty': {}}
print(json.dumps(data))
from py.builtins import eval
result: dict[str, list[int]] = eval("{'first': [1, 2], 'second': []}")
print(result['first'][1], result.keys())
`))
	want := "{\"name\": \"Ada\", \"values\": [1, {\"ok\": true}], \"empty\": {}}\n2 [first, second]\n"
	if err != nil || output != want {
		t.Fatalf("output %q, error %v; want %q", output, err, want)
	}
	b := privateBridge(t)
	copy := pythonFunction(t, b, "builtins", "dict")
	d := DictValue()
	_ = d.Dict.put(IntValue(math.MaxInt64), ListValue([]Value{IntValue(2), DictValue()}))
	_ = d.Dict.put(NoneValue(), StringValue("none"))
	result, err := b.Call(copy, []Value{d}, nil)
	if err != nil || !valuesEqual(d, result) {
		t.Fatalf("bridge round trip: %v %v", result, err)
	}
	_ = d.Dict.put(StringValue("self"), d)
	if _, err := b.valueToWire(d, 0); err == nil {
		t.Fatal("bridge accepted cyclic dictionary")
	}
}
