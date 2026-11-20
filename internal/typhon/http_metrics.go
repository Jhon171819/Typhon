package typhon

import (
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
)

type httpObservabilityMetrics struct {
	requests *prometheus.CounterVec
	stages   *prometheus.HistogramVec
	driver   *prometheus.HistogramVec
}

func newHTTPObservabilityMetrics(pool *httpWorkerPool, engines []*Engine) (*httpObservabilityMetrics, http.Handler) {
	registry := prometheus.NewRegistry()
	buckets := []float64{0.001, 0.003, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}
	metrics := &httpObservabilityMetrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "typhon_http_requests_total", Help: "Completed HTTP requests by template, method and status.",
		}, []string{"route", "method", "status"}),
		stages: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "typhon_http_stage_duration_seconds", Help: "HTTP stage durations by route template.", Buckets: buckets,
		}, []string{"route", "method", "stage"}),
		driver: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "typhon_driver_operation_duration_seconds", Help: "Native driver operation durations.", Buckets: buckets,
		}, []string{"driver", "operation", "outcome"}),
	}
	registry.MustRegister(metrics.requests, metrics.stages, metrics.driver, collectors.NewGoCollector())
	for _, gauge := range []struct {
		name, description string
		value             func() float64
	}{
		{"typhon_http_queued", "Requests waiting for a VM worker.", func() float64 { return float64(pool.metrics.waiting.Load()) }},
		{"typhon_http_busy_engines", "VM workers serving ordinary requests.", func() float64 { return float64(cap(pool.idle) - len(pool.idle)) }},
		{"typhon_http_event_connections", "Open server-sent event streams.", func() float64 { return float64(pool.metrics.eventConnections.Load()) }},
	} {
		registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: gauge.name, Help: gauge.description}, gauge.value))
	}
	registry.MustRegister(&driverPoolCollector{
		engines: engines,
		desc:    prometheus.NewDesc("typhon_driver_pool_connections", "Engine-owned driver pool connections by state.", []string{"driver", "state"}, nil),
	})
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{MaxRequestsInFlight: 2}))
	return metrics, mux
}

type driverPoolCollector struct {
	engines []*Engine
	desc    *prometheus.Desc
}

func (c *driverPoolCollector) Describe(out chan<- *prometheus.Desc) { out <- c.desc }

func (c *driverPoolCollector) Collect(out chan<- prometheus.Metric) {
	postgres := map[*pgxpool.Pool]bool{}
	caches := map[*redis.Client]bool{}
	for _, engine := range c.engines {
		engine.resources.mu.Lock()
		for handle := range engine.resources.entries {
			switch item := handle.(type) {
			case *postgresConnection:
				postgres[item.pool] = true
			case *redisConnection:
				caches[item.client] = true
			}
		}
		engine.resources.mu.Unlock()
	}
	var pgTotal, pgIdle, pgUsed, redisTotal, redisIdle int64
	for pool := range postgres {
		stats := pool.Stat()
		pgTotal += int64(stats.TotalConns())
		pgIdle += int64(stats.IdleConns())
		pgUsed += int64(stats.AcquiredConns())
	}
	for client := range caches {
		stats := client.PoolStats()
		redisTotal += int64(stats.TotalConns)
		redisIdle += int64(stats.IdleConns)
	}
	for _, metric := range []struct {
		driver, state string
		value         int64
	}{
		{"postgres", "total", pgTotal}, {"postgres", "idle", pgIdle}, {"postgres", "in_use", pgUsed},
		{"redis", "total", redisTotal}, {"redis", "idle", redisIdle}, {"redis", "in_use", max(0, redisTotal-redisIdle)},
	} {
		out <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(metric.value), metric.driver, metric.state)
	}
}
