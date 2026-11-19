package typhon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeOpenAPIContract(t *testing.T) {
	t.Setenv("TYPHON_DOCS", "")
	path := filepath.Join("..", "..", "examples", "http_openapi.ty")
	document, err := OpenAPIFile(t.Context(), path, "routes")
	if err != nil {
		t.Fatal(err)
	}
	again, err := OpenAPIFile(t.Context(), path, "routes")
	if err != nil || !bytes.Equal(document, again) {
		t.Fatalf("OpenAPI output changed between exports: %v", err)
	}
	var contract struct {
		OpenAPI string                               `json:"openapi"`
		Paths   map[string]map[string]map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(document, &contract); err != nil {
		t.Fatal(err)
	}
	if contract.OpenAPI != "3.1.0" || len(contract.Paths) != 2 || contract.Paths["/internal/ready"] != nil {
		t.Fatalf("unexpected exported paths: %+v", contract.Paths)
	}
	list := contract.Paths["/records"]["get"]
	read := contract.Paths["/records/{id}"]["get"]
	create := contract.Paths["/records/{id}"]["post"]
	if list["parameters"].([]any)[0].(map[string]any)["in"] != "query" ||
		read["security"] == nil || create["security"] != nil ||
		create["parameters"].([]any)[0].(map[string]any)["required"] != true {
		t.Fatal("path, query, or security metadata drifted")
	}
	body := create["requestBody"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)
	if body["required"].([]any)[0] != "name" || body["additionalProperties"] != false {
		t.Fatal("export missed required validation constraints")
	}
	for _, code := range []string{"400", "409", "422"} {
		response := create["responses"].(map[string]any)[code].(map[string]any)
		if response["content"].(map[string]any)["application/problem+json"] == nil {
			t.Fatalf("missing problem schema for %s", code)
		}
	}
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	withoutList := strings.Replace(string(source),
		"routes.document('GET', '/records', 'listRecords', ['records'], {200: v.array(record_response), 422: None}, None, list_query)", "", 1)
	changed, err := OpenAPIFile(t.Context(), sourceFile(t, withoutList), "routes")
	if err != nil || bytes.Equal(changed, document) || bytes.Contains(changed, []byte("listRecords")) {
		t.Fatalf("removing the documented route did not update the output: %v", err)
	}
	engine := NewEngine()
	defer engine.Close()
	module, err := engine.LoadModule(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ExecuteModule(module, false); err != nil {
		t.Fatal(err)
	}
	router, _ := routerValue(module.Globals["routes"])
	handler, err := router.docsHandler(httpVMHandler(engine, module.Globals["routes"]))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		method, path, body string
		code               int
	}{
		{"POST", "/records/id", "{", 400}, {"POST", "/records/id", "{}", 422},
		{"POST", "/records/id", `{"name":"Ada"}`, 201}, {"POST", "/records/id", `{"name":"Ada"}`, 409},
		{"GET", "/records/id", "", 401}, {"GET", "/records?name=Ada", "", 200},
		{"PUT", "/records/id", `{"name":"Updated"}`, 200}, {"DELETE", "/records/id", "", 204},
		{"GET", "/internal/ready", "", 204}, {"GET", "/docs", "", 404}, {"GET", "/openapi.json", "", 404},
	} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body)))
		if rec.Code != tc.code {
			t.Fatalf("%s %s: expected %d, got %d %s", tc.method, tc.path, tc.code, rec.Code, rec.Body)
		}
	}
}

func TestNativeOpenAPIInteractiveAndValidation(t *testing.T) {
	t.Setenv("TYPHON_DOCS", "1")
	path := filepath.Join("..", "..", "examples", "http_openapi.ty")
	engine := NewEngine()
	defer engine.Close()
	module, err := engine.LoadModule(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ExecuteModule(module, false); err != nil {
		t.Fatal(err)
	}
	router, _ := routerValue(module.Globals["routes"])
	handler, err := router.docsHandler(httpVMHandler(engine, module.Globals["routes"]))
	if err != nil {
		t.Fatal(err)
	}
	ui, spec, head := httptest.NewRecorder(), httptest.NewRecorder(), httptest.NewRecorder()
	handler.ServeHTTP(ui, httptest.NewRequest("GET", "/docs", nil))
	handler.ServeHTTP(spec, httptest.NewRequest("GET", "/openapi.json", nil))
	handler.ServeHTTP(head, httptest.NewRequest("HEAD", "/openapi.json", nil))
	if ui.Code != 200 || !strings.Contains(ui.Body.String(), "redoc@2.5.3") || ui.Header().Get("Content-Security-Policy") == "" ||
		spec.Code != 200 || !json.Valid(spec.Body.Bytes()) || head.Code != 200 || head.Body.Len() != 0 {
		t.Fatal("interactive OpenAPI paths did not serve the precomputed resources")
	}

	const source = `import typhon.http as http
import typhon.web as web
routes: web.Router = web.router()
def handler(req: http.Request) -> http.Response:
    return http.response(204, '')
routes.add('GET', '/a/{id}', handler)
routes.add('POST', '/a/{other}', handler)
routes.document('GET', '/a/{id}', 'operation', [], {204: None})
`
	for _, tc := range []struct {
		append string
		want   string
	}{
		{"routes.document('POST', '/a/{other}', 'operation', [], {204: None})\n", "duplicate OpenAPI operation identifier"},
		{"routes.document('POST', '/a/{other}', 'alternate', [], {204: None})\n", "different names"},
		{"http.serve('127.0.0.1:0', routes)\n", "main module guard"},
	} {
		_, err := OpenAPIFile(context.Background(), sourceFile(t, source+tc.append), "routes")
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("expected %q, got %v", tc.want, err)
		}
	}
}
