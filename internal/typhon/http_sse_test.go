package typhon

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const sseFixtureSource = `import typhon.auth as auth
import typhon.http as http
import typhon.web as web

r: web.Router = web.router()
r.use_errors()
verifier: auth.Authenticator = auth.jwt('https://issuer.example', 'events', 'https://issuer.example/jwks')

def produce(after: str, state: Any) -> http.Event | None:
    if after == '':
        return http.event('1', 'change', 'first\nline')
    if after == '1':
        return http.event('2', 'change', 'second')
    if after == '2':
        return http.event('2', 'change', 'duplicate')
    return None

def idle(after: str, state: Any) -> http.Event | None:
    return None

def notify(req: http.Request) -> http.Response:
    return http.sse(produce, None)

def notify_idle(req: http.Request) -> http.Response:
    return http.sse(idle, None)

def health(req: http.Request) -> http.Response:
    return http.response(204, '')

r.add('GET', '/event', notify)
r.enable_sse('/event', 20, 1)
r.add('GET', '/idle', notify_idle)
r.enable_sse('/idle', 20, 1)
r.add('GET', '/private', notify_idle)
r.enable_sse('/private', 20, 1)
r.require_auth('GET', '/private', verifier)
r.add('GET', '/health', health)
`

func eventPoolFixture(t *testing.T, ctx context.Context, timeout time.Duration) (*httpWorkerPool, *webRouter) {
	t.Helper()
	engine := NewEngine()
	t.Cleanup(func() {
		if err := engine.Close(); err != nil {
			t.Error(err)
		}
	})
	module, err := engine.LoadModule(sourceFile(t, sseFixtureSource))
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ExecuteModule(module, false); err != nil {
		t.Fatal(err)
	}
	pool := newHTTPWorkerPool(ctx, 1, 0)
	pool.requestTimeout = timeout
	engine.httpMetrics.Store(&pool.metrics)
	if err := pool.load(&VM{engine: engine}, module.Globals["r"]); err != nil {
		t.Fatal(err)
	}
	router, _ := routerValue(module.Globals["r"])
	return pool, router
}

func TestNativeSSEReplayHeartbeatAndAuthentication(t *testing.T) {
	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	pool, router := eventPoolFixture(t, ctx, 200*time.Millisecond)
	router.routes[[2]string{"GET", "/private"}].auth = rateFixtureAuth{
		"valid": {subject: "alice", tenant: "workspace", expires: time.Now().Add(3 * time.Second)},
	}
	server := httptest.NewServer(pool)
	defer func() {
		stop()
		server.Close()
	}()
	start := func(path, last, token string) *http.Response {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, "GET", server.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if last != "" {
			req.Header.Set("Last-Event-ID", last)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = resp.Body.Close() })
		return resp
	}
	block := func(reader *bufio.Reader) string {
		t.Helper()
		var lines strings.Builder
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			if line == "\n" {
				return lines.String()
			}
			lines.WriteString(line)
		}
	}
	initial := start("/event", "", "")
	if initial.StatusCode != 200 || initial.Header.Get("Content-Type") != "text/event-stream; charset=utf-8" {
		t.Fatal("SSE negotiation failed")
	}
	reader := bufio.NewReader(initial.Body)
	if block(reader) != ": connected\n" || block(reader) != "id: 1\nevent: change\ndata: first\ndata: line\n" {
		t.Fatal("first SSE frame did not preserve multiline data")
	}
	_ = initial.Body.Close()

	reconnected := start("/event", "1", "")
	reader = bufio.NewReader(reconnected.Body)
	if block(reader) != ": connected\n" || block(reader) != "id: 2\nevent: change\ndata: second\n" {
		t.Fatal("Last-Event-ID did not resume at the next event")
	}
	if block(reader) != ": keep-alive\n" {
		t.Fatal("the stream duplicated its last event or timed out at the normal HTTP deadline")
	}
	_ = reconnected.Body.Close()

	denied := start("/private", "", "")
	if denied.StatusCode != 401 || denied.Header.Get("WWW-Authenticate") != "Bearer" {
		t.Fatal("private SSE route did not authenticate the handshake")
	}
	private := start("/private", "", "valid")
	if private.StatusCode != 200 {
		t.Fatalf("valid SSE handshake returned %d", private.StatusCode)
	}
	body, err := io.ReadAll(private.Body)
	if err != nil || !strings.Contains(string(body), ": connected") {
		t.Fatalf("SSE did not close cleanly at token expiry: %q, %v", body, err)
	}
	rec := httptest.NewRecorder()
	bad := httptest.NewRequest("GET", "/event", nil)
	bad.Header.Add("Last-Event-ID", "1")
	bad.Header.Add("Last-Event-ID", "2")
	pool.ServeHTTP(rec, bad)
	readTestProblem(t, rec, 400, "bad_request")
}

func TestNativeSSEConcurrentShutdownKeepsHTTPAvailable(t *testing.T) {
	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	pool, _ := eventPoolFixture(t, ctx, 750*time.Millisecond)
	const clients = 100
	pool.eventSlots = make(chan struct{}, clients)
	// Make this the same entry point used by the production listener.
	server := httptest.NewUnstartedServer(pool)
	server.Config.WriteTimeout = 200 * time.Millisecond
	server.Start()
	defer func() {
		stop()
		server.Close()
	}()
	client := server.Client()
	client.Timeout = 10 * time.Second
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	var both sync.WaitGroup
	handles := make(chan io.ReadCloser, clients)
	for range clients {
		both.Go(func() {
			resp, err := client.Get(server.URL + "/idle")
			if err != nil {
				t.Error(err)
				return
			}
			if resp.StatusCode != 200 {
				t.Errorf("SSE subscription returned %d", resp.StatusCode)
				resp.Body.Close()
				return
			}
			handles <- resp.Body
		})
	}
	both.Wait()
	close(handles)
	if len(handles) != clients || pool.metrics.eventConnections.Load() != clients {
		t.Fatalf("expected %d active event streams, got %d", clients, pool.metrics.eventConnections.Load())
	}
	runtime.GC()
	var live runtime.MemStats
	runtime.ReadMemStats(&live)
	t.Logf("live client + server heap delta for %d sockets: %d bytes", clients, int64(live.HeapAlloc)-int64(before.HeapAlloc))
	for _, tc := range []struct {
		path string
		code int
	}{{"/idle", 503}, {"/health", 204}} {
		resp, err := client.Get(server.URL + tc.path)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != tc.code {
			t.Fatalf("%s: HTTP %d", tc.path, resp.StatusCode)
		}
	}
	began := time.Now()
	stop()
	for handle := range handles {
		both.Go(func() {
			body, err := io.ReadAll(handle)
			_ = handle.Close()
			if err != nil || !strings.Contains(string(body), ": connected") {
				t.Errorf("server shutdown did not drain stream: %q %v", body, err)
			}
		})
	}
	both.Wait()
	for pool.metrics.eventConnections.Load() != 0 && time.Since(began) < 2*time.Second {
		time.Sleep(time.Millisecond)
	}
	if pool.metrics.eventConnections.Load() != 0 || time.Since(began) >= 2*time.Second {
		t.Fatal("SSE connections were not drained promptly")
	}
}

type slowSSEWriter struct {
	*httptest.ResponseRecorder
	deadline atomic.Int64
	blocked  chan struct{}
}

func (w *slowSSEWriter) SetWriteDeadline(deadline time.Time) error {
	w.deadline.Store(deadline.UnixNano())
	return nil
}

func (w *slowSSEWriter) Write(data []byte) (int, error) {
	if strings.HasPrefix(string(data), ": connected") {
		return w.ResponseRecorder.Write(data)
	}
	close(w.blocked)
	<-time.After(time.Until(time.Unix(0, w.deadline.Load())))
	return 0, os.ErrDeadlineExceeded
}

func (w *slowSSEWriter) WriteString(data string) (int, error) { return w.Write([]byte(data)) }

func TestNativeSSEBackpressureReleasesConnection(t *testing.T) {
	pool, _ := eventPoolFixture(t, t.Context(), time.Second)
	writer := &slowSSEWriter{ResponseRecorder: httptest.NewRecorder(), blocked: make(chan struct{})}
	result := make(chan any, 1)
	go func() {
		defer func() { result <- recover() }()
		pool.ServeHTTP(writer, httptest.NewRequest(http.MethodGet, "/event", nil))
	}()
	select {
	case <-writer.blocked:
	case <-time.After(time.Second):
		t.Fatal("SSE did not reach the blocked write")
	}
	ready := httptest.NewRecorder()
	pool.ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/health", nil))
	if ready.Code != 204 {
		t.Fatalf("slow SSE client blocked a normal request: %d", ready.Code)
	}
	select {
	case stopped := <-result:
		err, ok := stopped.(error)
		if !ok || !errors.Is(err, http.ErrAbortHandler) || pool.metrics.eventConnections.Load() != 0 {
			t.Fatalf("slow connection was not aborted and released: %v", stopped)
		}
	case <-time.After(sseWriteTimeout + time.Second):
		t.Fatal("slow SSE connection outlasted its per-write deadline")
	}
}

func TestNativeSSEEventFieldValidation(t *testing.T) {
	for _, statement := range []string{
		`http.event('id\ndata: forged','message','text')`,
		`http.event('id','event\r\n','text')`,
		fmt.Sprintf(`http.event('id','message',%q)`, strings.Repeat("x", maxSSEDataBytes+1)),
	} {
		path := sourceFile(t, "import typhon.http as http\n"+statement+"\n")
		if err := RunFile(path); err == nil {
			t.Fatal("invalid SSE event was accepted")
		}
	}
}
