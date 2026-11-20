package typhon

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNativeMiddlewareOrderShortCircuitAndExceptions(t *testing.T) {
	engine := NewEngine()
	defer engine.Close()
	module, err := engine.LoadModule(sourceFile(t, `import typhon.http as http
import typhon.web as web
r: web.Router = web.router()
events: list[str] = []
def outer(req: http.Request, next: web.Next) -> http.Response:
    events.append('a before')
    try:
        return next.call()
    except ValidationError:
        events.append('a error')
        return http.response(400, 'handled')
    finally:
        events.append('a finally')
def inner(req: http.Request, next: web.Next) -> http.Response:
    events.append('b before')
    if req.path == '/blocked':
        return http.response(403, 'blocked')
    result: http.Response = next.call()
    events.append('b after')
    return result
def route_step(req: http.Request, next: web.Next) -> http.Response:
    events.append('c before')
    try:
        result: http.Response = next.call()
        if req.path == '/twice':
            return next.call()
        return result
    finally:
        events.append('c finally')
def handler(req: http.Request) -> http.Response:
    events.append('handler')
    if req.path == '/error':
        raise ValidationError('invalid')
    if req.path == '/deadline':
        while True:
            work: int = 1
    return http.response(200, req.request_id, [['X-Request-ID', 'fake']])
r.use_request_id()
r.use_logging()
r.use(outer)
r.use(inner)
r.add('GET', '/ok', handler)
r.add('GET', '/blocked', handler)
r.add('GET', '/error', handler)
r.add('GET', '/twice', handler)
r.add('GET', '/deadline', handler)
r.use_on('GET', '/ok', route_step)
r.use_on('GET', '/error', route_step)
r.use_on('GET', '/twice', route_step)
r.use_on('GET', '/deadline', route_step)
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ExecuteModule(module, false); err != nil {
		t.Fatal(err)
	}
	handler := httpVMHandler(engine, module.Globals["r"])
	seenIDs := map[string]bool{}
	for _, tc := range []struct {
		path, events string
		code         int
	}{
		{"/ok", "[a before, b before, c before, handler, c finally, b after, a finally]", 200},
		{"/blocked", "[a before, b before, a finally]", 403},
		{"/error", "[a before, b before, c before, handler, c finally, a error, a finally]", 400},
		{"/twice", "[a before, b before, c before, handler, c finally, a error, a finally]", 400},
		{"/deadline", "[a before, b before, c before, handler, c finally, a finally]", 504},
	} {
		*module.Globals["events"].List = nil
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		cancel := func() {}
		if tc.path == "/deadline" {
			var ctx context.Context
			ctx, cancel = context.WithTimeout(req.Context(), 50*time.Millisecond)
			req = req.WithContext(ctx)
		}
		req.Header.Set("X-Request-ID", "untrusted")
		handler.ServeHTTP(w, req)
		cancel()
		if w.Code != tc.code || module.Globals["events"].String() != tc.events {
			t.Fatalf("%s: code %d, events %s", tc.path, w.Code, module.Globals["events"])
		}
		id := w.Header().Get("X-Request-ID")
		if id == "" || id == "untrusted" || id == "fake" || seenIDs[id] || tc.code == 200 && id != w.Body.String() {
			t.Fatalf("%s: request ID did not remain unique and immutable: %q", tc.path, id)
		}
		seenIDs[id] = true
	}
}

func TestNativeMiddlewareRequestStateIsolation(t *testing.T) {
	engine := NewEngine()
	defer engine.Close()
	module, err := engine.LoadModule(sourceFile(t, `import typhon.http as http
import typhon.web as web
r: web.Router = web.router()
def state(req: http.Request, next: web.Next) -> http.Response:
    if req.state('local') is not None:
        return http.response(500, 'state escaped')
    req.set_state('local', req.query('marker'))
    req.set_state('identity', 'fake')
    return next.call()
def handle(req: http.Request) -> http.Response:
    return http.json(200, [req.state('local'), req.identity, req.tenant])
r.use(state)
r.add('GET', '/', handle)
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ExecuteModule(module, false); err != nil {
		t.Fatal(err)
	}
	pool := newHTTPWorkerPool(engine.ctx, 4, 64)
	defer pool.close()
	if err := pool.load(&VM{engine: engine}, module.Globals["r"]); err != nil {
		t.Fatal(err)
	}
	var requests sync.WaitGroup
	for i := range 48 {
		requests.Go(func() {
			marker := fmt.Sprintf("visitor_%d", i)
			req := httptest.NewRequest(http.MethodGet, "/?marker="+marker, nil)
			req.Header.Set("X-Identity", "fake")
			want := fmt.Sprintf(`[%q, null, null]`, marker)
			if i%2 == 0 {
				req = req.WithContext(context.WithValue(req.Context(), verifiedIdentityKey{}, verifiedRequestIdentity{subject: marker, tenant: "tenant"}))
				want = fmt.Sprintf(`[%q, %q, "tenant"]`, marker, marker)
			}
			out := httptest.NewRecorder()
			pool.ServeHTTP(out, req)
			if out.Code != 200 || strings.TrimSpace(out.Body.String()) != want {
				t.Errorf("request %s returned %d %s", marker, out.Code, out.Body)
			}
		})
	}
	requests.Wait()
}
