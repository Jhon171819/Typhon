package typhon

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

type httpObserver struct {
	logger        *slog.Logger
	tracer        trace.Tracer
	provider      *sdktrace.TracerProvider
	metrics       *httpObservabilityMetrics
	metricsServer *http.Server
	trustID       bool
	tenantKey     []byte
}

type requestObservationKey struct{}

type requestObservation struct {
	observer *httpObserver
	id       string
	route    string
	method   string
	tenant   string
	queue    time.Duration
	vm       time.Duration
}

func observeRequest(ctx context.Context) *requestObservation {
	observation, _ := ctx.Value(requestObservationKey{}).(*requestObservation)
	return observation
}

func newHTTPObserver(ctx context.Context, pool *httpWorkerPool, engines []*Engine) (*httpObserver, error) {
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT")
	metricsAddress := os.Getenv("TYPHON_METRICS_ADDR")
	logging := envEnabled("TYPHON_HTTP_JSON_LOGS")
	if endpoint == "" && metricsAddress == "" && !logging && !envEnabled("TYPHON_TRUST_REQUEST_ID") {
		return nil, nil
	}
	observer := &httpObserver{
		trustID:   envEnabled("TYPHON_TRUST_REQUEST_ID"),
		tenantKey: []byte(os.Getenv("TYPHON_TENANT_LOG_KEY")),
	}
	if len(observer.tenantKey) == 0 {
		observer.tenantKey = []byte(rand.Text())
	} else if len(observer.tenantKey) < 32 {
		return nil, errors.New("TYPHON_TENANT_LOG_KEY requires at least 32 bytes")
	}
	instance := rand.Text()
	if logging {
		observer.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil)).With("instance_id", instance)
	}
	if endpoint != "" {
		target, err := url.Parse(endpoint)
		if err != nil || target.Hostname() == "" || target.User != nil || target.RawQuery != "" ||
			target.Fragment != "" || (target.Scheme != "http" && target.Scheme != "https") {
			return nil, errors.New("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT must be an HTTP or HTTPS collector URL without credentials, query or fragment")
		}
		ratio := 0.1
		if raw := os.Getenv("TYPHON_TRACE_SAMPLE_RATIO"); raw != "" {
			ratio, err = strconv.ParseFloat(raw, 64)
			if err != nil || math.IsNaN(ratio) || ratio < 0 || ratio > 1 {
				return nil, errors.New("TYPHON_TRACE_SAMPLE_RATIO must be between 0 and 1")
			}
		}
		exporter, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(endpoint),
			otlptracehttp.WithTimeout(time.Second), otlptracehttp.WithRetry(otlptracehttp.RetryConfig{Enabled: false}))
		if err != nil {
			return nil, errors.New("could not initialize OTLP HTTP exporter")
		}
		service := os.Getenv("OTEL_SERVICE_NAME")
		if !validAuthText(service, 80) {
			service = "typhon"
		}
		observer.provider = sdktrace.NewTracerProvider(
			sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))),
			sdktrace.WithResource(resource.NewSchemaless(attribute.String("service.name", service), attribute.String("service.instance.id", instance))),
			sdktrace.WithBatcher(exporter, sdktrace.WithMaxQueueSize(2048), sdktrace.WithBatchTimeout(time.Second), sdktrace.WithExportTimeout(time.Second)),
		)
		observer.tracer = observer.provider.Tracer("typhon")
	}
	if metricsAddress != "" {
		listener, err := net.Listen("tcp", metricsAddress)
		if err != nil {
			observer.close()
			return nil, errors.New("could not bind Typhon metrics listener")
		}
		metrics, handler := newHTTPObservabilityMetrics(pool, engines)
		observer.metrics = metrics
		observer.metricsServer = &http.Server{
			Addr: listener.Addr().String(), Handler: handler, ReadHeaderTimeout: 2 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 10 * time.Second,
		}
		go func() {
			if err := observer.metricsServer.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
				log.Print("Typhon metrics listener stopped")
			}
		}()
	}
	return observer, nil
}

func (o *httpObserver) close() {
	if o == nil {
		return
	}
	if o.metricsServer != nil {
		reportCleanupError("HTTP metrics listener", o.metricsServer.Close())
	}
	if o.provider != nil {
		ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		reportCleanupError("OTLP trace exporter", o.provider.Shutdown(ctx))
	}
}

func observedHTTPMethod(method string) string {
	switch method {
	case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "CONNECT", "TRACE":
		return method
	default:
		return "_OTHER"
	}
}

type observedHTTPWriter struct {
	http.ResponseWriter
	status    int
	requestID string
}

func (w *observedHTTPWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *observedHTTPWriter) WriteHeader(status int) {
	if w.status == 0 && status >= 200 {
		w.status = status
		w.Header().Set("X-Request-ID", w.requestID)
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *observedHTTPWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
		w.Header().Set("X-Request-ID", w.requestID)
	}
	return w.ResponseWriter.Write(body)
}

func (o *httpObserver) wrap(next http.Handler, router *webRouter) http.Handler {
	if o == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		route := "handler"
		if router != nil {
			route = "unmatched"
			if matched := router.requestRoute(req.URL.EscapedPath(), req.Method); matched != nil {
				route = matched.pattern
			}
		}
		requestID := rand.Text()
		if o.trustID {
			if values := req.Header.Values("X-Request-ID"); len(values) == 1 &&
				len(values[0]) <= 64 && validIdempotencyKey(values[0]) {
				requestID = values[0]
			}
		}
		observation := &requestObservation{observer: o, id: requestID, route: route, method: observedHTTPMethod(req.Method)}
		ctx := propagation.TraceContext{}.Extract(req.Context(), propagation.HeaderCarrier(req.Header))
		var span trace.Span
		if o.tracer != nil {
			ctx, span = o.tracer.Start(ctx, observation.method+" "+route,
				trace.WithSpanKind(trace.SpanKindServer),
				trace.WithAttributes(attribute.String("http.request.method", observation.method), attribute.String("http.route", route)))
		}

		ctx = context.WithValue(ctx, requestObservationKey{}, observation)
		w.Header().Set("X-Request-ID", requestID)
		writer := &observedHTTPWriter{ResponseWriter: w, requestID: requestID}
		started := time.Now()
		completed := false
		defer func() {
			status := writer.status
			if status == 0 {
				status = 500
				if completed {
					status = 200
				}
			}
			duration := time.Since(started)
			if span != nil {
				span.SetAttributes(attribute.Int("http.response.status_code", status),
					attribute.Int64("typhon.queue.duration_ns", observation.queue.Nanoseconds()),
					attribute.Int64("typhon.vm.duration_ns", observation.vm.Nanoseconds()))
				if status >= 500 || !completed {
					span.SetStatus(codes.Error, "request failed")
				}
				span.End()
			}
			if o.metrics != nil {
				o.metrics.requests.WithLabelValues(route, observation.method, strconv.Itoa(status)).Inc()
				observation.stage("total", duration)
			}
			if o.logger != nil {
				traceID := ""
				if id := trace.SpanContextFromContext(ctx).TraceID(); id.IsValid() {
					traceID = id.String()
				}
				errorCode := webStatuses[status].code
				if status >= 400 && errorCode == "" {
					errorCode = "http_error"
				}
				o.logger.Info("http_request", "request_id", requestID, "trace_id", traceID,
					"route", route, "method", observation.method, "status", status, "duration_ms", float64(duration)/float64(time.Millisecond),
					"tenant", observation.tenant, "error_code", errorCode)
			}
		}()
		next.ServeHTTP(writer, req.WithContext(ctx))
		completed = true
	})
}

func (o *requestObservation) setTenant(tenant string) {
	if o.observer.logger == nil || tenant == "" {
		return
	}
	mac := hmac.New(sha256.New, o.observer.tenantKey)
	_, _ = mac.Write([]byte(tenant))
	o.tenant = hex.EncodeToString(mac.Sum(nil)[:12])
}

func (o *requestObservation) stage(stage string, duration time.Duration) {
	if o == nil {
		return
	}
	switch stage {
	case "queue":
		o.queue = duration
	case "vm":
		o.vm = duration
	}
	if o.observer.metrics != nil {
		o.observer.metrics.stages.WithLabelValues(o.route, o.method, stage).Observe(duration.Seconds())
	}
}

func observedNativeCall(vm *VM, fn *NativeFunc, args []Value) (Value, error) {
	if vm.cleanupCtx != nil {
		return fn.Fn(vm, args)
	}
	observation := observeRequest(vm.context())
	if observation == nil {
		return fn.Fn(vm, args)
	}
	driver, operation, _ := strings.Cut(fn.Name, ".")
	switch driver {
	case "postgres":
		if strings.HasPrefix(operation, "rows.") || operation == "stats" || operation == "env" ||
			operation == "array" || operation == "jsonb" || operation == "null" {
			return fn.Fn(vm, args)
		}
	case "redis":
		if operation == "stats" || operation == "env" {
			return fn.Fn(vm, args)
		}
	case "http":
		if operation != "request" && operation != "get" && operation != "request_json" && operation != "request_bytes" {
			return fn.Fn(vm, args)
		}
	default:
		return fn.Fn(vm, args)
	}
	var span trace.Span
	old := vm.ctx
	if o := observation.observer; o.tracer != nil {
		vm.ctx, span = o.tracer.Start(vm.context(), fn.Name, trace.WithSpanKind(trace.SpanKindClient),
			trace.WithAttributes(attribute.String("typhon.driver", driver), attribute.String("typhon.operation", operation)))
		defer func() { vm.ctx = old }()
	}
	start := time.Now()
	value, err := fn.Fn(vm, args)
	if metrics := observation.observer.metrics; metrics != nil {
		outcome := "ok"
		if err != nil {
			outcome = "error"
		}
		metrics.driver.WithLabelValues(driver, operation, outcome).Observe(time.Since(start).Seconds())
	}
	if span != nil {
		if err != nil {
			span.SetStatus(codes.Error, "operation failed")
		}
		span.End()
	}
	return value, err
}

func injectHTTPTrace(req *http.Request) {
	if request := observeRequest(req.Context()); request != nil && request.observer.tracer != nil {
		propagation.TraceContext{}.Inject(req.Context(), propagation.HeaderCarrier(req.Header))
	}
}
