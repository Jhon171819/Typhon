package typhon

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"math/rand/v2"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

type httpClientConfig struct {
	configured, allowHTTP bool
	connect, read, total  time.Duration
	responseLimit         int64
	connections           int
	redirects, retries    int
	rootPEM               string
}

func nativeHTTPClient(vm *VM, args []Value) (Value, error) {
	timeout, err := driverTimeout(args, 0)
	if err != nil {
		return NoneValue(), err
	}
	return makeHTTPClient(vm, httpClientConfig{allowHTTP: true, total: timeout, responseLimit: maxHTTPBody, connections: 16})
}

func nativeHTTPClientWithOptions(vm *VM, args []Value) (Value, error) {
	config := httpClientConfig{
		configured: true, connect: 5 * time.Second, read: 5 * time.Second, total: 10 * time.Second,
		responseLimit: maxHTTPBody, connections: 16,
	}
	for _, entry := range args[0].Dict.Entries {
		name, value := entry.Key.Str, entry.Value
		switch name {
		case "connect_timeout", "read_timeout", "total_timeout":
			if value.Kind != ValueInt && value.Kind != ValueFloat {
				return NoneValue(), errors.New("HTTP timeouts require seconds as a number")
			}
			seconds := numberValue(value)
			if !(seconds >= 0.001 && seconds <= 300) {
				return NoneValue(), errors.New("HTTP timeouts must be between 0.001 and 300 seconds")
			}
			duration := time.Duration(seconds * float64(time.Second))
			switch name {
			case "connect_timeout":
				config.connect = duration
			case "read_timeout":
				config.read = duration
			case "total_timeout":
				config.total = duration
			}
		case "max_response_bytes":
			if value.Kind != ValueInt || value.Int < 1 || value.Int > maxHTTPBody {
				return NoneValue(), errors.New("HTTP response limit must be between 1 byte and 8 MiB")
			}
			config.responseLimit = value.Int
		case "max_connections":
			if value.Kind != ValueInt || value.Int < 1 || value.Int > 64 {
				return NoneValue(), errors.New("HTTP connection pool size must be between 1 and 64")
			}
			config.connections = int(value.Int)
		case "max_redirects", "retries":
			if value.Kind != ValueInt || value.Int < 0 || value.Int > 4 {
				return NoneValue(), errors.New("HTTP retries and redirects must be between 0 and 4")
			}
			if name == "retries" {
				config.retries = int(value.Int)
			} else {
				config.redirects = int(value.Int)
			}
		case "allow_http":
			if value.Kind != ValueBool {
				return NoneValue(), errors.New("HTTP allow_http requires a boolean")
			}
			config.allowHTTP = value.Bool
		case "tls_root_pem":
			if value.Kind != ValueString || len(value.Str) == 0 || len(value.Str) > 64<<10 {
				return NoneValue(), errors.New("HTTP TLS requires up to 64 KiB of PEM certificates")
			}
			config.rootPEM = value.Str
		default:
			return NoneValue(), errors.New("unknown HTTP client option")
		}
	}
	return makeHTTPClient(vm, config)
}

type httpOutboundClient struct {
	client *http.Client
	config httpClientConfig
	closed atomic.Bool
}

func makeHTTPClient(vm *VM, config httpClientConfig) (Value, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxConnsPerHost = config.connections
	client := &http.Client{Transport: transport, Timeout: config.total}
	if config.configured {
		transport.DialContext = (&net.Dialer{Timeout: config.connect, KeepAlive: 30 * time.Second}).DialContext
		transport.TLSHandshakeTimeout = config.connect
		transport.ResponseHeaderTimeout = config.read
		transport.MaxIdleConns, transport.MaxIdleConnsPerHost = config.connections, config.connections
		transport.MaxResponseHeaderBytes = 64 << 10
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		if config.rootPEM != "" {
			pool, err := x509.SystemCertPool()
			if err != nil || pool == nil || !pool.AppendCertsFromPEM([]byte(config.rootPEM)) {
				return NoneValue(), errors.New("could not load HTTP TLS root certificates")
			}
			transport.TLSClientConfig.RootCAs = pool
		}
		client.CheckRedirect = config.redirect
	}

	outbound := &httpOutboundClient{client: client, config: config}
	scope := vm.resourceScope()
	closeClient := func(context.Context) error {
		if !outbound.closed.Swap(true) {
			scope.remove(outbound)
			client.CloseIdleConnections()
		}
		return nil
	}
	if err := scope.add(vm.context(), outbound, "HTTP client close", closeClient); err != nil {
		return NoneValue(), err
	}
	typ := httpDriverTypes()["Client"]
	return scopedNativeObject(vm, typ, map[string]Value{
		"request": driverFunc(typ.Members["request"], func(vm *VM, args []Value) (Value, error) {
			return outbound.request(vm, args[0].Str, args[1].Str, args[2].Str, args[3:], "")
		}),
		"get": driverFunc(typ.Members["get"], func(vm *VM, args []Value) (Value, error) {
			return outbound.request(vm, http.MethodGet, args[0].Str, "", nil, "")
		}),
		"request_json": driverFunc(typ.Members["request_json"], func(vm *VM, args []Value) (Value, error) {
			if outbound.closed.Load() {
				return NoneValue(), errors.New("HTTP client is closed")
			}
			body, err := dumpJSON(vm, args[2])
			if err != nil {
				return NoneValue(), err
			}
			return outbound.request(vm, args[0].Str, args[1].Str, body, args[3:], "application/json")
		}),
		"request_bytes": driverFunc(typ.Members["request_bytes"], func(vm *VM, args []Value) (Value, error) {
			data, ok := scalarData(args[2])
			if !ok {
				return NoneValue(), errors.New("expected native Bytes")
			}
			body, ok := data.sql.([]byte)
			if !ok {
				return NoneValue(), errors.New("expected native Bytes")
			}
			return outbound.request(vm, args[0].Str, args[1].Str, string(body), args[3:], "application/octet-stream")
		}),
		"close": driverCloser(typ.Members["close"], closeClient),
	}), nil
}

func httpJSONMediaType(contentType string) bool {
	media, _, err := mime.ParseMediaType(contentType)
	return err == nil && (media == "application/json" ||
		(strings.HasPrefix(media, "application/") && strings.HasSuffix(media, "+json")))
}

func (c httpClientConfig) redirect(next *http.Request, previous []*http.Request) error {
	initial := previous[0]
	if c.redirects == 0 || (initial.Method != http.MethodGet && initial.Method != http.MethodHead) {
		return http.ErrUseLastResponse
	}
	if len(previous) > c.redirects {
		return errors.New("HTTP redirect limit exceeded")
	}
	if next.URL.User != nil || next.URL.Scheme != initial.URL.Scheme || !strings.EqualFold(next.URL.Host, initial.URL.Host) {
		return http.ErrUseLastResponse
	}
	return nil
}

var errHTTPResponseLimit = errors.New("HTTP response body exceeds the byte limit")

type httpClientResult struct {
	status  int
	body    string
	headers http.Header
}

func (c *httpOutboundClient) request(vm *VM, method, target, body string, optionalHeaders []Value, contentType string) (Value, error) {
	if c.closed.Load() {
		return NoneValue(), errors.New("HTTP client is closed")
	}
	if len(body) > maxHTTPBody {
		return NoneValue(), errors.New("HTTP body limit exceeded")
	}
	if err := vm.chargeBytes(int64(len(body))); err != nil {
		return NoneValue(), err
	}
	ctx, cancel := driverContext(vm, c.config.total)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, method, target, strings.NewReader(body)) // #nosec G704 -- The trusted Typhon application chooses its upstream URL.
	if err != nil {
		return NoneValue(), driverFailure("HTTP", validationKind, err)
	}
	if request.URL.Scheme != "https" && !(c.config.allowHTTP && request.URL.Scheme == "http") {
		return NoneValue(), errors.New("HTTP client requires an allowed HTTP or HTTPS scheme")
	}
	if c.config.configured && (request.URL.User != nil || request.URL.Hostname() == "") {
		return NoneValue(), errors.New("HTTP client requires a host and disallows URL credentials")
	}
	if len(optionalHeaders) == 1 {
		request.Header, err = httpHeaders(optionalHeaders[0])
		if err != nil {
			return NoneValue(), err
		}
	}
	if contentType != "" && len(request.Header.Values("Content-Type")) == 0 {
		request.Header.Set("Content-Type", contentType)
	}
	idempotent := retryableHTTPMethod(request)
	for attempt := 0; ; attempt++ {
		result, err := c.attempt(vm, request, body)
		if err == nil && (!idempotent || attempt >= c.config.retries || !retryableHTTPStatus(result.status)) {
			return httpResponse(vm, int64(result.status), result.body, result.headers)
		}
		if err != nil && (!idempotent || attempt >= c.config.retries || !retryableHTTPTransport(err)) {
			return NoneValue(), classifyHTTPClientError(err)
		}
		delay := httpRetryDelay(attempt, result.headers)
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= delay {
			if err != nil {
				return NoneValue(), classifyHTTPClientError(err)
			}
			return httpResponse(vm, int64(result.status), result.body, result.headers)
		}
		if err := vm.chargeBytes(int64(len(result.body))); err != nil {
			return NoneValue(), err
		}
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return NoneValue(), classifyHTTPClientError(ctx.Err())
		}
	}
}

func (c *httpOutboundClient) attempt(vm *VM, original *http.Request, body string) (httpClientResult, error) {
	var result httpClientResult
	ctx, stop := context.WithCancel(original.Context())
	defer stop()
	request := original.Clone(ctx)
	request.Body, _ = original.GetBody()
	if c.config.configured {
		// The application controls replay of requests with bodies.
		request.GetBody = nil
		if len(body) == 0 && request.Method != http.MethodGet && request.Method != http.MethodHead &&
			request.Header.Get("Idempotency-Key") != "" {
			request.Body = io.NopCloser(strings.NewReader(""))
		}
	}
	injectHTTPTrace(request)
	response, err := c.client.Do(request) // #nosec G704 -- The trusted Typhon application chooses its upstream URL.
	if err != nil {
		return result, err
	}
	var expired atomic.Bool
	if c.config.read != 0 {
		timer := time.AfterFunc(c.config.read, func() {
			expired.Store(true)
			stop()
		})
		defer timer.Stop()
	}
	budget := vm.executionBudget()
	limit := min(c.config.responseLimit, max(0, budget.byteLimit-budget.bytes))
	contents, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	closeErr := response.Body.Close()
	if err != nil {
		reportCleanupError("HTTP response body close", driverFailure("HTTP", connectionKind, closeErr))
		if expired.Load() {
			return result, context.DeadlineExceeded
		}
		return result, err
	}
	if closeErr != nil {
		return result, closeErr
	}
	if int64(len(contents)) > limit {
		if limit < c.config.responseLimit {
			return result, quotaStop("byte", budget.byteLimit)
		}
		return result, errHTTPResponseLimit
	}
	return httpClientResult{status: response.StatusCode, body: string(contents), headers: response.Header}, nil
}

func classifyHTTPClientError(err error) error {
	if errors.Is(err, errHTTPResponseLimit) {
		return &Exception{kind: validationKind, message: errHTTPResponseLimit.Error()}
	}
	return driverFailure("HTTP", connectionKind, err)
}

func retryableHTTPMethod(request *http.Request) bool {
	switch request.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodPut, http.MethodDelete, http.MethodTrace:
		return true
	case http.MethodPost, http.MethodPatch:
		keys := request.Header.Values("Idempotency-Key")
		return len(keys) == 1 && validIdempotencyKey(keys[0])
	default:
		return false
	}
}

func retryableHTTPStatus(status int) bool {
	switch status {
	case 408, 429, 502, 503, 504:
		return true
	default:
		return false
	}
}

func retryableHTTPTransport(err error) bool {
	var certificate *tls.CertificateVerificationError
	if errors.As(err, &certificate) {
		return false
	}
	var network *net.OpError
	var timeout net.Error
	return errors.As(err, &network) || errors.As(err, &timeout) && timeout.Timeout() ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

func httpRetryDelay(attempt int, headers http.Header) time.Duration {
	base := (20 * time.Millisecond) << attempt
	jitter := base + rand.N(base)
	if value := headers.Get("Retry-After"); value != "" {
		if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
			if seconds > 300 {
				return 301 * time.Second
			}
			return max(jitter, time.Duration(seconds)*time.Second)
		}
		if when, err := http.ParseTime(value); err == nil {
			return max(jitter, time.Until(when))
		}
	}
	return jitter
}
