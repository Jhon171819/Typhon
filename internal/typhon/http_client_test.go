package typhon

import (
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNativeHTTPClientJSONBytesAndReuse(t *testing.T) {
	var connections, active atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Error(err)
		}
		switch req.URL.Path {
		case "/json":
			if req.Method != http.MethodPost || req.Header.Get("Content-Type") != "application/json" ||
				string(body) != `{"items": [1, null, true]}` ||
				strings.Join(req.Header.Values("X-Example"), ",") != "first,second" {
				t.Error("JSON request or repeated headers did not survive")
			}
			w.Header().Set("Content-Type", "application/problem+json; charset=utf-8")
			w.Header().Add("X-Result", "one")
			w.Header().Add("X-Result", "two")
			_, _ = w.Write(body)
		case "/bytes":
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(body)
		case "/bad-json":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"duplicate":1,"duplicate":2}`)
		default:
			w.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(w, "null")
		}
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
			active.Add(1)
		}
		if state == http.StateClosed {
			active.Add(-1)
		}
	}
	server.Start()
	t.Cleanup(server.Close)
	result, err := captureRun(t, sourceFile(t, fmt.Sprintf(`
import typhon.http as http
import typhon.scalars as s
client: http.Client = http.client_with_options({'allow_http': True, 'max_connections': 1})
base: str = %q
r: http.Response = client.request_json('POST', base + '/json', {'items': [1, None, True]}, [['X-Example','first'], ['X-Example','second']])
print(r.status, r.header_values('x-result'), r.json())
raw: http.Response = client.request_bytes('POST', base + '/bytes', s.bytes_hex('00ff800a'))
print(raw.bytes().hex())
try:
    client.get(base + '/text').json()
except ValidationError:
    print('media type rejected')
try:
    client.get(base + '/bad-json').json()
except ValidationError:
    print('invalid JSON rejected')
client.close()
client.close()
try:
    client.get(base + '/text')
except ValidationError:
    print('already closed')
`, server.URL)))
	if err != nil || !strings.Contains(result, "200 [one, two]") ||
		!strings.Contains(result, "00ff800a\nmedia type rejected\ninvalid JSON rejected\nalready closed\n") {
		t.Fatalf("response or codec contract failed: %q %v", result, err)
	}
	deadline := time.Now().Add(time.Second)
	for active.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if connections.Load() != 1 || active.Load() != 0 {
		t.Fatalf("expected one reused connection and complete close, opened=%d active=%d", connections.Load(), active.Load())
	}
}

func TestNativeHTTPClientTLSRedirectAndResponseLimit(t *testing.T) {
	var foreignCalls atomic.Int32
	foreign := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		foreignCalls.Add(1)
	}))
	t.Cleanup(foreign.Close)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/loop":
			http.Redirect(w, req, "/loop", http.StatusFound)
		case "/same":
			http.Redirect(w, req, "/ok", http.StatusFound)
		case "/foreign":
			http.Redirect(w, req, foreign.URL+"/never", http.StatusFound)
		case "/large":
			_, _ = io.WriteString(w, strings.Repeat("x", 2048))
		default:
			_, _ = io.WriteString(w, "ok")
		}
	}))
	t.Cleanup(server.Close)
	t.Setenv("TYPHON_TEST_HTTP_CA", string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})))
	result, err := captureRun(t, sourceFile(t, fmt.Sprintf(`
import typhon.http as http
base: str = %q
plain: str = %q
try:
    http.client_with_options({}).get(base + '/ok')
except ConnectionError:
    print('certificate rejected')
try:
    http.client_with_options({}).get(plain + '/never')
except ValidationError:
    print('plain transport rejected')
root: str = http.env('TYPHON_TEST_HTTP_CA')
safe: http.Client = http.client_with_options({'tls_root_pem': root, 'max_response_bytes': 512})
print(safe.get(base + '/same').status)
try:
    safe.get(base + '/large')
except ValidationError:
    print('large response rejected')
redirect: http.Client = http.client_with_options({'tls_root_pem': root, 'max_redirects': 2, 'allow_http': True})
print(redirect.get(base + '/same').status, redirect.get(base + '/foreign').status)
try:
    redirect.get(base + '/loop')
except ConnectionError:
    print('redirect loop stopped')
`, server.URL, foreign.URL)))
	want := "certificate rejected\nplain transport rejected\n302\nlarge response rejected\n200 302\nredirect loop stopped\n"
	if err != nil || result != want || foreignCalls.Load() != 0 {
		t.Fatalf("TLS/redirect contracts failed: %q %v foreign=%d", result, err, foreignCalls.Load())
	}
}

func TestNativeHTTPClientRetryAndTimeout(t *testing.T) {
	var gets, keyed, unkeyed, patience atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/get":
			if gets.Add(1) == 1 {
				w.WriteHeader(503)
				return
			}
		case "/keyed":
			body, _ := io.ReadAll(req.Body)
			if string(body) != `{"job": 1}` || req.Header.Get("Idempotency-Key") != "retry-1" {
				t.Error("keyed retry mutated the request")
			}
			if keyed.Add(1) == 1 {
				w.WriteHeader(503)
				return
			}
			w.WriteHeader(201)
		case "/unkeyed":
			unkeyed.Add(1)
			w.WriteHeader(200)
			_ = http.NewResponseController(w).Flush()
			<-req.Context().Done()
		case "/patience":
			patience.Add(1)
			w.Header().Set("Retry-After", "10")
			w.WriteHeader(503)
		default:
			<-req.Context().Done()
		}
	}))
	t.Cleanup(server.Close)
	result, err := captureRun(t, sourceFile(t, fmt.Sprintf(`
import typhon.http as http
base: str = %q
opts: dict[str, Any] = {'allow_http': True, 'read_timeout': 0.05, 'total_timeout': 0.5, 'retries': 2}
client: http.Client = http.client_with_options(opts)
print(client.get(base + '/get').status)
print(client.request_json('POST', base + '/keyed', {'job': 1}, [['Idempotency-Key', 'retry-1']]).status)
try:
    client.request('POST', base + '/unkeyed', 'uncertain')
except TimeoutError:
    print('response body timed out')
print(client.get(base + '/patience').status)
`, server.URL)))
	if err != nil || result != "200\n201\nresponse body timed out\n503\n" ||
		gets.Load() != 2 || keyed.Load() != 2 || unkeyed.Load() != 1 || patience.Load() != 1 {
		t.Fatalf("retry contract failed: %q %v get=%d key=%d plain=%d patience=%d",
			result, err, gets.Load(), keyed.Load(), unkeyed.Load(), patience.Load())
	}
	engine := NewEngine()
	t.Cleanup(func() {
		if err := engine.Close(); err != nil {
			t.Error(err)
		}
	})
	vm := &VM{engine: engine}
	opts := DictValue()
	for _, entry := range []DictEntry{
		{Key: StringValue("allow_http"), Value: BoolValue(true)},
		{Key: StringValue("read_timeout"), Value: FloatValue(0.5)},
		{Key: StringValue("retries"), Value: IntValue(2)},
	} {
		_ = opts.Dict.put(entry.Key, entry.Value)
	}
	client, err := nativeHTTPClientWithOptions(vm, []Value{opts})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	timer := time.AfterFunc(50*time.Millisecond, cancel)
	defer timer.Stop()
	vm.ctx = ctx
	start := time.Now()
	_, err = vm.callValue(client.NativeObject.Attrs["get"], []Value{StringValue(server.URL + "/slow")}, nil)
	if !errors.Is(err, context.Canceled) || time.Since(start) > 400*time.Millisecond {
		t.Fatalf("downstream survived request cancellation: %v elapsed=%s", err, time.Since(start))
	}
}
