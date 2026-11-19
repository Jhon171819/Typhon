package typhon

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

type webCORS struct {
	origins, methods, headers map[string]bool
	credentials               bool
}

type webSecurityHeaders struct {
	csp, frame, hsts string
}

type webBrowserPolicy struct {
	cors     *webCORS
	security *webSecurityHeaders
}

func validCORSOrigin(value string) bool {
	if len(value) > 512 || strings.ContainsAny(value, "\\?#,%") {
		return false
	}
	u, err := url.Parse(value)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != "" &&
		u.User == nil && u.Path == "" && u.RawQuery == "" && u.String() == value
}

func newWebCORS(args []Value) (*webCORS, error) {
	p := &webCORS{origins: map[string]bool{}, methods: map[string]bool{}, headers: map[string]bool{}}
	if len(args) == 4 {
		p.credentials = args[3].Bool
	}
	origins, methods, headers := *args[0].List, *args[1].List, *args[2].List
	if len(origins) == 0 || len(origins) > 32 || len(methods) == 0 || len(methods) > 16 || len(headers) > 32 {
		return nil, errors.New("invalid CORS allowlist size")
	}
	for _, origin := range origins {
		if origin.Str == "*" {
			if p.credentials || len(origins) != 1 {
				return nil, errors.New("wildcard CORS origin requires credential-free use and no other origins")
			}
		} else if !validCORSOrigin(origin.Str) {
			return nil, errors.New("invalid CORS origin")
		}
		p.origins[origin.Str] = true
	}
	for _, method := range methods {
		if len(method.Str) == 0 || len(method.Str) > 16 ||
			strings.IndexFunc(method.Str, func(c rune) bool { return c < 'A' || c > 'Z' }) >= 0 {
			return nil, errors.New("invalid CORS method")
		}
		p.methods[method.Str] = true
	}
	for _, header := range headers {
		if header.Str == "*" || len(header.Str) > 128 || !validHTTPHeaderName(header.Str) {
			return nil, errors.New("invalid CORS header")
		}
		p.headers[strings.ToLower(header.Str)] = true
	}
	return p, nil
}

func newWebSecurityHeaders(args []Value) (*webSecurityHeaders, error) {
	p := &webSecurityHeaders{frame: "DENY"}
	if len(args) > 0 {
		p.csp = args[0].Str
	}
	if len(args) > 1 {
		p.frame = args[1].Str
	}
	if len(p.csp) > 2048 || !validHTTPHeaderValue(p.csp) ||
		p.frame != "" && p.frame != "DENY" && p.frame != "SAMEORIGIN" {
		return nil, errors.New("invalid browser security header configuration")
	}
	if len(args) > 2 {
		if secs := args[2].Int; secs < 0 || secs > 63072000 {
			return nil, errors.New("invalid HSTS duration")
		} else if secs > 0 {
			p.hsts = "max-age=" + strconv.FormatInt(secs, 10)
		}
	}
	return p, nil
}

func webSessionCookie(args []Value) (Value, error) {
	cookie := http.Cookie{
		Name: args[0].Str, Value: args[1].Str, Path: "/",
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	}
	if len(args) > 2 {
		switch args[2].Str {
		case "Lax":
		case "Strict":
			cookie.SameSite = http.SameSiteStrictMode
		case "None":
			cookie.SameSite = http.SameSiteNoneMode
		default:
			return NoneValue(), errors.New("invalid cookie SameSite mode")
		}
	}
	if len(args) > 3 {
		age := args[3].Int
		if age < -1 || age > 31536000 {
			return NoneValue(), errors.New("invalid session cookie lifetime")
		}
		cookie.MaxAge = int(age)
	}
	if len(cookie.Name) > 128 || len(cookie.Value) > 2048 || cookie.Valid() != nil {
		return NoneValue(), errors.New("invalid secure session cookie")
	}
	return StringValue(cookie.String()), nil
}

func (p webBrowserPolicy) wrap(next http.Handler, problemJSON bool) http.Handler {
	if p == (webBrowserPolicy{}) {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		browser := &webBrowserWriter{ResponseWriter: w, security: p.security, tls: req.TLS != nil, cors: p.cors != nil}
		if p.cors != nil {
			browser.credentials = p.cors.credentials
			origins := req.Header.Values("Origin")
			origin := ""
			if len(origins) == 1 && validCORSOrigin(origins[0]) {
				if p.cors.origins[origins[0]] {
					origin = origins[0]
				} else if p.cors.origins["*"] {
					origin = "*"
				}
			}
			methods := req.Header.Values("Access-Control-Request-Method")
			if req.Method == http.MethodOptions && len(methods) > 0 {
				browser.preflight = true
				method := ""
				if len(methods) == 1 {
					method = methods[0]
				}
				headers, valid := p.cors.requestHeaders(req.Header.Values("Access-Control-Request-Headers"))
				if origin == "" || !p.cors.methods[method] || !valid {
					problemWriter{enabled: problemJSON}.write(browser, 403, "CORS preflight denied")
					return
				}
				browser.origin = origin
				browser.allowMethod, browser.allowHeaders = method, headers
				browser.WriteHeader(http.StatusNoContent)
				return
			}
			if p.cors.methods[req.Method] {
				browser.origin = origin
			}
		}
		next.ServeHTTP(browser, req)
	})
}

func (p *webCORS) requestHeaders(lines []string) (string, bool) {
	if len(lines) == 0 {
		return "", true
	}
	if len(lines) != 1 || len(lines[0]) > 4096 {
		return "", false
	}
	names := strings.Split(lines[0], ",")
	if len(names) > 32 {
		return "", false
	}
	for i, name := range names {
		name = strings.ToLower(strings.Trim(name, " \t"))
		if !p.headers[name] {
			return "", false
		}
		names[i] = name
	}
	return strings.Join(names, ", "), true
}

type webBrowserWriter struct {
	http.ResponseWriter
	security                          *webSecurityHeaders
	tls, cors, preflight, credentials bool
	origin, allowMethod, allowHeaders string
	wrote                             bool
}

func (w *webBrowserWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *webBrowserWriter) WriteHeader(status int) {
	if status >= 200 {
		w.apply()
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *webBrowserWriter) Write(body []byte) (int, error) {
	w.apply()
	return w.ResponseWriter.Write(body)
}

func (w *webBrowserWriter) apply() {
	if w.wrote {
		return
	}
	w.wrote = true
	h := w.Header()
	if w.cors {
		h.Add("Vary", "Origin")
		h.Del("Access-Control-Allow-Origin")
		h.Del("Access-Control-Allow-Credentials")
		h.Del("Access-Control-Allow-Methods")
		h.Del("Access-Control-Allow-Headers")
		h.Del("Access-Control-Max-Age")
		if w.preflight {
			h.Add("Vary", "Access-Control-Request-Method")
			h.Add("Vary", "Access-Control-Request-Headers")
		}
		if w.origin != "" {
			h.Set("Access-Control-Allow-Origin", w.origin)
			if w.credentials {
				h.Set("Access-Control-Allow-Credentials", "true")
			}
			if w.allowMethod != "" {
				h.Set("Access-Control-Allow-Methods", w.allowMethod)
			}
			if w.allowHeaders != "" {
				h.Set("Access-Control-Allow-Headers", w.allowHeaders)
			}
		}
	}
	if p := w.security; p != nil {
		h.Set("X-Content-Type-Options", "nosniff")
		if p.frame != "" {
			h.Set("X-Frame-Options", p.frame)
		}
		if p.csp != "" {
			h.Set("Content-Security-Policy", p.csp)
		}
		h.Del("Strict-Transport-Security")
		if w.tls && p.hsts != "" {
			h.Set("Strict-Transport-Security", p.hsts)
		}
	}
}
