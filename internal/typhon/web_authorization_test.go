package typhon

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/jackc/pgx/v5"
)

func TestNativeAuthorizationPolicy(t *testing.T) {
	roles, scopes := DictValue(), DictValue()
	actions := ListValue([]Value{StringValue("write")})
	if err := roles.Dict.put(StringValue("admin"), actions); err != nil {
		t.Fatal(err)
	}
	if err := scopes.Dict.put(StringValue("write"), StringValue("edit")); err != nil {
		t.Fatal(err)
	}
	p, err := newWebPolicy([]Value{roles, scopes})
	if err != nil {
		t.Fatal(err)
	}
	(*actions.List)[0] = StringValue("read")
	principal := verifiedRequestIdentity{
		subject: "a", tenant: "tenant-a", roles: []string{"admin"}, scopes: map[string]bool{"edit": true},
	}
	if !p.allows(principal, "write") || p.allows(principal, "read") {
		t.Fatal("policy did not copy configured permissions or deny unknown actions")
	}
	principal.scopes = nil
	if p.allows(principal, "write") {
		t.Fatal("policy ignored the configured scope")
	}
	principal.scopes = map[string]bool{"edit": true}
	principal.tenant = ""
	if p.allows(principal, "write") || p.allows(verifiedRequestIdentity{}, "write") {
		t.Fatal("policy ignored missing verified tenant or subject")
	}
}

func TestNativeTenantAuthorizationPostgresIntegration(t *testing.T) {
	dsn := os.Getenv("TYPHON_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("set TYPHON_TEST_POSTGRES_URL to exercise the tenant authorization example on PostgreSQL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(driverFailure("PostgreSQL", connectionKind, err))
	}
	schema := "typhon_auth_" + strings.ToLower(rand.Text())
	quoted := pgx.Identifier{schema}.Sanitize()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		if _, err := admin.Exec(ctx, "DROP SCHEMA IF EXISTS "+quoted+" CASCADE"); err != nil {
			t.Error(driverFailure("PostgreSQL", databaseKind, err))
		}
		if err := admin.Close(ctx); err != nil {
			t.Error(driverFailure("PostgreSQL", connectionKind, err))
		}
	})
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(driverFailure("PostgreSQL", databaseKind, err))
	}
	table := pgx.Identifier{schema, "records"}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE TABLE "+table+
		" (tenant_id text NOT NULL, id text NOT NULL, owner_id text NOT NULL, content text NOT NULL, PRIMARY KEY (tenant_id,id))"); err != nil {
		t.Fatal(driverFailure("PostgreSQL", databaseKind, err))
	}
	insertSQL := "INSERT INTO " + table + " (tenant_id, id, owner_id, content) VALUES ($1, $2, $3, $4)"
	for _, row := range [][4]string{
		{"tenant-a", "owned", "alice", "alice-content"},
		{"tenant-a", "coworker", "colleague", "coworker-content"},
		{"tenant-b", "foreign", "alice", "other-tenant-content"},
	} {
		if _, err := admin.Exec(ctx, insertSQL, row[0], row[1], row[2], row[3]); err != nil {
			t.Fatal(driverFailure("PostgreSQL", databaseKind, err))
		}
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(driverFailure("PostgreSQL", validationKind, err))
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	t.Setenv("DATABASE_URL", parsed.String())
	t.Setenv("AUTH_ISSUER", "https://issuer.example.org")
	t.Setenv("AUTH_AUDIENCE", "records")

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	foreignKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwks := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		keys := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "test"}}}
		if err := json.NewEncoder(w).Encode(keys); err != nil {
			t.Error(err)
		}
	}))
	defer jwks.Close()
	t.Setenv("AUTH_JWKS_URL", jwks.URL)
	token := func(key *rsa.PrivateKey, tenant, role, scope string) string {
		t.Helper()
		options := (&jose.SignerOptions{}).WithType("at+jwt").WithHeader("kid", "test")
		signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, options)
		if err != nil {
			t.Fatal(err)
		}
		compact, err := jwt.Signed(signer).Claims(jwt.Claims{
			Issuer: "https://issuer.example.org", Subject: "alice", Audience: jwt.Audience{"records"},
			Expiry: jwt.NewNumericDate(time.Now().Add(time.Minute)),
		}).Claims(map[string]any{"tenant": tenant, "roles": []string{role}, "scope": scope}).Serialize()
		if err != nil {
			t.Fatal(err)
		}
		return compact
	}
	engine := NewEngine()
	defer engine.Close()
	module, err := engine.LoadModule(filepath.Join("..", "..", "examples", "http_tenant_authorization.ty"))
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ExecuteModule(module, false); err != nil {
		t.Fatal(err)
	}
	module.Globals["verifier"].NativeObject.data.(*jwtAuthenticator).client = jwks.Client()
	handler := httpVMHandler(engine, module.Globals["routes"])
	user := token(key, "tenant-a", "user", "records:write")
	professional := token(key, "tenant-a", "professional", "")
	adminNoScope := token(key, "tenant-a", "admin", "")
	adminWithScope := token(key, "tenant-a", "admin", "records:write")
	forged := token(foreignKey, "tenant-b", "admin", "records:write")
	missingTenant := token(key, "", "admin", "records:write")
	for _, tc := range []struct {
		method, id, token, body string
		status                  int
	}{
		{"GET", "owned", user, "alice-content", 200},
		{"GET", "foreign", user, "", 404},
		{"GET", "coworker", user, "", 404},
		{"GET", "absent", user, "", 404},
		{"POST", "new", user, "", 403},
		{"DELETE", "owned", user, "", 403},
		{"DELETE", "owned", adminNoScope, "", 403},
		{"DELETE", "foreign", adminWithScope, "", 404},
		{"GET", "foreign", forged, "", 401},
		{"GET", "owned", missingTenant, "", 403},
		{"POST", "new", professional, "created", 201},
		{"GET", "new", user, "written-content", 200},
		{"DELETE", "new", adminWithScope, "", 204},
		{"GET", "new", user, "", 404},
	} {
		req := httptest.NewRequest(tc.method, "/records/"+tc.id, strings.NewReader("written-content"))
		req.Header.Set("Authorization", "Bearer "+tc.token)
		req.Header.Set("X-Tenant", "tenant-b")
		req.Header.Set("X-Role", "admin")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.body) ||
			strings.Contains(rec.Body.String(), "other-tenant-content") || strings.Contains(rec.Body.String(), "coworker-content") {
			t.Errorf("%s %s: expected %d, got %d %s", tc.method, tc.id, tc.status, rec.Code, rec.Body)
		}
	}
	var remaining int
	if err := admin.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&remaining); err != nil {
		t.Fatal(driverFailure("PostgreSQL", databaseKind, err))
	}
	if remaining != 3 {
		t.Fatalf("expected all original rows, got %d", remaining)
	}
	log.Print("PostgreSQL tenant authorization verified against the published example")
}
