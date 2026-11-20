package typhon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func readTestProblem(t *testing.T, response *httptest.ResponseRecorder, status int, code string) []map[string]string {
	t.Helper()
	if response.Code != status || response.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatalf("problem response %d %s: %s", response.Code, response.Header().Get("Content-Type"), response.Body)
	}
	var problem struct {
		Code      string              `json:"code"`
		Message   string              `json:"message"`
		Details   []map[string]string `json:"details"`
		RequestID string              `json:"request_id"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &problem); err != nil {
		t.Fatal(err)
	}
	if problem.Code != code || problem.RequestID == "" || problem.RequestID != response.Header().Get("X-Request-ID") || problem.Details == nil || problem.Message == "" {
		t.Fatalf("invalid envelope: %+v", problem)
	}
	if strings.Contains(response.Body.String(), "private-token") || strings.Contains(response.Body.String(), "SELECT") {
		t.Fatalf("unsafe response: %s", response.Body)
	}
	return problem.Details
}

func TestNativeHTTPProblemBoundaryAndRecovery(t *testing.T) {
	engine := NewEngine()
	defer engine.Close()
	module, err := engine.LoadModule(sourceFile(t, `import typhon.http as http
import typhon.web as web
import typhon.validation as v
schema: v.Schema = v.object({'name': v.text(1, 8)})
r: web.Router = web.router()
r.use_errors()
def create(req: http.Request) -> http.Response:
    issue: http.Response | None = web.validate(req, schema, req.json())
    if issue is not None:
        return issue
    return http.response(201, 'created')
def error(req: http.Request) -> http.Response:
    raise Error('private-token')
def timeout(req: http.Request) -> http.Response:
    raise TimeoutError('private-token')
def delay(req: http.Request) -> http.Response:
    while True:
        busy: int = 1
    return http.response(200, 'unreachable')
def explicit(req: http.Request) -> http.Response:
    raise web.reject(409)
def dependent(req: http.Request) -> http.Response:
    raise ConnectionError('private-token')
def denied(req: http.Request) -> http.Response:
    return web.problem(req, 403)
def unauthorized(req: http.Request) -> http.Response:
    raise web.reject(401)
def limited(req: http.Request) -> http.Response:
    raise web.reject(429)
r.add('POST', '/create', create)
r.add('GET', '/error', error)
r.add('GET', '/timeout', timeout)
r.add('GET', '/delay', delay)
r.add('GET', '/conflict', explicit)
r.add('GET', '/dependency', dependent)
r.add('GET', '/denied', denied)
r.add('GET', '/auth', unauthorized)
r.add('GET', '/limit', limited)
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ExecuteModule(module, false); err != nil {
		t.Fatal(err)
	}
	handler := httpVMHandler(engine, module.Globals["r"])
	var logs bytes.Buffer
	output := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(output)
	for _, tc := range []struct {
		method, path, body, code string
		status                   int
	}{
		{"POST", "/create", `{"name":private-token}`, "bad_request", 400},
		{"POST", "/create", `{"private-token":42}`, "validation_error", 422},
		{"GET", "/missing", "", "not_found", 404},
		{"GET", "/create", "", "method_not_allowed", 405},
		{"GET", "/error", "", "internal_error", 500},
		{"GET", "/timeout", "", "timeout", 504},
		{"GET", "/delay", "", "timeout", 504},
		{"GET", "/conflict", "", "conflict", 409},
		{"GET", "/dependency", "", "service_unavailable", 503},
		{"GET", "/denied", "", "forbidden", 403},
		{"GET", "/auth", "", "unauthorized", 401},
		{"GET", "/limit", "", "too_many_requests", 429},
	} {
		request := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		cancel := func() {}
		if tc.path == "/delay" {
			var ctx context.Context
			ctx, cancel = context.WithTimeout(request.Context(), 30*time.Millisecond)
			request = request.WithContext(ctx)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, request)
		cancel()
		details := readTestProblem(t, w, tc.status, tc.code)
		if tc.status == 422 && (len(details) != 2 || details[1]["path"] != "name" || details[1]["code"] != "missing_field") {
			t.Fatalf("unexpected schema details: %v", details)
		}
		if tc.status == 405 && w.Header().Get("Allow") != "OPTIONS, POST" {
			t.Fatalf("405 lost Allow: %v", w.Header())
		}
	}
	if strings.Contains(logs.String(), "private-token") || !strings.Contains(logs.String(), "route=\"/error\" status=500") {
		t.Fatalf("log lacked safe diagnostic context: %s", logs.String())
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/create", strings.NewReader(`{"name":"Ada"}`)))
	if w.Code != 201 || w.Body.String() != "created" {
		t.Fatalf("worker failed after a boundary error: %d %s", w.Code, w.Body)
	}
}

func TestNativePostgresHTTPProblemUniqueIntegration(t *testing.T) {
	dsn := os.Getenv("TYPHON_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("set TYPHON_TEST_POSTGRES_URL for a dedicated test PostgreSQL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(driverFailure("PostgreSQL", connectionKind, err))
	}
	table := pgx.Identifier{fmt.Sprintf("typhon_conflict_%d", time.Now().UnixNano())}.Sanitize()
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), cleanupTimeout)
		defer stop()
		if _, err := admin.Exec(cleanup, "DROP TABLE IF EXISTS "+table); err != nil {
			t.Error(driverFailure("PostgreSQL", databaseKind, err))
		}
		if err := admin.Close(cleanup); err != nil {
			t.Error(driverFailure("PostgreSQL", connectionKind, err))
		}
	})
	if _, err := admin.Exec(ctx, "CREATE TABLE "+table+" (key text PRIMARY KEY)"); err != nil {
		t.Fatal(driverFailure("PostgreSQL", databaseKind, err))
	}
	engine := NewEngine()
	defer engine.Close()
	module, err := engine.LoadModule(sourceFile(t, fmt.Sprintf(`import typhon.http as http
import typhon.postgres as pg
import typhon.web as web
db: pg.Connection = pg.connect(%q)
r: web.Router = web.router()
r.use_errors()
def create(req: http.Request) -> http.Response:
    db.exec(%q, ['private-token'])
    return http.response(201, 'created')
r.add('POST', '/', create)
`, dsn, "INSERT INTO "+table+" (key) VALUES ($1)")))
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ExecuteModule(module, false); err != nil {
		t.Fatal(err)
	}
	handler := httpVMHandler(engine, module.Globals["r"])
	for _, status := range []int{201, 409} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/", nil))
		if status == 409 {
			readTestProblem(t, response, status, "conflict")
		} else if response.Code != status {
			t.Fatalf("insert returned %d %s", response.Code, response.Body)
		}
	}
}
