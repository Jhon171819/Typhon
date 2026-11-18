package typhon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseExceptions(t *testing.T) {
	module, err := ParseFile("test.ty", `try:
    try:
        raise ValidationError('bad')
    finally:
        print('inner')
except ValidationError as problem:
    raise
except Error:
    print('outer')
else:
    print('success')
finally:
    print('done')
`)
	if err != nil {
		t.Fatal(err)
	}
	stmt, ok := module.Stmts[0].(*TryStmt)
	if !ok || len(stmt.Excepts) != 2 || stmt.Excepts[0].Binding != "problem" || stmt.Excepts[1].Type != "Error" || len(stmt.Else) != 1 || len(stmt.Finally) != 1 {
		t.Fatalf("unexpected AST: %#v", module.Stmts[0])
	}
	if nested, ok := stmt.Body[0].(*TryStmt); !ok || len(nested.Finally) != 1 {
		t.Fatalf("unexpected nested AST: %#v", stmt.Body[0])
	}
	for _, source := range []string{
		"try:\n    print(1)\n",
		"try:\nfinally:\n    print(1)\n",
		"try:\n    print(1)\nexcept Error as 1:\n    print(2)\n",
		"try:\n    print(1)\nexcept Error as (err):\n    print(2)\n",
		"try:\n    print(1)\nexcept:\n    print(2)\n",
		"try:\n    print(1)\nelse:\n    print(2)\n",
		"try:\n    print(1)\nfinally:\n    print(2)\nexcept Error:\n    print(3)\n",
		"try:\n    print(1)\nexcept Error:\n    print(2)\nelse:\n",
	} {
		if _, err := ParseFile("test.ty", source); err == nil {
			t.Errorf("accepted invalid syntax: %q", source)
		}
	}
}

func TestCheckExceptions(t *testing.T) {
	for _, tc := range []struct{ source, want string }{
		{"raise\n", "bare raise requires"},
		{"raise 'bad'\n", "raise requires Error"},
		{"raise Error(12)\n", "expected str"},
		{"x: TimeoutError = ConnectionError('bad')\n", "assignment expected TimeoutError"},
		{"try:\n    print(1)\nexcept str:\n    print(2)\n", "requires an Error type"},
		{"try:\n    print(1)\nexcept Error:\n    print(2)\nexcept TimeoutError:\n    print(3)\n", "already covered"},
		{"try:\n    print(1)\nexcept Error as e:\n    e = Error('other')\n", "read-only"},
		{"try:\n    print(1)\nexcept Error as e:\n    print(e.kind)\nprint(e)\n", "unknown name"},
		{"try:\n    print(1)\nexcept Error:\n    print(2)\nfinally:\n    raise\n", "bare raise requires"},
		{"try:\n    print(1)\nexcept Error:\n    def nested() -> None:\n        raise\n", "bare raise requires"},
		{"def f() -> int:\n    try:\n        return 1\n    except Error:\n        print(2)\n", "must return int"},
		{"def f() -> int:\n    result: int\n    try:\n        result = 1\n    except Error:\n        print(result)\n    return result\n", "not initialized"},
		{`type BadInput = ValidationError
def fail() -> Error:
    raise ValidationError('bad')
def read(flag: bool) -> str:
    result: str
    try:
        if flag:
            fail()
        result = 'ok'
    except BadInput as problem:
        result = problem.message
    return result
`, ""},
	} {
		module, err := ParseFile("test.ty", tc.source)
		if err != nil {
			t.Fatalf("parser: %v", err)
		}
		_, err = CheckModule(module, nil)
		if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("wanted %q, got %v for %q", tc.want, err, tc.source)
		}
	}
}

func TestExceptionUnwind(t *testing.T) {
	source := `def fail() -> int:
    raise ValidationError('bad')

def add(left: int, right: int) -> int:
    return left + right

def run() -> int:
    for i: int in range(4):
        try:
            if i == 0:
                continue
            if i == 1:
                return add(99, fail())
            if i == 3:
                break
        except ValidationError as err:
            print(err.kind, err.message)
        except Error:
            print('wrong handler')
        else:
            print('else', i)
        finally:
            for k: int in range(2):
                try:
                    continue
                finally:
                    print('finally', i, k)
    try:
        return 42
    finally:
        print('return cleanup')

def override() -> int:
    try:
        return 1
    finally:
        raise TimeoutError('replacement')

print(run())
try:
    try:
        fail()
    except ValidationError:
        raise DatabaseError('from handler')
    except DatabaseError:
        print('wrong sibling')
    finally:
        print('handler cleanup')
except DatabaseError as err:
    print(err.message)
try:
    override()
except TimeoutError as err:
    print(err.message)
try:
    try:
        print('body succeeded')
    except Error:
        print('wrong else handler')
    else:
        fail()
except Error as caught:
    saved: Any = caught
    try:
        raise saved
    except ValidationError as caught:
        print(caught is saved)
    print(caught is saved)
try:
    dynamic: Any = 123
    raise dynamic
except ValidationError as err:
    print(err.message)
`
	output, err := captureRun(t, sourceFile(t, source))
	want := "finally 0 0\nfinally 0 1\nValidationError bad\nfinally 1 0\nfinally 1 1\nelse 2\nfinally 2 0\nfinally 2 1\nfinally 3 0\nfinally 3 1\nreturn cleanup\n42\nhandler cleanup\nfrom handler\nreplacement\nbody succeeded\nTrue\nTrue\nraise requires Error, got int\n"
	if err != nil || output != want {
		t.Fatalf("got %q (%v), want %q", output, err, want)
	}
}

func TestExceptionReraiseAndReuseEngine(t *testing.T) {
	engine := NewEngine()
	defer engine.Close()
	failed, err := engine.LoadModule(sourceFile(t, `def fail() -> None:
    raise ValidationError('first')
def run() -> None:
    try:
        fail()
    except Error as err:
        try:
            raise TimeoutError('inner')
        except TimeoutError as err:
            print(err.message)
        finally:
            raise
run()
`))
	if err != nil {
		t.Fatal(err)
	}
	err = engine.ExecuteModule(failed, true)
	var exception *Exception
	if !errors.As(err, &exception) || exception.Kind() != validationKind {
		t.Fatalf("unexpected exception: %v", err)
	}
	for _, line := range []string{":2 in fail", ":5 in run", ":13 in <module>"} {
		if !strings.Contains(err.Error(), line) {
			t.Errorf("missing frame %q: %v", line, err)
		}
	}
	if strings.Contains(err.Error(), ":12 in run") {
		t.Fatalf("rethrow changed the original traceback: %v", err)
	}

	next, err := engine.LoadModule(sourceFile(t, `def fail() -> None:
    raise ConnectionError('child')
task: TyphonTask = spawn(fail)
result: str = ''
try:
    join(task)
except ConnectionError as err:
    result = err.message
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ExecuteModule(next, true); err != nil || next.Globals["result"].Str != "child" {
		t.Fatalf("next module failed or task error was reported twice: %v", err)
	}
}

func TestExceptionMandatoryCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	transport := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cancel()
		<-r.Context().Done()
	}))
	defer transport.Close()
	source := fmt.Sprintf(`import typhon.http as http
client: http.Client = http.client(1)
state: str = 'started'
try:
    client.get(%q)
except Error:
    state = 'caught'
finally:
    state = 'cleanup'
`, transport.URL)
	engine := NewEngineWithContext(ctx)
	defer engine.Close()
	module, err := engine.LoadModule(sourceFile(t, source))
	if err != nil {
		t.Fatal(err)
	}
	err = engine.ExecuteModule(module, true)
	var exception *Exception
	if !errors.Is(err, context.Canceled) || errors.As(err, &exception) || module.Globals["state"].Str != "started" {
		t.Fatalf("cancellation was intercepted: %v, state %v", err, module.Globals["state"])
	}
}

func TestExceptionExecutionLimits(t *testing.T) {
	engine := NewEngine()
	defer engine.Close()
	module, err := engine.LoadModule(sourceFile(t, `state: str = 'started'
def recurse() -> None:
    try:
        recurse()
    except Error:
        print('intercepted')
    finally:
        print('intercepted in finally')
try:
    recurse()
except Error:
    state = 'caught'
finally:
    state = 'cleanup'
`))
	if err != nil {
		t.Fatal(err)
	}
	err = engine.ExecuteModule(module, false)
	var stop *executionStop
	if !errors.As(err, &stop) || !strings.Contains(err.Error(), "call depth limit") || module.Globals["state"].Str != "started" {
		t.Fatalf("call depth limit was intercepted: %v", err)
	}
	vm := &VM{engine: engine, module: module, instructions: 9_999_997}
	_, err = vm.runFunction(module.Program.Module, nil)
	if !errors.As(err, &stop) || !strings.Contains(err.Error(), "instruction budget") || module.Globals["state"].Str != "started" {
		t.Fatalf("instruction budget was intercepted: %v", err)
	}
}
