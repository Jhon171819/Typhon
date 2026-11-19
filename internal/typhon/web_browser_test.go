package typhon

import (
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func TestNativeHTTPBrowserPolicy(t *testing.T) {
	engine := NewEngine()
	defer engine.Close()
	module, err := engine.LoadModule(sourceFile(t, `import typhon.http as http
import typhon.web as web
r: web.Router = web.router()
r.use_errors()
r.use_cors(['https://ui.example.org'], ['POST'], ['Content-Type', 'Authorization'], True)
r.use_security_headers("default-src 'none'", 'DENY', 3600)
events: list[str] = []
def handle(req: http.Request) -> http.Response:
    events.append('ran')
    cookie: str = web.session_cookie('__Host-session', 'opaque-id', 'None', 600)
    return http.response(201, 'created', [
        ['Set-Cookie', cookie],
        ['Vary', 'Accept'],
        ['Access-Control-Allow-Origin', '*'],
        ['Strict-Transport-Security', 'max-age=0']
    ])
r.add('POST', '/submit', handle)
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ExecuteModule(module, false); err != nil {
		t.Fatal(err)
	}
	handler := httpVMHandler(engine, module.Globals["r"])
	corsRequest := func(method, origin, wantedMethod, wantedHeaders string, tls bool) *httptest.ResponseRecorder {
		t.Helper()
		url := "http://api.example.org/submit"
		if tls {
			url = "https://api.example.org/submit"
		}
		req := httptest.NewRequest(method, url, nil)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if wantedMethod != "" {
			req.Header.Set("Access-Control-Request-Method", wantedMethod)
		}
		if wantedHeaders != "" {
			req.Header.Set("Access-Control-Request-Headers", wantedHeaders)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	preflight := corsRequest("OPTIONS", "https://ui.example.org", "POST", "Authorization, content-type", true)
	headers := preflight.Header()
	if preflight.Code != 204 || preflight.Body.Len() != 0 ||
		headers.Get("Access-Control-Allow-Origin") != "https://ui.example.org" ||
		headers.Get("Access-Control-Allow-Credentials") != "true" ||
		headers.Get("Access-Control-Allow-Methods") != "POST" ||
		headers.Get("Access-Control-Allow-Headers") != "authorization, content-type" ||
		!slices.Contains(headers.Values("Vary"), "Origin") ||
		!slices.Contains(headers.Values("Vary"), "Access-Control-Request-Method") ||
		!slices.Contains(headers.Values("Vary"), "Access-Control-Request-Headers") ||
		headers.Get("Content-Security-Policy") != "default-src 'none'" ||
		headers.Get("X-Frame-Options") != "DENY" || headers.Get("X-Content-Type-Options") != "nosniff" ||
		headers.Get("Strict-Transport-Security") != "max-age=3600" {
		t.Fatalf("incorrect preflight %d %v", preflight.Code, headers)
	}
	for _, tc := range [][3]string{
		{"https://untrusted.example.org", "POST", "authorization"},
		{"", "POST", "authorization"},
		{"https://ui.example.org", "DELETE", "authorization"},
		{"https://ui.example.org", "POST", "x-not-allowed"},
		{"https://ui.example.org", "POST", "authorization\r\nX-Fake: yes"},
	} {
		denied := corsRequest("OPTIONS", tc[0], tc[1], tc[2], false)
		readTestProblem(t, denied, 403, "forbidden")
		if denied.Header().Get("Access-Control-Allow-Origin") != "" ||
			denied.Header().Get("Access-Control-Allow-Credentials") != "" {
			t.Fatalf("denied preflight returned CORS credentials: %v", denied.Header())
		}
	}
	if len(*module.Globals["events"].List) != 0 {
		t.Fatal("a preflight executed an application handler")
	}

	success := corsRequest("POST", "https://ui.example.org", "", "", true)
	if success.Code != 201 || success.Header().Get("Access-Control-Allow-Origin") != "https://ui.example.org" ||
		success.Header().Get("Strict-Transport-Security") != "max-age=3600" ||
		!slices.Contains(success.Header().Values("Vary"), "Accept") {
		t.Fatalf("invalid actual response: %d %v", success.Code, success.Header())
	}
	cookies := success.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != "__Host-session" || !cookies[0].HttpOnly ||
		!cookies[0].Secure || cookies[0].SameSite != http.SameSiteNoneMode || cookies[0].Path != "/" ||
		cookies[0].Domain != "" || cookies[0].MaxAge != 600 {
		t.Fatalf("invalid browser session cookie: %#v", cookies)
	}
	denied := corsRequest("POST", "https://untrusted.example.org", "", "", false)
	if denied.Code != 201 || denied.Header().Get("Access-Control-Allow-Origin") != "" ||
		denied.Header().Get("Strict-Transport-Security") != "" {
		t.Fatalf("handler overrode the transport policy: %d %v", denied.Code, denied.Header())
	}
}

func TestNativeHTTPCORSConfigAndAdmission(t *testing.T) {
	engine := NewEngine()
	defer engine.Close()
	const config = `import typhon.web as web
r: web.Router = web.router()
`
	for _, line := range []string{
		`r.use_cors(['*'], ['POST'], [], True)`,
		`r.use_cors(['https://ui.example.org/path'], ['POST'], [])`,
		`r.use_cors(['https://ui.example.org'], ['POST\r\nGET'], [])`,
		`r.use_cors(['https://ui.example.org'], ['POST'], ['X-Test\r\nX-Foo'])`,
		`r.use_security_headers('default-src *\r\nX-Foo: bar')`,
		`web.session_cookie('Session', 'id\r\nX-Foo: bar')`,
	} {
		module, err := engine.LoadModule(sourceFile(t, config+line+"\n"))
		if err != nil {
			t.Fatal(err)
		}
		if err := engine.ExecuteModule(module, false); err == nil {
			t.Fatalf("unsafe configuration accepted: %s", line)
		}
	}

	p, err := newWebCORS([]Value{
		ListValue([]Value{StringValue("*")}), ListValue([]Value{StringValue("POST")}), ListValue(nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	pool := newHTTPWorkerPool(t.Context(), 1, 0)
	pool.admission <- struct{}{}
	server := httptest.NewServer(webBrowserPolicy{cors: p}.wrap(pool, false))
	defer server.Close()
	for _, tc := range []struct {
		method string
		status int
	}{{"OPTIONS", 204}, {"POST", 503}} {
		req, err := http.NewRequest(tc.method, server.URL, strings.NewReader(""))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Origin", "https://ui.example.org")
		if tc.method == "OPTIONS" {
			req.Header.Set("Access-Control-Request-Method", "POST")
		}
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, readErr := io.Copy(io.Discard, resp.Body)
		closeErr := resp.Body.Close()
		if resp.StatusCode != tc.status || resp.Header.Get("Access-Control-Allow-Origin") != "*" ||
			resp.Header.Get("Access-Control-Allow-Credentials") != "" || readErr != nil || closeErr != nil {
			t.Fatalf("%s: %d %v %v %v", tc.method, resp.StatusCode, resp.Header, readErr, closeErr)
		}
	}
}
