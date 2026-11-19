package typhon

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNativeHTTPWorkerProgressAndIsolation(t *testing.T) {
	arrived := make(chan struct{}, 2)
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer upstream.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	engine := NewEngine()
	defer engine.Close()
	path := sourceFile(t, fmt.Sprintf(`import typhon.http as http
visits: list[int] = []
upstream: http.Client = http.client(4)
def handle(req: http.Request) -> http.Response:
    if req.path == '/ready':
        return http.response(200, 'ready')
    if req.path == '/stats':
        return http.json(200, http.server_stats())
    visits.append(1)
    upstream.get(%q)
    return http.json(200, len(visits))
if __name__ == '__main__':
    http.serve(%q, handle, 2, 1)
`, upstream.URL, address))
	module, err := engine.LoadModule(path)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- engine.ExecuteModule(module, true) }()
	client := &http.Client{Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	base := "http://" + address
	deadline := time.Now().Add(3 * time.Second)
	for {
		response, err := client.Get(base + "/ready")
		if err == nil {
			response.Body.Close()
			if response.StatusCode != 200 {
				t.Fatalf("ready status %d", response.StatusCode)
			}
			break
		}
		select {
		case err := <-done:
			t.Fatalf("serve failed: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("server did not listen", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	results := make(chan string, 2)
	for range 2 {
		go func() {
			response, err := client.Get(base + "/work")
			if err != nil {
				results <- err.Error()
				return
			}
			body, err := io.ReadAll(response.Body)
			response.Body.Close()
			results <- fmt.Sprintf("%d %s %v", response.StatusCode, body, err)
		}()
	}
	for range 2 {
		select {
		case <-arrived:
		case <-time.After(2 * time.Second):
			t.Fatal("two Typhon workers did not reach the upstream concurrently")
		}
	}
	close(release)
	for range 2 {
		if got := <-results; got != "200 1 <nil>" {
			t.Fatalf("workers shared mutable state or failed: %s", got)
		}
	}
	response, err := client.Get(base + "/stats")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || !strings.Contains(string(body), `"admitted": 4`) || !strings.Contains(string(body), `"completed": 3`) {
		t.Fatalf("metrics: %q %v", body, err)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("server did not stop on Engine close: %v", err)
	}
}

func TestNativeHTTPWorkerBackpressureAndCancellation(t *testing.T) {
	pool := newHTTPWorkerPool(context.Background(), 1, 1)
	started, release := make(chan struct{}), make(chan struct{})
	pool.idle <- http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		close(started)
		<-release
		writer.WriteHeader(204)
	})
	active := make(chan int, 1)
	go func() { active <- workerStatus(pool, context.Background()) }()
	defer func() { close(release); <-active }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not start")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	queued := make(chan int, 1)
	go func() { queued <- workerStatus(pool, ctx) }()
	deadline := time.Now().Add(2 * time.Second)
	for pool.metrics.waiting.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if pool.metrics.waiting.Load() != 1 {
		t.Fatal("request did not enter queue")
	}
	if status := workerStatus(pool, context.Background()); status != 503 {
		t.Fatalf("overload returned %d", status)
	}
	cancel()
	select {
	case status := <-queued:
		if status != 503 {
			t.Errorf("cancelled wait returned %d", status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queue ignored client cancellation")
	}
	if pool.metrics.rejected.Load() != 2 || pool.metrics.waiting.Load() != 0 ||
		pool.metrics.peak.Load() != 1 || pool.metrics.waitNS.Load() == 0 {
		t.Fatal("incorrect queue or rejection metrics")
	}
}

func workerStatus(pool *httpWorkerPool, ctx context.Context) int {
	recorder := httptest.NewRecorder()
	pool.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx))
	return recorder.Code
}

func TestNativeHTTPWorkerStress(t *testing.T) {
	for _, count := range []int{1, 10, 100, 500} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			pool := newHTTPWorkerPool(context.Background(), 4, 16)
			var active, peak atomic.Int64
			for range 4 {
				pool.idle <- http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					current := active.Add(1)
					for previous := peak.Load(); current > previous; previous = peak.Load() {
						if peak.CompareAndSwap(previous, current) {
							break
						}
					}
					time.Sleep(time.Millisecond)
					active.Add(-1)
					w.WriteHeader(204)
				})
			}
			start := make(chan struct{})
			var waits sync.WaitGroup
			for range count {
				waits.Go(func() {
					<-start
					if status := workerStatus(pool, context.Background()); status != 204 && status != 503 {
						t.Errorf("unexpected status %d", status)
					}
				})
			}
			close(start)
			waits.Wait()
			if peak.Load() > 4 || active.Load() != 0 || pool.metrics.waiting.Load() != 0 ||
				pool.metrics.completed.Load()+pool.metrics.rejected.Load() != int64(count) {
				t.Fatal("incorrect accounting or concurrency limit")
			}
		})
	}
}

func TestNativePostgresHTTPWorkersIntegration(t *testing.T) {
	dsn := os.Getenv("TYPHON_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("set TYPHON_TEST_POSTGRES_URL for a dedicated test PostgreSQL")
	}
	arrived, release := make(chan struct{}, 3), make(chan struct{}, 3)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived <- struct{}{}
		select {
		case <-release:
			w.WriteHeader(204)
		case <-r.Context().Done():
		}
	}))
	defer upstream.Close()
	engine := NewEngine()
	defer engine.Close()
	module, err := engine.LoadModule(sourceFile(t, fmt.Sprintf(`import typhon.postgres as pg
import typhon.http as http
db: pg.Connection = pg.connect(%q, 2)
upstream: http.Client = http.client(4)
def handle(req: http.Request) -> http.Response:
    upstream.get(%q)
    return http.json(200, db.query('SELECT pg_backend_pid() AS pid').get(0, 'pid'))
`, dsn, upstream.URL)))
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ExecuteModule(module, false); err != nil {
		t.Fatal(err)
	}
	pool := newHTTPWorkerPool(engine.ctx, 3, 0)
	defer pool.close()
	if err := pool.load(&VM{engine: engine}, module.Globals["handle"]); err != nil {
		t.Fatal(err)
	}
	backends := map[string]bool{}
	for round := range 4 {
		results := make(chan string, 3)
		for range 3 {
			go func() {
				response := httptest.NewRecorder()
				pool.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
				results <- fmt.Sprintf("%d %s", response.Code, response.Body.String())
			}()
		}
		for range 3 {
			select {
			case <-arrived:
			case <-time.After(2 * time.Second):
				t.Fatalf("round %d: workers failed to reach I/O barrier", round)
			}
		}
		for range 3 {
			release <- struct{}{}
		}
		for range 3 {
			result := <-results
			if !strings.HasPrefix(result, "200 ") {
				t.Fatalf("PostgreSQL worker failed: %s", result)
			}
			backends[result] = true
		}
	}
	if len(backends) != 3 {
		t.Fatalf("expected exactly one reused PostgreSQL backend per worker, got %d", len(backends))
	}
}

func BenchmarkNativeHTTPWorkerThroughput(b *testing.B) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(time.Millisecond)
		w.WriteHeader(204)
	}))
	defer server.Close()
	path := filepath.Join(b.TempDir(), "bench.ty")
	source := fmt.Sprintf(`import typhon.http as http
upstream: http.Client = http.client()
def handle(req: http.Request) -> http.Response:
    result: http.Response = upstream.get(%q)
    return http.response(result.status, '')
`, server.URL)
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		b.Fatal(err)
	}
	for _, workers := range []int{1, 4} {
		b.Run(fmt.Sprintf("workers=%d", workers), func(b *testing.B) {
			engine := NewEngine()
			defer engine.Close()
			module, err := engine.LoadModule(path)
			if err != nil {
				b.Fatal(err)
			}
			if err := engine.ExecuteModule(module, false); err != nil {
				b.Fatal(err)
			}
			pool := newHTTPWorkerPool(engine.ctx, workers, 256)
			defer pool.close()
			if err := pool.load(&VM{engine: engine}, module.Globals["handle"]); err != nil {
				b.Fatal(err)
			}
			latencies := make([]time.Duration, b.N)
			var index atomic.Int64
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					start := time.Now()
					if status := workerStatus(pool, context.Background()); status != 204 {
						b.Errorf("status %d", status)
					}
					latencies[index.Add(1)-1] = time.Since(start)
				}
			})
			b.StopTimer()
			sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
			for _, percentile := range []int{50, 95, 99} {
				i := (len(latencies) - 1) * percentile / 100
				b.ReportMetric(float64(latencies[i].Microseconds()), fmt.Sprintf("p%d-us", percentile))
			}
			b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "req/s")
		})
	}
}
