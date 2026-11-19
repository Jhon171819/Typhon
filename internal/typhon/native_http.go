package typhon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
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
		"query":                   nativeFuncType("http.query", []Type{StrType()}, StrType(), 0),
		"query_all":               nativeFuncType("http.query_all", []Type{StrType()}, listType(StrType()), 0),
		"param":                   nativeFuncType("http.param", []Type{StrType()}, StrType(), 0),
		"json":                    nativeFuncType("http.request.json", nil, AnyType(), 0),
		"remaining":               nativeFuncType("http.request.remaining", nil, FloatType(), 0),
		"cancelled":               nativeFuncType("http.request.cancelled", nil, BoolType(), 0),
		"request_id":              StrType(),
		"identity":                unionType(StrType(), NoneType()),
		"tenant":                  unionType(StrType(), NoneType()),
		"idempotency_fingerprint": unionType(StrType(), NoneType()),
		"state":                   nativeFuncType("http.request.state", []Type{StrType()}, unionType(StrType(), NoneType()), 0),
		"set_state":               nativeFuncType("http.request.set_state", []Type{StrType(), StrType()}, NoneType(), 0),
	}}
	headers := listType(listType(StrType()))
	bodyType, form, part := httpStreamTypes()
	request.Members["stream"] = nativeFuncType("http.request.stream", nil, bodyType, 0)
	request.Members["multipart"] = nativeFuncType("http.request.multipart", nil, form, 0)
	producer := nativeFuncType("http.stream.producer", []Type{AnyType()}, unionType(scalarType("Bytes"), NoneType()), 0)
	client := Type{Kind: TypeNative, Name: "typhon.http.Client", Members: map[string]Type{
		"get":     nativeFuncType("http.get", []Type{StrType()}, response, 0),
		"request": nativeFuncType("http.request", []Type{StrType(), StrType(), StrType(), headers}, response, 1),
		"close":   nativeFuncType("http.close", nil, NoneType(), 0),
	}}
	types := map[string]Type{
		"Request": request, "Response": response, "Client": client,
		"client":   nativeFuncType("http.client", []Type{FloatType()}, client, 1),
		"response": nativeFuncType("http.response", []Type{IntType(), StrType(), headers}, response, 1),
		"json":     nativeFuncType("http.json", []Type{IntType(), AnyType()}, response, 0),
		"stream":   nativeFuncType("http.stream", []Type{IntType(), producer, AnyType(), headers}, response, 1),
		"Body":     bodyType, "Multipart": form, "Part": part,
		"serve":        nativeFuncType("http.serve", []Type{StrType(), unionType(httpHandlerType(), Type{Kind: TypeNative, Name: routerTypeName}), IntType(), IntType(), FloatType()}, NoneType(), 3),
		"server_stats": nativeFuncType("http.server_stats", nil, dictType(StrType(), IntType()), 0),
		"env":          nativeFuncType("http.env", []Type{StrType()}, StrType(), 0),
	}
	httpSSETypes(types)
	return types
}

func httpDriverExports(types map[string]Type) map[string]Value {
	exports := driverTypeSymbols(types)
	exports["client"] = driverFunc(types["client"], nativeHTTPClient)
	exports["stream"] = driverFunc(types["stream"], makeHTTPStream)
	exports["event"] = driverFunc(types["event"], makeSSEEvent)
	exports["sse"] = driverFunc(types["sse"], makeSSEResponse)
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
		if !validHTTPHeaderValue(content) {
			return nil, fmt.Errorf("invalid HTTP header value")
		}
		if !validHTTPHeaderName(name) {
			return nil, fmt.Errorf("invalid HTTP header name")
		}
		headers.Add(name, content)
	}
	return headers, nil
}

func validHTTPHeaderName(name string) bool {
	return name != "" && strings.IndexFunc(name, func(ch rune) bool {
		return !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", ch))
	}) < 0
}

func validHTTPHeaderValue(value string) bool {
	return strings.IndexFunc(value, func(ch rune) bool { return ch < ' ' && ch != '\t' || ch == 127 }) < 0
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
		req, err := http.NewRequestWithContext(ctx, args[0].Str, args[1].Str, strings.NewReader(args[2].Str)) // #nosec G704 -- The trusted Typhon application chooses its upstream URL.
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
		injectHTTPTrace(req)
		resp, err := client.Do(req) // #nosec G704 -- The trusted Typhon application chooses its upstream URL.
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
	server := httpVMHandlerWithTimeout(engine, handler, defaultRequestTimeout)
	if router, ok := routerValue(handler); ok {
		return router.browser.wrap(server, router.apiErrors)
	}
	return server
}

func httpVMHandlerWithTimeout(engine *Engine, handler Value, timeout time.Duration) http.Handler {
	return httpVMHandlerWithContext(engine, handler, timeout, engine.ctx)
}

func httpVMHandlerWithContext(engine *Engine, handler Value, timeout time.Duration, serverContext context.Context) http.Handler {
	// ponytail: at most 16 queued/running requests, while the Engine serializes
	// Typhon execution; use independent Engines when parallel CPU work is needed.
	admission := make(chan struct{}, 16)
	return http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		webState := newWebRequest(req)
		problems := problemWriter{id: webState.id, head: req.Method == http.MethodHead}
		router, routed := routerValue(handler)
		var route *webRoute
		if routed {
			problems.enabled = router.apiErrors
			route = router.requestRoute(webState.path, req.Method)
		}
		var sse *httpSSEPolicy
		lastEvent := ""
		if route != nil {
			sse = route.sse
		}
		if sse == nil {
			select {
			case admission <- struct{}{}:
				defer func() { <-admission }()
			default:
				problems.write(writer, http.StatusServiceUnavailable, "server busy")
				return
			}
		} else {
			var valid bool
			lastEvent, valid = lastSSEEvent(req)
			if !valid || req.ContentLength != 0 {
				problems.write(writer, 400, "invalid event stream request")
				return
			}
		}
		if !engine.beginExecution() {
			problems.write(writer, http.StatusServiceUnavailable, "server shutting down")
			return
		}
		defer engine.active.Done()
		ctx, cancel := httpRequestContext(req.Context(), serverContext, timeout)
		defer cancel()
		if ctx.Err() != nil {
			writeHTTPStop(writer, serverContext, ctx.Err(), problems)
			return
		}
		var upload *httpUploadPolicy
		if route != nil {
			upload = route.upload
		}
		var inbound *httpInboundStream
		var body []byte
		var err error
		if upload == nil {
			body, err = readHTTPRequestBody(writer, req, ctx, min(int64(maxHTTPBody), engine.quotas.requestBytes-valueSlotBytes))
		} else if req.ContentLength > upload.total {
			err = &http.MaxBytesError{Limit: upload.total}
		} else {
			inbound = &httpInboundStream{
				ctx: ctx, body: http.MaxBytesReader(writer, req.Body, upload.total), transport: req.Body, contentType: req.Header.Get("Content-Type"),
				controller: http.NewResponseController(writer), policy: *upload,
			}
			// net/http otherwise drains the upload before committing a
			// response, including when the multipart parser rejected it.
			_ = inbound.controller.EnableFullDuplex()
			defer func() { reportCleanupError("HTTP streaming request body", inbound.release()) }()
		}
		if ctx.Err() != nil {
			writeHTTPStop(writer, serverContext, ctx.Err(), problems)
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
				problems.write(writer, http.StatusRequestTimeout, "request body timed out")
			} else {
				problems.write(writer, http.StatusRequestEntityTooLarge, "request body is too large or unreadable")
			}
			return
		}
		scope := &requestScope{cancel: cancel, web: webState}
		if inbound != nil {
			if err := scope.resources.add(ctx, inbound, "HTTP streaming request body", inbound.close); err != nil {
				problems.write(writer, http.StatusInternalServerError, "could not initialize streaming request body")
				return
			}
		}
		child := &VM{engine: engine, ctx: ctx, request: scope, depth: 1}
		child.executionBudget().bytes = int64(len(body)) + valueSlotBytes
		typ := httpDriverTypes()["Request"]
		bodyText := string(body)
		query := req.URL.Query()
		attrs := map[string]Value{
			"method": StringValue(req.Method), "path": StringValue(req.URL.Path), "body": StringValue(bodyText),
			"header": driverFunc(typ.Members["header"], func(vm *VM, args []Value) (Value, error) { return StringValue(req.Header.Get(args[0].Str)), nil }),
			"query":  driverFunc(typ.Members["query"], func(vm *VM, args []Value) (Value, error) { return StringValue(query.Get(args[0].Str)), nil }),
			"query_all": driverFunc(typ.Members["query_all"], func(vm *VM, args []Value) (Value, error) {
				matches := query[args[0].Str]
				if err := vm.chargeBytes(int64(len(matches)) * 2 * valueSlotBytes); err != nil {
					return NoneValue(), err
				}
				items := make([]Value, len(matches))
				for i, match := range matches {
					items[i] = StringValue(match)
				}
				return ListValue(items), nil
			}),
			"param": driverFunc(typ.Members["param"], func(vm *VM, args []Value) (Value, error) {
				return StringValue(webState.params[args[0].Str]), nil
			}),
			"json": driverFunc(typ.Members["json"], func(vm *VM, args []Value) (Value, error) {
				if inbound != nil {
					return NoneValue(), errors.New("buffered JSON is unavailable on streaming upload routes")
				}
				return loadJSON(vm, bodyText)
			}),
			"stream":    driverFunc(typ.Members["stream"], func(vm *VM, args []Value) (Value, error) { return inbound.raw(vm) }),
			"multipart": driverFunc(typ.Members["multipart"], func(vm *VM, args []Value) (Value, error) { return inbound.multipart(vm) }),
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
		attrs["request_id"] = StringValue(webState.id)
		attrs["identity"] = optionalWebString(webState.principal.subject, webState.principal.subject != "")
		attrs["tenant"] = optionalWebString(webState.principal.tenant, webState.principal.tenant != "")
		attrs["idempotency_fingerprint"] = NoneValue()
		attrs["state"] = driverFunc(typ.Members["state"], func(vm *VM, args []Value) (Value, error) {
			value, exists := webState.state[args[0].Str]
			return optionalWebString(value, exists), nil
		})
		attrs["set_state"] = driverFunc(typ.Members["set_state"], func(vm *VM, args []Value) (Value, error) {
			return NoneValue(), webState.setState(vm, args[0].Str, args[1].Str)
		})
		request := scopedNativeObject(child, typ, attrs)
		request.NativeObject.data = webState
		var stream *httpStreamingResponse
		var events *httpSSEResponse
		var stopEvents context.CancelFunc
		result, runErr := func() (Value, error) {
			engine.execMu.Lock()
			defer engine.execMu.Unlock()
			started := time.Now()
			result, err := callHTTPHandler(child, handler, request)
			observeRequest(ctx).stage("vm", time.Since(started))
			if err == nil && result.Kind == ValueNativeObject {
				stream, _ = result.NativeObject.data.(*httpStreamingResponse)
				events, _ = result.NativeObject.data.(*httpSSEResponse)
			}
			if events != nil {
				if sse == nil && req.Method == http.MethodHead && route == nil && routed {
					if get := router.requestRoute(webState.path, http.MethodGet); get != nil {
						sse = get.sse
					}
				}
				if sse == nil {
					cancel()
					return NoneValue(), errors.New("configure router.enable_sse for an event stream")
				}
				if err := child.executionError(); err != nil {
					cancel()
					return NoneValue(), err
				}
				cancel()
				if req.Method != http.MethodHead {
					ctx, stopEvents = newHTTPSSEContext(req, serverContext, webState.principal, sse)
					child.ctx, scope.cancel = ctx, stopEvents
				}
			} else if stream == nil {
				// Stop request tasks before they can acquire the VM lock.
				cancel()
			}
			return result, err
		}()
		streamStarted := false
		if stream != nil {
			streamStarted, runErr = writeHTTPStream(child, writer, req, webState, result, stream)
		}
		if events != nil && runErr == nil {
			if req.Method == http.MethodHead {
				setHTTPResponseHeaders(writer, webState, sseResponseHeaders())
				writer.WriteHeader(http.StatusOK)
				streamStarted = true
			} else {
				streamStarted, runErr = writeSSEResponse(child, writer, webState, lastEvent, sse, events, timeout)
			}
		}
		if stopEvents != nil {
			stopEvents()
		}
		cancel()
		cleanupErr := scope.finish(engine)
		if inbound != nil && !inbound.ended && !streamStarted {
			writer.Header().Set("Connection", "close")
		}
		if streamStarted {
			reportCleanupError("HTTP stream", cleanupErr)
			if runErr != nil {
				logWebFailure(webState.id, "", http.StatusInternalServerError, runErr)
				// A completed HTTP message would hide truncation from the client.
				panic(http.ErrAbortHandler)
			}
			return
		}
		if webState.sendID {
			writer.Header().Set("X-Request-ID", webState.id)
		}
		var stopped *executionStop
		if errors.As(runErr, &stopped) && stopped.cancelled {
			reportCleanupError("HTTP request", cleanupErr)
			writeHTTPStop(writer, serverContext, stopped.cause, problems)
			return
		}
		primary := runErr
		if primary == nil {
			primary = cleanupErr
		}
		if primary != nil {
			var rejection *webRejection
			if inbound != nil && errors.As(primary, &rejection) {
				problems.write(writer, rejection.status, rejection.Error())
				return
			}
			if !problems.enabled {
				if observation := observeRequest(req.Context()); observation == nil || observation.observer.logger == nil {
					log.Printf("Typhon HTTP handler: %v", finalizeRun(runErr, cleanupErr))
				} else {
					logWebFailure(webState.id, observation.route, http.StatusInternalServerError, runErr)
				}
			}
			var quota *QuotaError
			if errors.As(primary, &quota) {
				if problems.enabled {
					logWebFailure(problems.id, "", http.StatusUnprocessableEntity, primary)
				}
				writeHTTPQuota(writer, engine, quota, problems)
				return
			}
			if problems.enabled {
				status := webErrorStatus(primary)
				logWebFailure(problems.id, "", status, primary)
				problems.write(writer, status, "")
				return
			}
			var ex *Exception
			if errors.As(primary, &ex) && ex.kind == timeoutKind {
				problems.write(writer, http.StatusGatewayTimeout, "upstream operation timed out")
				return
			}
			problems.write(writer, http.StatusInternalServerError, "internal server error")
			return
		}
		if req.Context().Err() != nil || serverContext.Err() != nil {
			writeHTTPStop(writer, serverContext, req.Context().Err(), problems)
			return
		}
		if deadline, _ := ctx.Deadline(); time.Now().After(deadline) {
			writeHTTPStop(writer, serverContext, context.DeadlineExceeded, problems)
			return
		}
		if result.Kind != ValueNativeObject || !result.NativeObject.Type.Equal(httpDriverTypes()["Response"]) {
			problems.write(writer, http.StatusInternalServerError, "invalid handler response")
			return
		}
		responseAttrs := result.NativeObject.Attrs
		if req.Method != http.MethodHead {
			if err := child.executionBudget().chargeBytes(int64(len(responseAttrs["body"].Str))); err != nil {
				var quota *QuotaError
				if errors.As(err, &quota) {
					writeHTTPQuota(writer, engine, quota, problems)
					return
				}
			}
		}
		setHTTPResponseHeaders(writer, webState, result.NativeObject.data.(http.Header))
		status := int(responseAttrs["status"].Int)
		if req.Method == http.MethodHead && writer.Header().Get("Content-Length") == "" && status != 204 && status != 304 {
			writer.Header().Set("Content-Length", strconv.Itoa(len(responseAttrs["body"].Str)))
		}
		writer.WriteHeader(status)
		if req.Method != http.MethodHead {
			_, _ = io.WriteString(writer, responseAttrs["body"].Str)
		}
	})
}

func setHTTPResponseHeaders(writer http.ResponseWriter, state *webRequest, headers http.Header) {
	for name, values := range headers {
		writer.Header()[name] = values
	}
	for name, values := range state.rateHeaders {
		writer.Header()[name] = values
	}
	if state.sendID {
		writer.Header().Set("X-Request-ID", state.id)
	}
}

func writeHTTPQuota(writer http.ResponseWriter, engine *Engine, quota *QuotaError, problems problemWriter) {
	if metrics := engine.httpMetrics.Load(); metrics != nil {
		metrics.quotaRejected.Add(1)
	}
	problems.write(writer, http.StatusUnprocessableEntity, quota.Error())
}

func nativeHTTPServe(vm *VM, args []Value) (_ Value, runErr error) {
	handler := args[1]
	if vm.engine.openAPIExport {
		return NoneValue(), fmt.Errorf("put http.serve inside the main module guard when exporting OpenAPI")
	}
	if _, router := routerValue(handler); !router && (handler.Kind != ValueFunction || !handler.Function.Signature.Equal(httpHandlerType())) {
		return NoneValue(), fmt.Errorf("handler must be a Router or a function (Request) -> Response")
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
	grace, err := httpShutdownGrace()
	if err != nil {
		return NoneValue(), err
	}
	readiness, err := httpReadinessPath(vm.engine.quotas.liveness)
	if err != nil {
		return NoneValue(), err
	}
	listener, err := net.Listen("tcp", args[0].Str)
	if err != nil {
		return NoneValue(), driverFailure("HTTP", connectionKind, err)
	}
	workerContext, cancelWorkers := context.WithCancel(context.WithoutCancel(vm.context()))
	defer cancelWorkers()
	stopStartup := context.AfterFunc(vm.context(), cancelWorkers)
	defer stopStartup()
	pool := newHTTPWorkerPool(workerContext, int(workers), int(queue))
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
	if !stopStartup() || vm.context().Err() != nil {
		reportCleanupError("HTTP listener after startup cancellation", driverFailure("HTTP", connectionKind, listener.Close()))
		return NoneValue(), vm.context().Err()
	}
	httpHandler := pool.browser.wrap(pool, pool.apiErrors)
	if router, ok := routerValue(handler); ok {
		httpHandler, err = router.docsHandler(httpHandler)
		if err != nil {
			reportCleanupError("HTTP listener after docs configuration failure", driverFailure("HTTP", connectionKind, listener.Close()))
			return NoneValue(), err
		}
	}
	observer, err := newHTTPObserver(workerContext, pool, append([]*Engine{vm.engine}, pool.engines...))
	if err != nil {
		reportCleanupError("HTTP listener after observability configuration failure", driverFailure("HTTP", connectionKind, listener.Close()))
		return NoneValue(), err
	}
	defer observer.close()
	router, _ := routerValue(handler)
	drain := newHTTPDrain(observer.wrap(httpHandler, router), pool, readiness)
	health, err := newHTTPHealth(workerContext, readiness, pool.liveness)
	if err != nil {
		reportCleanupError("HTTP listener after health configuration failure", driverFailure("HTTP", connectionKind, listener.Close()))
		return NoneValue(), err
	}
	drain.health = health
	defer health.close()
	server := &http.Server{Handler: drain, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 * 1024}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	vm.engine.execMu.Unlock()
	select {
	case err = <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			reportCleanupError("HTTP server close", driverFailure("HTTP", connectionKind, server.Close()))
		}
	case <-vm.context().Done():
		drain.shutdown(server, cancelWorkers, grace)
		err = <-done
	}
	vm.engine.execMu.Lock()
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return NoneValue(), driverFailure("HTTP", connectionKind, err)
}
