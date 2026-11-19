package typhon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"
)

type httpPoolMetrics struct {
	admitted, rejected, completed atomic.Int64
	waiting, peak, waitNS         atomic.Int64
	quotaRejected                 atomic.Int64
}

func (m *httpPoolMetrics) snapshot() Value {
	result := DictValue()
	for _, metric := range []struct {
		name  string
		value *atomic.Int64
	}{
		{"admitted", &m.admitted}, {"rejected", &m.rejected}, {"completed", &m.completed},
		{"queued", &m.waiting}, {"queue_peak", &m.peak}, {"queue_wait_ns", &m.waitNS},
		{"quota_rejected", &m.quotaRejected},
	} {
		_ = result.Dict.put(StringValue(metric.name), IntValue(metric.value.Load()))
	}
	return result
}

type httpWorkerPool struct {
	ctx            context.Context
	idle           chan http.Handler
	admission      chan struct{}
	engines        []*Engine
	metrics        httpPoolMetrics
	requestTimeout time.Duration
	liveness       string
}

func newHTTPWorkerPool(ctx context.Context, workers, queue int) *httpWorkerPool {
	return &httpWorkerPool{
		ctx: ctx, idle: make(chan http.Handler, workers),
		admission: make(chan struct{}, workers+queue), requestTimeout: defaultRequestTimeout,
	}
}

func (p *httpWorkerPool) load(vm *VM, handler Value) error {
	p.idle <- httpVMHandlerWithTimeout(vm.engine, handler, p.requestTimeout)
	for i := 1; i < cap(p.idle); i++ {
		if err := vm.executionError(); err != nil {
			return err
		}
		engine := NewEngineWithContext(p.ctx)
		engine.httpWorker = true
		engine.httpMetrics.Store(&p.metrics)
		p.engines = append(p.engines, engine)
		module, err := engine.LoadModule(handler.Function.ModulePath)
		if err == nil {
			err = engine.ExecuteModule(module, false)
		}
		if err != nil {
			return fmt.Errorf("HTTP worker %d initialization: %w", i+1, err)
		}
		entry := module.Globals[handler.Function.Name]
		if entry.Kind != ValueFunction || !entry.Function.Signature.Equal(handler.Function.Signature) {
			return fmt.Errorf("HTTP worker %d could not resolve handler %q", i+1, handler.Function.Name)
		}
		p.idle <- httpVMHandlerWithTimeout(engine, entry, p.requestTimeout)
	}
	return nil
}

func (p *httpWorkerPool) close() error {
	var failures []error
	for i := len(p.engines) - 1; i >= 0; i-- {
		if err := p.engines[i].Close(); err != nil {
			failures = append(failures, fmt.Errorf("HTTP worker %d: %w", i+2, err))
		}
	}
	return errors.Join(failures...)
}

func (p *httpWorkerPool) ServeHTTP(writer http.ResponseWriter, req *http.Request) {
	if p.liveness != "" && req.URL.Path == p.liveness {
		if req.Method == http.MethodGet || req.Method == http.MethodHead {
			writer.WriteHeader(http.StatusNoContent)
		} else {
			writer.Header().Set("Allow", "GET, HEAD")
			http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}
	if p.ctx.Err() != nil {
		p.reject(writer, "server shutting down")
		return
	}
	ctx, cancel := httpRequestContext(req.Context(), p.ctx, p.requestTimeout)
	defer cancel()
	select {
	case p.admission <- struct{}{}:
		defer func() { <-p.admission }()
	default:
		p.reject(writer, "server busy")
		return
	}
	p.metrics.admitted.Add(1)
	var worker http.Handler
	select {
	case worker = <-p.idle:
	default:
		worker = p.wait(ctx)
	}
	if worker == nil {
		p.metrics.rejected.Add(1)
		writeHTTPStop(writer, p.ctx, ctx.Err())
		return
	}
	defer func() { p.idle <- worker }()
	if ctx.Err() != nil || p.ctx.Err() != nil {
		p.metrics.rejected.Add(1)
		writeHTTPStop(writer, p.ctx, ctx.Err())
		return
	}
	worker.ServeHTTP(writer, req.WithContext(ctx))
	p.metrics.completed.Add(1)
}

func (p *httpWorkerPool) wait(ctx context.Context) http.Handler {
	start := time.Now()
	queued := p.metrics.waiting.Add(1)
	for peak := p.metrics.peak.Load(); queued > peak; peak = p.metrics.peak.Load() {
		if p.metrics.peak.CompareAndSwap(peak, queued) {
			break
		}
	}
	defer func() {
		p.metrics.waiting.Add(-1)
		p.metrics.waitNS.Add(time.Since(start).Nanoseconds())
	}()
	select {
	case worker := <-p.idle:
		return worker
	case <-ctx.Done():
	case <-p.ctx.Done():
	}
	return nil
}

func (p *httpWorkerPool) reject(writer http.ResponseWriter, message string) {
	p.metrics.rejected.Add(1)
	http.Error(writer, message, http.StatusServiceUnavailable)
}
