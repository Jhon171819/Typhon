package typhon

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestNativeRouterCRUDAndMethods(t *testing.T) {
	engine := NewEngine()
	defer engine.Close()
	module, err := engine.LoadModule(examplePath(t, "http_router.ty"))
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ExecuteModule(module, false); err != nil {
		t.Fatal(err)
	}
	handler := httpVMHandler(engine, module.Globals["routes"])
	allow := "DELETE, GET, HEAD, OPTIONS, PATCH, PUT"
	for _, tc := range []struct {
		method, path, input, output, allow string
		status                             int
	}{
		{"POST", "/v1/users", `{"id":"café","name":"Ada"}`, `{"id": "café", "name": "Ada"}`, "", 201},
		{"GET", "/v1/users/caf%C3%A9", "", `{"id": "café", "name": "Ada"}`, "", 200},
		{"HEAD", "/v1/users/caf%C3%A9", "", "", "", 200},
		{"PUT", "/v1/users/caf%C3%A9", `{"name":"Grace"}`, `{"id": "café", "name": "Grace"}`, "", 200},
		{"PATCH", "/v1/users/caf%C3%A9", `{"name":"Lin"}`, `{"id": "café", "name": "Lin"}`, "", 200},
		{"OPTIONS", "/v1/users/caf%C3%A9", "", "", allow, 204},
		{"POST", "/v1/users/caf%C3%A9", "", "method not allowed", allow, 405},
		{"GET", "/v1/users/search?tag=x&tag=y&limit=10", "", `{"tags": ["x", "y"], "limit": "10", "offset": ""}`, "", 200},
		{"POST", "/v1/users/search", "", "method not allowed", "GET, HEAD, OPTIONS", 405},
		{"DELETE", "/v1/users/caf%C3%A9", "", "", "", 204},
		{"GET", "/v1/users/caf%C3%A9", "", "user not found", "", 404},
		{"GET", "/v2/users", "", "not found", "", 404},
		{"GET", "/v1/users/%2e%2e", "", "invalid request path", "", 400},
		{"GET", "/v1/users/a%2Fb", "", "invalid request path", "", 400},
		{"GET", "/v1/users/a%5Cb", "", "invalid request path", "", 400},
		{"GET", "/v1/users/a%252Fb", "", "invalid request path", "", 400},
		{"GET", "/v1/users/%FF", "", "invalid request path", "", 400},
	} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.input)))
		if w.Code != tc.status || w.Body.String() != tc.output || w.Header().Get("Allow") != tc.allow {
			t.Errorf("%s %s: %d %q Allow=%q; wanted %d %q Allow=%q", tc.method, tc.path, w.Code, w.Body, w.Header().Get("Allow"), tc.status, tc.output, tc.allow)
		}
		if tc.method == http.MethodHead && w.Header().Get("Content-Length") == "" {
			t.Error("HEAD lost the length of the GET representation")
		}
	}
}

func TestNativeRouterRegistrationConflicts(t *testing.T) {
	for _, tc := range []struct{ registration, want string }{
		{"r.add('GET', '/users/{name}', f)", "conflicting GET route"},
		{"r.add('GET', '/users/{id}/{id}', f)", "duplicate route parameter"},
		{"r.add('GET', '/users//bad', f)", "invalid literal"},
		{"r.add('get', '/users', f)", "uppercase HTTP method"},
	} {
		source := `import typhon.http as http
import typhon.web as web
def f(req: http.Request) -> http.Response:
    return http.response(200, 'ok')
r: web.Router = web.router()
r.add('GET', '/users/{id}', f)
` + tc.registration + "\n"
		err := RunFile(sourceFile(t, source))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: expected %q, got %v", tc.registration, tc.want, err)
		}
	}
}

func TestNativeRouterConcurrentWorkers(t *testing.T) {
	engine := NewEngine()
	defer engine.Close()
	module, err := engine.LoadModule(examplePath(t, "http_router.ty"))
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ExecuteModule(module, false); err != nil {
		t.Fatal(err)
	}
	pool := newHTTPWorkerPool(engine.ctx, 4, 128)
	defer pool.close()
	if err := pool.load(&VM{engine: engine}, module.Globals["routes"]); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(pool)
	defer server.Close()
	var requests sync.WaitGroup
	for range 100 {
		requests.Go(func() {
			response, err := server.Client().Get(server.URL + "/v1/users/search?tag=concurrent")
			if err != nil {
				t.Error(err)
				return
			}
			response.Body.Close()
			if response.StatusCode != 200 {
				t.Errorf("route failed on preloaded worker: %d", response.StatusCode)
			}
		})
	}
	requests.Wait()
}
