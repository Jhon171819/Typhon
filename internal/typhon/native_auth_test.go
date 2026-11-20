package typhon

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

func TestNativeJWTAuthentication(t *testing.T) {
	first, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	second, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var keySet atomic.Pointer[jose.JSONWebKeySet]
	var fetches atomic.Int32
	var block atomic.Bool
	requested := make(chan struct{}, 1)
	jwks := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		fetches.Add(1)
		if block.Load() {
			requested <- struct{}{}
			<-req.Context().Done()
			return
		}
		if keys := keySet.Load(); keys != nil {
			if err := json.NewEncoder(w).Encode(keys); err != nil {
				t.Error(err)
			}
		} else {
			w.WriteHeader(503)
		}
	}))
	defer jwks.Close()
	keySet.Store(&jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &first.PublicKey, KeyID: "old"}}})
	engine := NewEngine()
	defer engine.Close()
	module, err := engine.LoadModule(sourceFile(t, fmt.Sprintf(`import typhon.http as http
import typhon.web as web
import typhon.auth as auth
verifier: auth.Authenticator = auth.jwt('https://issuer.example', 'orders-api', %q)
r: web.Router = web.router()
r.use_errors()
r.use_logging()
def identity(req: http.Request, next: web.Next) -> http.Response:
    if req.path == '/private':
        return http.json(200, [req.identity, req.tenant])
    return next.call()
def public(req: http.Request) -> http.Response:
    return http.json(200, [req.identity, req.tenant])
r.use(identity)
r.add('GET', '/public', public)
r.add('GET', '/private', public)
r.require_auth('GET', '/private', verifier)
`, jwks.URL)))
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ExecuteModule(module, false); err != nil {
		t.Fatal(err)
	}
	verifier := module.Globals["verifier"].NativeObject.data.(*jwtAuthenticator)
	verifier.client = jwks.Client()
	now := time.Now().Truncate(time.Second)
	verifier.now = func() time.Time { return now }
	sign := func(t *testing.T, algorithm jose.SignatureAlgorithm, key any, id, typ string, claims jwt.Claims) string {
		t.Helper()
		options := (&jose.SignerOptions{}).WithType(jose.ContentType(typ)).WithHeader("kid", id)
		signer, err := jose.NewSigner(jose.SigningKey{Algorithm: algorithm, Key: key}, options)
		if err != nil {
			t.Fatal(err)
		}
		token, err := jwt.Signed(signer).Claims(claims).Claims(map[string]any{"tenant": "workspace"}).Serialize()
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	claims := jwt.Claims{
		Issuer: "https://issuer.example", Audience: jwt.Audience{"orders-api"}, Subject: "alice",
		Expiry: jwt.NewNumericDate(now.Add(10 * time.Minute)), NotBefore: jwt.NewNumericDate(now),
	}
	valid := sign(t, jose.RS256, first, "old", "at+jwt", claims)
	handler := httpVMHandler(engine, module.Globals["r"])
	serve := func(path string, tokens ...string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		for _, token := range tokens {
			req.Header.Add("Authorization", "Bearer "+token)
		}
		req.Header.Set("X-Identity", "untrusted")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	var logged bytes.Buffer
	oldLog := log.Writer()
	log.SetOutput(&logged)
	defer log.SetOutput(oldLog)
	if rec := serve("/public", valid); rec.Code != 200 || rec.Body.String() != "[null, null]" {
		t.Fatalf("public request identity: %d %s", rec.Code, rec.Body)
	}
	if rec := serve("/private", valid); rec.Code != 200 || rec.Body.String() != "[\"alice\", \"workspace\"]" {
		t.Fatalf("verified identity: %d %s", rec.Code, rec.Body)
	}
	assertUnauthorized := func(t *testing.T, tokens ...string) {
		t.Helper()
		rec := serve("/private", tokens...)
		readTestProblem(t, rec, 401, "unauthorized")
		if rec.Header().Get("WWW-Authenticate") != "Bearer" {
			t.Fatal("missing bearer challenge")
		}
	}
	assertUnauthorized(t)
	assertUnauthorized(t, valid, valid)
	for _, tc := range []struct {
		name          string
		change        func(*jwt.Claims)
		alg           jose.SignatureAlgorithm
		key           any
		id, tokenType string
	}{
		{"expiry", func(c *jwt.Claims) { c.Expiry = jwt.NewNumericDate(now) }, jose.RS256, first, "old", "at+jwt"},
		{"missing expiry", func(c *jwt.Claims) { c.Expiry = nil }, jose.RS256, first, "old", "at+jwt"},
		{"not before", func(c *jwt.Claims) { c.NotBefore = jwt.NewNumericDate(now.Add(time.Minute)) }, jose.RS256, first, "old", "at+jwt"},
		{"issuer", func(c *jwt.Claims) { c.Issuer = "https://another.example" }, jose.RS256, first, "old", "at+jwt"},
		{"audience", func(c *jwt.Claims) { c.Audience = jwt.Audience{"some-app"} }, jose.RS256, first, "old", "at+jwt"},
		{"missing subject", func(c *jwt.Claims) { c.Subject = "" }, jose.RS256, first, "old", "at+jwt"},
		{"signature", nil, jose.RS256, second, "old", "at+jwt"},
		{"symmetric algorithm", nil, jose.HS256, make([]byte, 32), "old", "at+jwt"},
		{"unknown kid", nil, jose.RS256, first, "unknown", "at+jwt"},
		{"missing kid", nil, jose.RS256, first, "", "at+jwt"},
		{"different purpose", nil, jose.RS256, first, "old", "JWT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := claims
			if tc.change != nil {
				tc.change(&c)
			}
			assertUnauthorized(t, sign(t, tc.alg, tc.key, tc.id, tc.tokenType, c))
		})
	}
	assertUnauthorized(t, "eyJhbGciOiJub25lIiwia2lkIjoib2xkIiwidHlwIjoiYXQrand0In0.e30.")
	if got := fetches.Load(); got != 1 {
		t.Fatalf("invalid/unknown tokens bypassed refresh cooldown: %d fetches", got)
	}

	now = now.Add(2 * time.Second)
	keySet.Store(&jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &second.PublicKey, KeyID: "new"}}})
	var concurrent sync.WaitGroup
	for i := range 12 {
		account := claims
		account.Subject = fmt.Sprintf("account-%d", i)
		token := sign(t, jose.RS256, second, "new", "at+jwt", account)
		concurrent.Go(func() {
			rec := serve("/private", token)
			if rec.Code != 200 || rec.Body.String() != fmt.Sprintf("[%q, \"workspace\"]", account.Subject) {
				t.Errorf("account isolation: %d %s", rec.Code, rec.Body)
			}
		})
	}
	concurrent.Wait()
	assertUnauthorized(t, valid)
	if got := fetches.Load(); got != 2 {
		t.Fatalf("rotation/concurrent cache used %d JWKS fetches", got)
	}

	keySet.Store(nil)
	now = now.Add(time.Minute)
	rotated := sign(t, jose.RS256, second, "new", "at+jwt", claims)
	readTestProblem(t, serve("/private", rotated), 503, "service_unavailable")
	if strings.Contains(logged.String(), valid) || strings.Contains(logged.String(), rotated) {
		t.Fatal("request log contains a raw bearer token")
	}

	now = now.Add(2 * time.Second)
	block.Store(true)
	req := httptest.NewRequest(http.MethodGet, "/private", nil)
	ctx, cancel := context.WithCancel(req.Context())
	defer cancel()
	req.Header.Set("Authorization", "Bearer "+rotated)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(rec, req.WithContext(ctx))
	}()
	select {
	case <-requested:
		cancel()
	case <-time.After(time.Second):
		t.Fatal("JWKS request did not start")
	}
	select {
	case <-done:
		readTestProblem(t, rec, 499, "client_closed_request")
	case <-time.After(time.Second):
		t.Fatal("cancelled JWKS lookup did not return")
	}
}
