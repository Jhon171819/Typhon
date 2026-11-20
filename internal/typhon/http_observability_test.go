package typhon

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	traceexport "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

type observationLog struct {
	mu sync.Mutex
	bytes.Buffer
}

func (b *observationLog) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.Write(p)
}

func (b *observationLog) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.String()
}

func checkHTTPObservability(t *testing.T, pgURL string) {
	t.Helper()
	redis := miniredis.RunT(t)
	recorded := make(chan *tracepb.Span, 32)
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, 1<<20))
		var payload traceexport.ExportTraceServiceRequest
		if err != nil || proto.Unmarshal(body, &payload) != nil || req.URL.Path != "/v1/traces" {
			http.Error(w, "invalid OTLP request", 400)
			return
		}
		for _, resource := range payload.ResourceSpans {
			for _, scope := range resource.ScopeSpans {
				for _, span := range scope.Spans {
					recorded <- span
				}
			}
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
	}))
	t.Cleanup(collector.Close)

	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	arrived := make(chan string, 1)
	release := make(chan struct{})
	releaseWork := sync.OnceFunc(func() { close(release) })
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		arrived <- req.Header.Get("Traceparent")
		select {
		case <-release:
		case <-req.Context().Done():
		}
	}))
	t.Cleanup(upstream.Close)
	t.Cleanup(releaseWork)

	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", collector.URL+"/v1/traces")
	t.Setenv("TYPHON_TRACE_SAMPLE_RATIO", "1")
	t.Setenv("TYPHON_METRICS_ADDR", "127.0.0.1:0")
	t.Setenv("TYPHON_HTTP_JSON_LOGS", "1")
	t.Setenv("TYPHON_TRUST_REQUEST_ID", "1")
	t.Setenv("TYPHON_TENANT_LOG_KEY", "a-fixed-key-for-the-test-suite-only")
	var pgSetup, pgQuery string
	if pgURL != "" {
		t.Setenv("DATABASE_URL", pgURL)
		pgSetup = "import typhon.postgres as pg\ndb: pg.Connection = pg.connect(pg.env('DATABASE_URL'))\n"
		pgQuery = "    db.query('SELECT $1::text AS value', ['postgres-parameter-private'])\n"
	}
	engine := NewEngine()
	t.Cleanup(func() {
		if err := engine.Close(); err != nil {
			t.Error(err)
		}
	})
	module, err := engine.LoadModule(sourceFile(t, fmt.Sprintf(`
import typhon.auth as auth
import typhon.http as http
import typhon.redis as redis
import typhon.web as web
%scache: redis.Connection = redis.connect(%q)
client: http.Client = http.client()
token: auth.Authenticator = auth.jwt('https://issuer.example', 'test', 'https://issuer.example/jwks')
policy: auth.Policy = auth.policy({'reader': ['read']})
routes: web.Router = web.router()
routes.use_errors()
routes.use_request_id()

def read(req: http.Request) -> http.Response:
    if req.param('id') == 'unexpected':
        cache.command(['NoSuchCommand', req.header('Authorization')])
    cache.get('redis-key-private')
%s    if req.param('id') == 'slow':
        client.get(%q)
    return http.json(200, {'request_id': req.request_id})

routes.add('GET', '/items/{id}', read)
routes.require_auth('GET', '/items/{id}', token)
routes.authorize('GET', '/items/{id}', policy, 'read')
`, pgSetup, "redis://"+redis.Addr(), pgQuery, upstream.URL)))
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ExecuteModule(module, false); err != nil {
		t.Fatal(err)
	}
	entry := module.Globals["routes"]
	router, _ := routerValue(entry)
	pool := newHTTPWorkerPool(t.Context(), 1, 1)
	vm := &VM{engine: engine}
	if err := pool.load(vm, entry); err != nil {
		t.Fatal(err)
	}
	for _, route := range router.routes {
		route.auth = rateFixtureAuth{"bearer-private": {subject: "reader", tenant: "tenant-private", roles: []string{"reader"}}}
	}
	observer, err := newHTTPObserver(t.Context(), pool, []*Engine{engine})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(observer.close)
	var logs observationLog
	observer.logger = slog.New(slog.NewJSONHandler(&logs, nil))
	handler := observer.wrap(pool, router)
	request := func(path, proxyID string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer bearer-private")
		req.Header.Set("X-Request-ID", proxyID)
		req.Header.Set("Traceparent", "00-"+traceID+"-00f067aa0ba902b7-01")
		got := httptest.NewRecorder()
		handler.ServeHTTP(got, req)
		return got
	}
	metricsClient := &http.Client{Timeout: time.Second}
	t.Cleanup(metricsClient.CloseIdleConnections)
	scrape := func() string {
		t.Helper()
		response, err := metricsClient.Get("http://" + observer.metricsServer.Addr + "/metrics")
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil || response.StatusCode != 200 {
			t.Fatalf("metrics: %d %v", response.StatusCode, err)
		}
		return string(body)
	}

	ready := make(chan *httptest.ResponseRecorder, 2)
	go func() { ready <- request("/items/slow", "proxy-a") }()
	select {
	case inboundTrace := <-arrived:
		if !strings.HasPrefix(inboundTrace, "00-"+traceID+"-") {
			t.Fatalf("downstream did not get the request trace: %q", inboundTrace)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("slow request did not arrive at the upstream")
	}
	go func() { ready <- request("/items/url-private", "proxy-b") }()
	deadline := time.Now().Add(time.Second)
	for pool.metrics.waiting.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	for _, name := range []string{
		"typhon_http_busy_engines 1", "typhon_http_queued 1",
		`typhon_driver_pool_connections{driver="redis",state="total"} 1`,
	} {
		if values := scrape(); !strings.Contains(values, name) {
			t.Fatalf("missing live metric %q", name)
		}
	}
	releaseWork()
	for range 2 {
		select {
		case response := <-ready:
			id := response.Header().Get("X-Request-ID")
			if response.Code != 200 || !strings.HasPrefix(id, "proxy-") || !strings.Contains(response.Body.String(), id) {
				t.Fatalf("request ID did not reach handler and response: %d %s", response.Code, response.Body.String())
			}
		case <-time.After(3 * time.Second):
			t.Fatal("queued requests did not finish")
		}
	}
	if response := request("/items/unexpected", "proxy-error"); response.Code != 500 {
		t.Fatalf("expected a sanitized Redis failure, got %d", response.Code)
	}
	values := scrape()
	for _, metric := range []string{
		`typhon_http_requests_total{method="GET",route="/items/{id}",status="200"} 2`,
		`typhon_http_stage_duration_seconds_count{method="GET",route="/items/{id}",stage="queue"} 3`,
		`typhon_driver_operation_duration_seconds_count{driver="redis",operation="get",outcome="ok"} 2`,
		"go_memstats_heap_alloc_bytes",
	} {
		if !strings.Contains(values, metric) {
			t.Fatalf("missing metric %q", metric)
		}
	}

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := observer.provider.ForceFlush(ctx); err != nil {
		t.Fatal(err)
	}
	parent := map[string]bool{}
	var spans []*tracepb.Span
	for len(recorded) > 0 {
		span := <-recorded
		spans = append(spans, span)
		if span.Name == "GET /items/{id}" && fmt.Sprintf("%x", span.ParentSpanId) == "00f067aa0ba902b7" {
			parent[string(span.SpanId)] = true
		}
	}
	children := map[string]bool{}
	for _, span := range spans {
		if fmt.Sprintf("%x", span.TraceId) != traceID {
			t.Fatalf("span lost incoming trace: %s", span.Name)
		}
		if parent[string(span.ParentSpanId)] {
			children[span.Name] = true
		}
	}
	if !children["redis.get"] || !children["http.get"] || (pgURL != "" && !children["postgres.query"]) {
		t.Fatalf("missing driver children of HTTP spans: %v", children)
	}
	log := logs.String()
	for _, value := range []string{"bearer-private", "redis-key-private", "postgres-parameter-private", "tenant-private", "url-private"} {
		if strings.Contains(log+values, value) {
			t.Fatal("request or driver values leaked into telemetry")
		}
	}
	if !strings.Contains(log, `"request_id":"proxy-error"`) || !strings.Contains(log, `"trace_id":"`+traceID+`"`) ||
		!strings.Contains(log, `"error_code":"internal_error"`) || strings.Contains(log, `"tenant":""`) {
		t.Fatalf("missing request ID, trace, error code or pseudonym in JSON: %s", log)
	}
}

func TestNativeHTTPObservability(t *testing.T) {
	checkHTTPObservability(t, "")
}

func TestNativeHTTPObservabilityPostgresIntegration(t *testing.T) {
	dsn := os.Getenv("TYPHON_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("set TYPHON_TEST_POSTGRES_URL to verify traced PostgreSQL queries")
	}
	checkHTTPObservability(t, dsn)
}
