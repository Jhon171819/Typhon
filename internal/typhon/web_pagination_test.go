package typhon

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

type pageTestBody struct {
	Items []json.RawMessage `json:"items"`
	Limit int               `json:"limit"`
	Next  *string           `json:"next_cursor"`
	Links struct {
		Next *string `json:"next"`
	} `json:"links"`
}

func paginationKey(t *testing.T) string {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(key)
}

func requirePage(t *testing.T, rec *httptest.ResponseRecorder) pageTestBody {
	t.Helper()
	if rec.Code != 200 {
		t.Fatalf("page returned %d: %s", rec.Code, rec.Body)
	}
	var page pageTestBody
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	return page
}

func TestNativeHTTPPaginationValidationAndCursor(t *testing.T) {
	key := paginationKey(t)
	engine := NewEngine()
	defer engine.Close()
	source := fmt.Sprintf(`import typhon.http as http
import typhon.validation as v
import typhon.web as web
filters: v.Schema = v.object({'status': v.enumeration(['ready','done'])}, {'priority': v.integer(0, 20), 'active': v.boolean()})
pages: web.Paginator = web.paginator(%q, 'id_asc', filters, 2, 4, 30)
routes: web.Router = web.router()
routes.use_errors()
def handler(req: http.Request) -> http.Response:
    page: web.Page = pages.parse(req)
    payload: list[Any] = [[page.after, page.offset, page.filter('status'), page.filter('priority'), page.filter('active')]]
    return page.respond(payload, page.after + 10)
routes.add('GET', '/v1/list', handler)
routes.add('GET', '/v2/list', handler)
`, key)
	module, err := engine.LoadModule(sourceFile(t, source))
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ExecuteModule(module, false); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Second)
	module.Globals["pages"].NativeObject.data.(*webPaginator).now = func() time.Time { return now }
	handler := httpVMHandler(engine, module.Globals["routes"])
	request := func(path, identity, tenant string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		ctx := context.WithValue(req.Context(), verifiedIdentityKey{}, verifiedRequestIdentity{subject: identity, tenant: tenant})
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req.WithContext(ctx))
		return rec
	}
	for _, query := range []string{
		"limit=-1&status=ready", "limit=5&status=ready", "limit=2&limit=3&status=ready",
		"offset=10001&status=ready", "status=ready&new_filter=true", "status=ready&priority=1.5",
		"status=ready&active=0", "status=ready&order=id+DESC", "status=';DROP+TABLE+x;--",
		"priority=2", "status=%zz", "status=ready&cursor=garbage",
	} {
		readTestProblem(t, request("/v1/list?"+query, "alice", "a"), 400, "bad_request")
	}
	start := requirePage(t, request("/v1/list?offset=2&status=ready&priority=3&active=true", "alice", "a"))
	if start.Limit != 2 || string(start.Items[0]) != `[0, 2, "ready", 3, true]` || start.Next == nil || start.Links.Next == nil ||
		strings.Contains(*start.Links.Next, "offset=") || !strings.Contains(*start.Links.Next, "order=id_asc") {
		t.Fatalf("incorrect first page: %+v", start)
	}
	second := requirePage(t, request(*start.Links.Next, "alice", "a"))
	if string(second.Items[0]) != `[10, 0, "ready", 3, true]` {
		t.Fatalf("cursor did not advance: %s", second.Items[0])
	}
	replacement := "A"
	if strings.HasPrefix(*start.Next, replacement) {
		replacement = "B"
	}
	for _, tc := range []struct{ path, identity, tenant string }{
		{*start.Links.Next, "alice", "b"},
		{*start.Links.Next, "bob", "a"},
		{strings.Replace(*start.Links.Next, "/v1/", "/v2/", 1), "alice", "a"},
		{strings.Replace(*start.Links.Next, "priority=3", "priority=4", 1), "alice", "a"},
		{*start.Links.Next + "&offset=0", "alice", "a"},
		{strings.Replace(*start.Links.Next, *start.Next, replacement+(*start.Next)[1:], 1), "alice", "a"},
	} {
		readTestProblem(t, request(tc.path, tc.identity, tc.tenant), 400, "bad_request")
	}
	now = now.Add(30 * time.Second)
	readTestProblem(t, request(*start.Links.Next, "alice", "a"), 400, "bad_request")
}

func TestNativeHTTPPaginationPostgresIntegration(t *testing.T) {
	dsn := os.Getenv("TYPHON_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("set TYPHON_TEST_POSTGRES_URL to exercise the published pagination example against PostgreSQL")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
	defer cancel()
	db, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(driverFailure("PostgreSQL", connectionKind, err))
	}
	schema := "typhon_pages_" + strings.ToLower(rand.Text())
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), cleanupTimeout)
		defer stop()
		if _, err := db.Exec(cleanup, "DROP SCHEMA IF EXISTS "+quotedSchema+" CASCADE"); err != nil {
			t.Error(driverFailure("PostgreSQL", databaseKind, err))
		}
		if err := db.Close(cleanup); err != nil {
			t.Error(driverFailure("PostgreSQL", connectionKind, err))
		}
	})
	table := pgx.Identifier{schema, "appointments"}.Sanitize()
	for _, sql := range []string{
		"CREATE SCHEMA " + quotedSchema,
		"CREATE TABLE " + table + " (tenant_id text NOT NULL, id bigint NOT NULL, status text NOT NULL, PRIMARY KEY (tenant_id,id))",
	} {
		if _, err := db.Exec(ctx, sql); err != nil {
			t.Fatal(driverFailure("PostgreSQL", databaseKind, err))
		}
	}
	insert := "INSERT INTO " + table + " (tenant_id,id,status) VALUES ($1,$2,$3)"
	for _, id := range []int64{10, 30, 50} {
		if _, err := db.Exec(ctx, insert, "tenant-a", id, "scheduled"); err != nil {
			t.Fatal(driverFailure("PostgreSQL", databaseKind, err))
		}
	}
	if _, err := db.Exec(ctx, insert, "tenant-b", int64(35), "scheduled"); err != nil {
		t.Fatal(driverFailure("PostgreSQL", databaseKind, err))
	}
	database, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(driverFailure("PostgreSQL", validationKind, err))
	}
	opts := database.Query()
	opts.Set("search_path", schema)
	database.RawQuery = opts.Encode()
	t.Setenv("DATABASE_URL", database.String())
	t.Setenv("AUTH_ISSUER", "https://issuer.example")
	t.Setenv("AUTH_AUDIENCE", "appointments")
	t.Setenv("AUTH_JWKS_URL", "https://issuer.example/jwks")
	t.Setenv("PAGINATION_KEY", paginationKey(t))
	engine := NewEngine()
	defer engine.Close()
	file := filepath.Join("..", "..", "examples", "http_paginated_appointments.ty")
	module, err := engine.LoadModule(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ExecuteModule(module, false); err != nil {
		t.Fatal(err)
	}
	router, _ := routerValue(module.Globals["routes"])
	document, err := router.openAPI()
	if err != nil || !json.Valid(document) {
		t.Fatalf("could not export pagination OpenAPI document: %v", err)
	}
	for _, field := range []string{`"name": "cursor"`, `"name": "status"`, `"maximum": 100`, `"bearerAuth"`} {
		if !strings.Contains(string(document), field) {
			t.Fatalf("pagination OpenAPI document omitted %s", field)
		}
	}
	router.routes[[2]string{"GET", "/v1/appointments"}].auth = rateFixtureAuth{
		"a": {subject: "professional", tenant: "tenant-a", roles: []string{"professional"}},
		"b": {subject: "professional", tenant: "tenant-b", roles: []string{"professional"}},
	}
	handler := httpVMHandler(engine, module.Globals["routes"])
	request := func(path, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Tenant", "tenant-b")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	ids := func(page pageTestBody) []int64 {
		t.Helper()
		ids := make([]int64, 0, len(page.Items))
		for _, row := range page.Items {
			var item struct {
				ID int64 `json:"id"`
			}
			if err := json.Unmarshal(row, &item); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, item.ID)
		}
		return ids
	}
	first := requirePage(t, request("/v1/appointments?limit=2&status=scheduled", "a"))
	if !slices.Equal(ids(first), []int64{10, 30}) || first.Links.Next == nil {
		t.Fatal("the first page was not ordered by id or exceeded its limit")
	}
	// An independent PostgreSQL session inserts on both sides of the cursor.
	for _, id := range []int64{20, 40} {
		if _, err := db.Exec(ctx, insert, "tenant-a", id, "scheduled"); err != nil {
			t.Fatal(driverFailure("PostgreSQL", databaseKind, err))
		}
	}
	second := requirePage(t, request(*first.Links.Next, "a"))
	if !slices.Equal(ids(second), []int64{40, 50}) || second.Next != nil || second.Links.Next != nil {
		t.Fatalf("cursor duplicated records, crossed tenant or failed to advance: %v", ids(second))
	}
	readTestProblem(t, request(*first.Links.Next, "b"), 400, "bad_request")
	readTestProblem(t, request("/v1/appointments?status=%27%3BDELETE%20FROM%20appointments%3B--", "a"), 400, "bad_request")
	readTestProblem(t, request("/v1/appointments?order=id%20DESC%3B--", "a"), 400, "bad_request")
	if got := ids(requirePage(t, request("/v1/appointments?limit=1&offset=1&status=scheduled", "a"))); !slices.Equal(got, []int64{20}) {
		t.Fatalf("unexpected offset result: %v", got)
	}
}
