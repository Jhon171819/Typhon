package typhon

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

const authenticatorTypeName = "typhon.auth.Authenticator"
const maxBearerBytes = 8192
const maxJWKSBytes = 64 << 10

var errInvalidBearer = errors.New("missing or invalid bearer credentials")
var errJWKSUnavailable = errors.New("JWT signing keys unavailable")

type webAuthenticator interface {
	verify(context.Context, string) (verifiedRequestIdentity, error)
}

type jwtAuthenticator struct {
	issuer, audience, endpoint string
	ttl                        time.Duration
	client                     *http.Client
	now                        func() time.Time

	// Each authenticator is owned by one Engine. Lookups run under execMu.
	keys       map[string]*rsa.PublicKey
	expires    time.Time
	refreshAt  time.Time
	refreshErr error
}

func authDriverTypes() map[string]Type {
	auth := Type{Kind: TypeNative, Name: authenticatorTypeName}
	return map[string]Type{
		"Authenticator": auth,
		"jwt":           nativeFuncType("auth.jwt", []Type{StrType(), StrType(), StrType(), IntType()}, auth, 1),
	}
}

func authDriverExports(types map[string]Type) map[string]Value {
	exports := driverTypeSymbols(types)
	exports["jwt"] = driverFunc(types["jwt"], func(vm *VM, args []Value) (Value, error) {
		if vm.request != nil {
			return NoneValue(), errors.New("configure JWT authentication during Engine initialization")
		}
		issuer, audience := args[0].Str, args[1].Str
		endpoint, err := url.Parse(args[2].Str)
		if err != nil || len(args[2].Str) > 2048 || endpoint.Scheme != "https" || endpoint.Hostname() == "" ||
			endpoint.User != nil || endpoint.Fragment != "" {
			return NoneValue(), errors.New("JWT signing keys require a configured HTTPS URL")
		}
		if !validAuthText(issuer, 512) || !validAuthText(audience, 512) {
			return NoneValue(), errors.New("JWT issuer and audience must be configured")
		}
		seconds := int64(60)
		if len(args) == 4 {
			seconds = args[3].Int
		}
		if seconds < 1 || seconds > 300 {
			return NoneValue(), errors.New("JWT key cache TTL must be between 1 and 300 seconds")
		}
		transport := &http.Transport{ForceAttemptHTTP2: true, MaxIdleConns: 2, IdleConnTimeout: 90 * time.Second}
		auth := &jwtAuthenticator{
			issuer: issuer, audience: audience, endpoint: endpoint.String(),
			ttl: time.Duration(seconds) * time.Second, now: time.Now,
			client: &http.Client{
				Transport: transport,
				CheckRedirect: func(*http.Request, []*http.Request) error {
					return http.ErrUseLastResponse
				},
			},
		}
		if err := vm.resourceScope().add(vm.context(), auth, "JWT signing keys", func(context.Context) error {
			transport.CloseIdleConnections()
			return nil
		}); err != nil {
			return NoneValue(), err
		}
		value := nativeObject(vm, types["Authenticator"], nil)
		value.NativeObject.data = auth
		return value, nil
	})
	return exports
}

func validAuthText(value string, maxBytes int) bool {
	return len(value) > 0 && len(value) <= maxBytes && utf8.ValidString(value) &&
		strings.IndexFunc(value, unicode.IsControl) < 0
}

func bearerToken(headers []string) string {
	if len(headers) != 1 || len(headers[0]) > maxBearerBytes+16 {
		return ""
	}
	scheme, token, ok := strings.Cut(headers[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimLeft(token, " ")
}

func (a *jwtAuthenticator) verify(ctx context.Context, token string) (verifiedRequestIdentity, error) {
	var empty verifiedRequestIdentity
	if len(token) == 0 || len(token) > maxBearerBytes || strings.Count(token, ".") != 2 ||
		strings.IndexFunc(token, func(c rune) bool {
			return !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.')
		}) >= 0 {
		return empty, errInvalidBearer
	}
	parsed, err := jwt.ParseSigned(token, []jose.SignatureAlgorithm{jose.RS256})
	if err != nil || len(parsed.Headers) != 1 {
		return empty, errInvalidBearer
	}
	header := parsed.Headers[0]
	typ, _ := header.ExtraHeaders["typ"].(string)
	if !validAuthText(header.KeyID, 128) || header.JSONWebKey != nil ||
		(!strings.EqualFold(typ, "at+jwt") && !strings.EqualFold(typ, "application/at+jwt")) {
		return empty, errInvalidBearer
	}
	key, err := a.key(ctx, header.KeyID)
	if err != nil {
		return empty, err
	}
	var claims jwt.Claims
	var extra struct {
		Tenant string `json:"tenant"`
	}
	if err := parsed.Claims(key, &claims, &extra); err != nil {
		return empty, errInvalidBearer
	}
	now := a.now()
	expected := jwt.Expected{Issuer: a.issuer, AnyAudience: jwt.Audience{a.audience}, Time: now}
	if claims.Expiry == nil || !now.Before(claims.Expiry.Time()) || claims.ValidateWithLeeway(expected, 0) != nil ||
		!validAuthText(claims.Subject, 256) || extra.Tenant != "" && !validAuthText(extra.Tenant, 256) {
		return empty, errInvalidBearer
	}
	return verifiedRequestIdentity{subject: claims.Subject, tenant: extra.Tenant}, nil
}

func (a *jwtAuthenticator) key(ctx context.Context, id string) (*rsa.PublicKey, error) {
	now := a.now()
	if a.expires.After(now) && a.keys[id] != nil {
		return a.keys[id], nil
	}
	if !now.Before(a.refreshAt) {
		err := a.refresh(ctx)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		a.refreshErr = err
		a.refreshAt = a.now().Add(time.Second)
	}
	if a.expires.After(a.now()) && a.keys[id] != nil {
		return a.keys[id], nil
	}
	if a.refreshErr != nil || !a.expires.After(a.now()) {
		return nil, errJWKSUnavailable
	}
	return nil, errInvalidBearer
}

func (a *jwtAuthenticator) refresh(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.endpoint, nil)
	if err != nil {
		return errJWKSUnavailable
	}
	response, err := a.client.Do(req)
	if err != nil {
		return errJWKSUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return errJWKSUnavailable
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxJWKSBytes+1))
	if err != nil || len(body) > maxJWKSBytes {
		return errJWKSUnavailable
	}
	var jwks jose.JSONWebKeySet
	if err := json.Unmarshal(body, &jwks); err != nil || len(jwks.Keys) > 32 {
		return errJWKSUnavailable
	}
	keys := make(map[string]*rsa.PublicKey)
	for _, jwk := range jwks.Keys {
		key, isRSA := jwk.Key.(*rsa.PublicKey)
		if !isRSA || jwk.Use != "" && jwk.Use != "sig" ||
			jwk.Algorithm != "" && jwk.Algorithm != string(jose.RS256) {
			continue
		}
		if !validAuthText(jwk.KeyID, 128) || key.N.BitLen() < 2048 || key.N.BitLen() > 8192 {
			continue
		}
		if keys[jwk.KeyID] != nil {
			return errJWKSUnavailable
		}
		keys[jwk.KeyID] = key
	}
	if len(keys) == 0 {
		return errJWKSUnavailable
	}
	a.keys, a.expires = keys, a.now().Add(a.ttl)
	return nil
}
