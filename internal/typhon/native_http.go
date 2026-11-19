package typhon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

const maxHTTPBody = 8 * 1024 * 1024

func httpDriverTypes() map[string]Type {
	header := nativeFuncType("http.header", []Type{StrType()}, StrType(), 0)
	response := Type{Kind: TypeNative, Name: "typhon.http.Response", Members: map[string]Type{
		"status": IntType(), "body": StrType(), "header": header,
	}}
	request := Type{Kind: TypeNative, Name: "typhon.http.Request", Members: map[string]Type{
		"method": StrType(), "path": StrType(), "body": StrType(), "header": header,
		"query":     nativeFuncType("http.query", []Type{StrType()}, StrType(), 0),
		"json":      nativeFuncType("http.request.json", nil, AnyType(), 0),
		"remaining": nativeFuncType("http.request.remaining", nil, FloatType(), 0),
		"cancelled": nativeFuncType("http.request.cancelled", nil, BoolType(), 0),
	}}
	headers := listType(listType(StrType()))
	client := Type{Kind: TypeNative, Name: "typhon.http.Client", Members: map[string]Type{
		"get":     nativeFuncType("http.get", []Type{StrType()}, response, 0),
		"request": nativeFuncType("http.request", []Type{StrType(), StrType(), StrType(), headers}, response, 1),
		"close":   nativeFuncType("http.close", nil, NoneType(), 0),
	}}
	return map[string]Type{
		"Request": request, "Response": response, "Client": client,
		"client":       nativeFuncType("http.client", []Type{FloatType()}, client, 1),
		"response":     nativeFuncType("http.response", []Type{IntType(), StrType(), headers}, response, 1),
		"json":         nativeFuncType("http.json", []Type{IntType(), AnyType()}, response, 0),
		"serve":        nativeFuncType("http.serve", []Type{StrType(), nativeFuncType("http.handler", []Type{request}, response, 0), IntType(), IntType(), FloatType()}, NoneType(), 3),
		"server_stats": nativeFuncType("http.server_stats", nil, dictType(StrType(), IntType()), 0),
		"env":          nativeFuncType("http.env", []Type{StrType()}, StrType(), 0),
	}
}

func httpDriverExports(types map[string]Type) map[string]Value {
	exports := driverTypeSymbols(types)
	exports["client"] = driverFunc(types["client"], nativeHTTPClient)
	exports["response"] = driverFunc(types["response"], func(vm *VM, args []Value) (Value, error) {
		headers := http.Header{}
		if len(args) == 3 {
			var err error
			headers, err = httpHeaders(args[2])
			if err != nil {
				return NoneValue(), err
			}
		}
		return httpResponse(vm, args[0].Int, args[1].Str, headers)
	})
	exports["json"] = driverFunc(types["json"], func(vm *VM, args []Value) (Value, error) {
		body, err := dumpJSON(vm, args[1])
		if err != nil {
			return NoneValue(), err
		}
		return httpResponse(vm, args[0].Int, body, http.Header{"Content-Type": {"application/json; charset=utf-8"}})
	})
	exports["serve"] = driverFunc(types["serve"], nativeHTTPServe)
	exports["server_stats"] = driverFunc(types["server_stats"], func(vm *VM, args []Value) (Value, error) {
		metrics := vm.engine.httpMetrics.Load()
		if metrics == nil {
			return DictValue(), nil
		}
		return metrics.snapshot(), nil
	})
	exports["env"] = driverFunc(types["env"], driverEnv)
	return exports
}

func httpHeaders(value Value) (http.Header, error) {
	headers := http.Header{}
	if value.List == nil {
		return nil, fmt.Errorf("headers must be a list of [name, value] pairs")
	}
	for _, pair := range *value.List {
		if pair.List == nil || len(*pair.List) != 2 {
			return nil, fmt.Errorf("header must contain name and value")
		}
		name, content := (*pair.List)[0].Str, (*pair.List)[1].Str
		if name == "" {
			return nil, fmt.Errorf("invalid HTTP header")
		}
		for _, ch := range content {
			if ch < ' ' && ch != '\t' || ch == 127 {
				return nil, fmt.Errorf("invalid HTTP header value")
			}
		}
		for _, ch := range name {
			if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", ch)) {
				return nil, fmt.Errorf("invalid HTTP header name")
			}
		}
		headers.Add(name, content)
	}
	return headers, nil
}

func httpResponse(vm *VM, status int64, body string, headers http.Header) (Value, error) {
	if status < 200 || status > 599 {
		return NoneValue(), fmt.Errorf("response status must be between 200 and 599")
	}
	if len(body) > maxHTTPBody {
		return NoneValue(), fmt.Errorf("HTTP body limit exceeded")
	}
	if err := vm.chargeBytes(int64(len(body))); err != nil {
		return NoneValue(), err
	}
	typ := httpDriverTypes()["Response"]
	value := nativeObject(vm, typ, map[string]Value{
		"status": IntValue(status), "body": StringValue(body),
		"header": driverFunc(typ.Members["header"], func(vm *VM, args []Value) (Value, error) { return StringValue(headers.Get(args[0].Str)), nil }),
	})
	value.NativeObject.data = headers.Clone()
	return value, nil
}

func nativeHTTPClient(vm *VM, args []Value) (Value, error) {
	timeout, err := driverTimeout(args, 0)
	if err != nil {
		return NoneValue(), err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxConnsPerHost = 16
	client := &http.Client{Transport: transport, Timeout: timeout}
	var closed atomic.Bool
	scope := vm.resourceScope()
	close := func(context.Context) error {
		if !closed.Swap(true) {
			scope.remove(&closed)
			transport.CloseIdleConnections()
		}
		return nil
	}
	if err := scope.add(vm.context(), &closed, "HTTP client close", close); err != nil {
		return NoneValue(), err
	}
	typ := httpDriverTypes()["Client"]
	request := func(vm *VM, args []Value) (Value, error) {
		if closed.Load() {
			return NoneValue(), fmt.Errorf("HTTP client is closed")
		}
		if len(args[2].Str) > maxHTTPBody {
			return NoneValue(), fmt.Errorf("HTTP body limit exceeded")
		}
		if err := vm.chargeBytes(int64(len(args[2].Str))); err != nil {
			return NoneValue(), err
		}
		ctx, cancel := driverContext(vm, timeout)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, args[0].Str, args[1].Str, strings.NewReader(args[2].Str))
		if err != nil {
			return NoneValue(), driverFailure("HTTP", validationKind, err)
		}
		if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
			return NoneValue(), fmt.Errorf("HTTP URL must use http or https")
		}
		if len(args) == 4 {
			req.Header, err = httpHeaders(args[3])
			if err != nil {
				return NoneValue(), err
			}
		}
		resp, err := client.Do(req)
		if err != nil {
			return NoneValue(), driverFailure("HTTP", connectionKind, err)
		}
		budget := vm.executionBudget()
		limit := min(int64(maxHTTPBody), budget.byteLimit-budget.bytes)
		body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
		closeErr := driverFailure("HTTP", connectionKind, resp.Body.Close())
		if err != nil {
			reportCleanupError("HTTP response body close", closeErr)
			return NoneValue(), driverFailure("HTTP", connectionKind, err)
		}
		if closeErr != nil {
			return NoneValue(), closeErr
		}
		if int64(len(body)) > limit && limit < maxHTTPBody {
			return NoneValue(), quotaStop("byte", budget.byteLimit)
		}
		return httpResponse(vm, int64(resp.StatusCode), string(body), resp.Header)
	}
	return scopedNativeObject(vm, typ, map[string]Value{
		"request": driverFunc(typ.Members["request"], request),
		"get": driverFunc(typ.Members["get"], func(vm *VM, args []Value) (Value, error) {
			return request(vm, []Value{StringValue("GET"), args[0], StringValue("")})
		}),
		"close": driverCloser(typ.Members["close"], close),
	}), nil
}

func httpVMHandler(engine *Engine, handler Value) http.Handler {
	return httpVMHandlerWithTimeout(engine, handler, defaultRequestTimeout)
}

func httpVMHandlerWithTimeout(engine *Engine, handler Value, timeout time.Duration) http.Handler {
	// ponytail: at most 16 queued/running requests, while the Engine serializes
	// Typhon execution; use independent Engines when parallel CPU work is needed.
	admission := make(chan struct{}, 16)
	return http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		select {
		case admission <- struct{}{}:
			defer func() { <-admission }()
		default:
			http.Error(writer, "server busy", http.StatusServiceUnavailable)
			return
		}
		if !engine.beginExecution() {
			http.Error(writer, "server shutting down", http.StatusServiceUnavailable)
			return
		}
		defer engine.active.Done()
		ctx, cancel := httpRequestContext(req.Context(), engine.ctx, timeout)
		defer cancel()
		if ctx.Err() != nil {
			writeHTTPStop(writer, engine.ctx, ctx.Err())
			return
		}
		body, err := readHTTPRequestBody(writer, req, ctx, min(int64(maxHTTPBody), engine.quotas.requestBytes-valueSlotBytes))
		if ctx.Err() != nil {
			writeHTTPStop(writer, engine.ctx, ctx.Err())
			return
		}
		if err != nil {
			var network net.Error
			var oversized *http.MaxBytesError
			if errors.As(err, &oversized) {
				if metrics := engine.httpMetrics.Load(); metrics != nil {
					metrics.quotaRejected.Add(1)
				}
			}
			if errors.As(err, &network) && network.Timeout() {
				http.Error(writer, "request body timed out", http.StatusRequestTimeout)
			} else {
				http.Error(writer, "request body is too large or unreadable", http.StatusRequestEntityTooLarge)
			}
			return
		}
		scope := &requestScope{cancel: cancel}
		child := &VM{engine: engine, ctx: ctx, request: scope, depth: 1}
		child.executionBudget().bytes = int64(len(body)) + valueSlotBytes
		typ := httpDriverTypes()["Request"]
		bodyText := string(body)
		attrs := map[string]Value{
			"method": StringValue(req.Method), "path": StringValue(req.URL.Path), "body": StringValue(bodyText),
			"header": driverFunc(typ.Members["header"], func(vm *VM, args []Value) (Value, error) { return StringValue(req.Header.Get(args[0].Str)), nil }),
			"query":  driverFunc(typ.Members["query"], func(vm *VM, args []Value) (Value, error) { return StringValue(req.URL.Query().Get(args[0].Str)), nil }),
			"json":   driverFunc(typ.Members["json"], func(vm *VM, args []Value) (Value, error) { return loadJSON(vm, bodyText) }),
			"remaining": driverFunc(typ.Members["remaining"], func(vm *VM, args []Value) (Value, error) {
				deadline, _ := ctx.Deadline()
				if ctx.Err() != nil {
					return FloatValue(0), nil
				}
				return FloatValue(max(0, time.Until(deadline).Seconds())), nil
			}),
			"cancelled": driverFunc(typ.Members["cancelled"], func(vm *VM, args []Value) (Value, error) {
				return BoolValue(ctx.Err() != nil), nil
			}),
		}
		attrs["remaining"].Native.cleanup = true
		attrs["cancelled"].Native.cleanup = true
		request := scopedNativeObject(child, typ, attrs)
		result, runErr := func() (Value, error) {
			engine.execMu.Lock()
			defer engine.execMu.Unlock()
			defer cancel()
			return child.runFunction(handler.Function, []Value{request})
		}()
		cleanupErr := scope.finish(engine)
		var stopped *executionStop
		if errors.As(runErr, &stopped) && stopped.cancelled {
			reportCleanupError("HTTP request", cleanupErr)
			writeHTTPStop(writer, engine.ctx, stopped.cause)
			return
		}
		primary := runErr
		if primary == nil {
			primary = cleanupErr
		}
		if primary != nil {
			log.Printf("Typhon HTTP handler: %v", finalizeRun(runErr, cleanupErr))
			var quota *QuotaError
			if errors.As(primary, &quota) {
				writeHTTPQuota(writer, engine, quota)
				return
			}
			var ex *Exception
			if errors.As(primary, &ex) && ex.kind == timeoutKind {
				http.Error(writer, "upstream operation timed out", http.StatusGatewayTimeout)
				return
			}
			http.Error(writer, "internal server error", http.StatusInternalServerError)
			return
		}
		if req.Context().Err() != nil || engine.ctx.Err() != nil {
			writeHTTPStop(writer, engine.ctx, req.Context().Err())
			return
		}
		if deadline, _ := ctx.Deadline(); time.Now().After(deadline) {
			writeHTTPStop(writer, engine.ctx, context.DeadlineExceeded)
			return
		}
		if result.Kind != ValueNativeObject || !result.NativeObject.Type.Equal(httpDriverTypes()["Response"]) {
			http.Error(writer, "invalid handler response", http.StatusInternalServerError)
			return
		}
		responseAttrs := result.NativeObject.Attrs
		if err := child.executionBudget().chargeBytes(int64(len(responseAttrs["body"].Str))); err != nil {
			var quota *QuotaError
			if errors.As(err, &quota) {
				writeHTTPQuota(writer, engine, quota)
				return
			}
		}
		headers := result.NativeObject.data.(http.Header)
		for name, values := range headers {
			writer.Header()[name] = values
		}
		writer.WriteHeader(int(responseAttrs["status"].Int))
		_, _ = io.WriteString(writer, responseAttrs["body"].Str)
	})
}

func writeHTTPQuota(writer http.ResponseWriter, engine *Engine, quota *QuotaError) {
	if metrics := engine.httpMetrics.Load(); metrics != nil {
		metrics.quotaRejected.Add(1)
	}
	http.Error(writer, quota.Error(), http.StatusUnprocessableEntity)
}

func nativeHTTPServe(vm *VM, args []Value) (_ Value, runErr error) {
	handler := args[1]
	types := httpDriverTypes()
	if handler.Kind != ValueFunction || len(handler.Function.Signature.Params) != 1 || !handler.Function.Signature.Params[0].Equal(types["Request"]) || handler.Function.Signature.Return == nil || !handler.Function.Signature.Return.Equal(types["Response"]) {
		return NoneValue(), fmt.Errorf("handler must have signature (Request) -> Response")
	}
	if vm.engine.httpWorker || vm.engine.httpMetrics.Load() != nil {
		return NoneValue(), fmt.Errorf("HTTP serve is unavailable during worker initialization or a running server")
	}
	workers, queue := int64(1), min(int64(15), vm.engine.quotas.queue)
	if len(args) > 2 {
		workers = args[2].Int
	}
	if len(args) > 3 {
		queue = args[3].Int
	}
	if workers < 1 || workers > 16 || queue < 0 || queue > vm.engine.quotas.queue {
		return NoneValue(), fmt.Errorf("HTTP serve requires 1 to 16 workers and a queue size from 0 to %d", vm.engine.quotas.queue)
	}
	requestTimeout, err := driverTimeout(args, 4)
	if err != nil {
		return NoneValue(), err
	}
	listener, err := net.Listen("tcp", args[0].Str)
	if err != nil {
		return NoneValue(), driverFailure("HTTP", connectionKind, err)
	}
	pool := newHTTPWorkerPool(vm.context(), int(workers), int(queue))
	pool.requestTimeout = requestTimeout
	pool.liveness = vm.engine.quotas.liveness
	vm.engine.httpMetrics.Store(&pool.metrics)
	defer func() {
		vm.engine.httpMetrics.Store(nil)
		cleanupErr := pool.close()
		if vm.context().Err() != nil {
			reportCleanupError("HTTP workers", cleanupErr)
		}
		runErr = finalizeRun(runErr, cleanupErr)
	}()
	if err := pool.load(vm, handler); err != nil {
		reportCleanupError("HTTP listener after worker startup failure", driverFailure("HTTP", connectionKind, listener.Close()))
		return NoneValue(), err
	}
	server := &http.Server{Handler: pool, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 * 1024}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	vm.engine.execMu.Unlock()
	select {
	case err = <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			reportCleanupError("HTTP server close", driverFailure("HTTP", connectionKind, server.Close()))
		}
	case <-vm.context().Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		shutdownErr := server.Shutdown(shutdown)
		cancel()
		if shutdownErr != nil {
			reportCleanupError("HTTP server shutdown", driverFailure("HTTP", connectionKind, shutdownErr))
			reportCleanupError("HTTP server close", driverFailure("HTTP", connectionKind, server.Close()))
		}
		err = <-done
	}
	vm.engine.execMu.Lock()
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return NoneValue(), driverFailure("HTTP", connectionKind, err)
}
