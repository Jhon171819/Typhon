package typhon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestResourceScopeLIFO(t *testing.T) {
	var scope resourceScope
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	first, last := errors.New("first failure"), errors.New("last failure")
	var closed []int
	for i := range 4 {
		if err := scope.add(ctx, i, fmt.Sprintf("resource %d", i), func(ctx context.Context) error {
			if _, ok := ctx.Deadline(); !ok {
				t.Error("closer received no deadline")
			}
			closed = append(closed, i)
			switch i {
			case 0:
				return first
			case 3:
				return last
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	scope.remove(1)
	for range 2 {
		err := scope.close(ctx)
		if !errors.Is(err, first) || !errors.Is(err, last) || !strings.HasPrefix(err.Error(), "resource 3:") {
			t.Fatalf("incomplete or unordered diagnostics: %v", err)
		}
	}
	if !reflect.DeepEqual(closed, []int{3, 2, 0}) {
		t.Fatalf("closure order or count: %v", closed)
	}
	if err := scope.add(ctx, 4, "late resource", func(context.Context) error { return first }); !errors.Is(err, first) {
		t.Fatalf("late resource cleanup error lost: %v", err)
	}
}

func TestNativeCleanupDeadlineAndConcurrentEngineClose(t *testing.T) {
	engine := NewEngine()
	started := make(chan struct{})
	finished := make(chan struct{})
	if err := engine.resources.add(engine.ctx, 0, "test closer", func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		close(finished)
		return ctx.Err()
	}); err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	go func() { results <- engine.Close() }()
	<-started
	go func() { results <- engine.Close() }()
	for range 2 {
		select {
		case err := <-results:
			if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "test closer") {
				t.Fatal(err)
			}
			select {
			case <-finished:
			default:
				t.Fatal("Close returned before the resource finished")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("unbounded Engine close")
		}
	}
}

func TestFinallyCleanupFailure(t *testing.T) {
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })
	err := RunFile(sourceFile(t, `try:
    raise ValidationError('original')
finally:
    raise DatabaseError('cleanup failed')
`))
	var ex *Exception
	if !errors.As(err, &ex) || ex.Message() != "original" || strings.Contains(err.Error(), "cleanup failed") {
		t.Fatalf("primary error replaced: %v", err)
	}
	if !strings.Contains(logs.String(), "Typhon cleanup (finally)") || !strings.Contains(logs.String(), "cleanup failed") {
		t.Fatalf("cleanup failed without a diagnostic: %s", logs.String())
	}
	primary := errors.New("primary")
	closeFailure := errors.New("close failure")
	if err := finalizeRun(primary, closeFailure); !errors.Is(err, primary) || !errors.Is(err, closeFailure) || !strings.HasPrefix(err.Error(), "primary\n") {
		t.Fatalf("lost primary or close error: %v", err)
	}
}

func TestNativeCancellationFinallyAndEngineClose(t *testing.T) {
	started := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		close(started)
		<-req.Context().Done()
	}))
	defer upstream.Close()
	engine := NewEngine()
	defer engine.Close()
	module, err := engine.LoadModule(sourceFile(t, fmt.Sprintf(`import typhon.http as http
state: list[str] = []
client: http.Client = http.client()
def inner() -> int:
    try:
        client.get(%q)
        return 0
    finally:
        client.close()
        state.append('inner')
        return 1
try:
    inner()
    state.append('resumed')
except Error:
    state.append('caught')
finally:
    state.append('outer')
`, upstream.URL)))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- engine.ExecuteModule(module, true) }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("HTTP operation did not start")
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	if got := module.Globals["state"].String(); got != "[inner, outer]" {
		t.Fatalf("unexpected flow after cancellation: %s", got)
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation replaced by finally return: %v", err)
	}
}

func TestCancellationFinallyBudget(t *testing.T) {
	engine := NewEngine()
	defer engine.Close()
	module, err := engine.LoadModule(sourceFile(t, `state: str = 'started'
try:
    try:
        print('cancel')
    finally:
        while True:
            state = 'cleanup'
finally:
    state = 'outer'
`))
	if err != nil {
		t.Fatal(err)
	}
	module.Globals["print"] = nativeValue("print", func(*VM, []Value) (Value, error) {
		engine.cancel()
		return NoneValue(), nil
	})
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })
	err = engine.ExecuteModule(module, true)
	if !errors.Is(err, context.Canceled) || module.Globals["state"].Str != "cleanup" ||
		!strings.Contains(logs.String(), "cancellation finally instruction budget exceeded") {
		t.Fatalf("cleanup budget or cancellation lost: %v, state %v, logs %s", err, module.Globals["state"], logs.String())
	}
}

func TestRequestScopeHandlesAndTaskReclamation(t *testing.T) {
	engine := NewEngine()
	defer engine.Close()
	module, err := engine.LoadModule(sourceFile(t, `import typhon.http as http
saved: list[Any] = []
events: list[str] = []
def background() -> None:
    events.append('orphan')
def handle(req: http.Request) -> http.Response:
    if req.path == '/store':
        client: http.Client = http.client()
        saved.append(req.query)
        saved.append(client.close)
        client.close()
        client.close()
    if req.path == '/use':
        for method: Any in saved:
            try:
                method()
                return http.response(500, 'method escaped')
            except ValidationError as err:
                events.append(err.message)
    spawn(background)
    return http.response(200, 'ok')
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ExecuteModule(module, false); err != nil {
		t.Fatal(err)
	}
	handler := httpVMHandler(engine, module.Globals["handle"])
	for i := range 270 {
		path := "/"
		switch i {
		case 0:
			path = "/store"
		case 1:
			path = "/use"
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != 200 {
			t.Fatalf("request %d returned %d: %s", i, recorder.Code, recorder.Body)
		}
	}
	if got := module.Globals["events"].String(); got != "[native object belongs to another request, native object belongs to another request]" {
		t.Fatalf("request state escaped its scope: %s", got)
	}
	other := NewEngine()
	defer other.Close()
	methods := *module.Globals["saved"].List
	if _, err := (&VM{engine: other}).callValue(methods[1], nil, nil); err == nil || !strings.Contains(err.Error(), "another Engine") {
		t.Fatalf("bound method escaped its Engine: %v", err)
	}
}
