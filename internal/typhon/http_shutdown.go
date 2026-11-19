package typhon

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const defaultHTTPShutdownGrace = 5 * time.Second

func httpShutdownGrace() (time.Duration, error) {
	raw := os.Getenv("TYPHON_HTTP_SHUTDOWN_GRACE")
	if raw == "" {
		return defaultHTTPShutdownGrace, nil
	}
	grace, err := time.ParseDuration(raw)
	if err != nil || grace < 0 || grace > time.Minute {
		return 0, errors.New("TYPHON_HTTP_SHUTDOWN_GRACE must be a duration between 0s and 1m")
	}
	return grace, nil
}

// ShutdownExitTimeout bounds CLI shutdown even when an external operation
// ignores context cancellation. The Go runtime never force-exits its host.
func ShutdownExitTimeout() time.Duration {
	grace, err := httpShutdownGrace()
	if err != nil {
		grace = defaultHTTPShutdownGrace
	}
	return grace + 8*time.Second
}

func httpReadinessPath(liveness string) (string, error) {
	path := os.Getenv("TYPHON_READINESS_PATH")
	if path != "" && (!validHealthPath(path) || path == liveness) {
		return "", errors.New("TYPHON_READINESS_PATH must be an absolute HTTP path distinct from liveness")
	}
	return path, nil
}

func validHealthPath(path string) bool {
	return strings.HasPrefix(path, "/") && !strings.ContainsAny(path, "\t\r\n ?#")
}

type httpDrain struct {
	handler   http.Handler
	pool      *httpWorkerPool
	readiness string
	health    *httpHealth
	mu        sync.Mutex
	draining  bool
	active    int
	done      chan struct{}
}

func newHTTPDrain(handler http.Handler, pool *httpWorkerPool, readiness string) *httpDrain {
	return &httpDrain{handler: handler, pool: pool, readiness: readiness, done: make(chan struct{})}
}

func (d *httpDrain) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if d.pool.liveness != "" && request.URL.Path == d.pool.liveness {
		d.pool.ServeHTTP(writer, request)
		return
	}
	d.mu.Lock()
	readyPath := d.readiness != "" && request.URL.Path == d.readiness
	diagnostics := d.health != nil && d.health.diagnostics != "" && request.URL.Path == d.health.diagnostics
	if readyPath || diagnostics {
		draining := d.draining
		d.mu.Unlock()
		writer.Header().Set("Cache-Control", "no-store")
		if request.Method != http.MethodGet && request.Method != http.MethodHead {
			writer.Header().Set("Allow", "GET, HEAD")
			writer.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		capacity := len(d.pool.admission) < cap(d.pool.admission)
		dependenciesReady, dependencies := d.health.status()
		ready := !draining && capacity && dependenciesReady
		switch {
		case diagnostics:
			d.health.writeDiagnostics(writer, request, ready, draining, capacity, dependencies)
		case !ready:
			writer.WriteHeader(http.StatusServiceUnavailable)
		default:
			writer.WriteHeader(http.StatusOK)
		}
		return
	}
	if d.draining {
		d.mu.Unlock()
		d.pool.reject(writer, "server shutting down", problemWriter{enabled: d.pool.apiErrors, head: request.Method == http.MethodHead})
		return
	}
	d.active++
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.active--
		if d.draining && d.active == 0 {
			close(d.done)
		}
	}()
	d.handler.ServeHTTP(writer, request)
}

func (d *httpDrain) start() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.draining {
		return d.active
	}
	d.draining = true
	if d.active == 0 {
		close(d.done)
	}
	return d.active
}

func (d *httpDrain) shutdown(server *http.Server, cancelWorkers context.CancelFunc, grace time.Duration) {
	log.Printf("Typhon HTTP shutdown stage=drain active=%d grace=%s", d.start(), grace)
	if d.pool.stopEvents != nil {
		d.pool.stopEvents()
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-d.done:
	case <-timer.C:
		log.Print("Typhon HTTP shutdown stage=cancel")
	}
	cancelWorkers()
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		reportCleanupError("HTTP server shutdown", driverFailure("HTTP", connectionKind, err))
		reportCleanupError("HTTP server close", driverFailure("HTTP", connectionKind, server.Close()))
	}
	log.Print("Typhon HTTP shutdown stage=listener_closed")
}
