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
		"query": nativeFuncType("http.query", []Type{StrType()}, StrType(), 0),
	}}
	headers := listType(listType(StrType()))
	client := Type{Kind: TypeNative, Name: "typhon.http.Client", Members: map[string]Type{
		"get":     nativeFuncType("http.get", []Type{StrType()}, response, 0),
		"request": nativeFuncType("http.request", []Type{StrType(), StrType(), StrType(), headers}, response, 1),
		"close":   nativeFuncType("http.close", nil, NoneType(), 0),
	}}
	return map[string]Type{
		"Request": request, "Response": response, "Client": client,
		"client":   nativeFuncType("http.client", []Type{FloatType()}, client, 1),
		"response": nativeFuncType("http.response", []Type{IntType(), StrType(), headers}, response, 1),
		"json":     nativeFuncType("http.json", []Type{IntType(), AnyType()}, response, 0),
		"serve":    nativeFuncType("http.serve", []Type{StrType(), nativeFuncType("http.handler", []Type{request}, response, 0)}, NoneType(), 0),
		"env":      nativeFuncType("http.env", []Type{StrType()}, StrType(), 0),
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
		body, err := renderPythonJSON(args[1])
		if err != nil {
			return NoneValue(), err
		}
		return httpResponse(vm, args[0].Int, body, http.Header{"Content-Type": {"application/json; charset=utf-8"}})
	})
	exports["serve"] = driverFunc(types["serve"], nativeHTTPServe)
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
	close := func() error {
		if !closed.Swap(true) {
			scope.remove(&closed)
			transport.CloseIdleConnections()
		}
		return nil
	}
	if err := scope.add(&closed, close); err != nil {
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
		ctx, cancel := driverContext(vm, timeout)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, args[0].Str, args[1].Str, strings.NewReader(args[2].Str))
		if err != nil {
			return NoneValue(), err
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
			return NoneValue(), err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxHTTPBody+1))
		if err != nil {
			return NoneValue(), err
		}
		return httpResponse(vm, int64(resp.StatusCode), string(body), resp.Header)
	}
	return nativeObject(vm, typ, map[string]Value{
		"request": driverFunc(typ.Members["request"], request),
		"get": driverFunc(typ.Members["get"], func(vm *VM, args []Value) (Value, error) {
			return request(vm, []Value{StringValue("GET"), args[0], StringValue("")})
		}),
		"close": driverFunc(typ.Members["close"], func(vm *VM, args []Value) (Value, error) { return NoneValue(), close() }),
	}), nil
}

func httpVMHandler(engine *Engine, handler Value) http.Handler {
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
		body, err := io.ReadAll(http.MaxBytesReader(writer, req.Body, maxHTTPBody))
		if err != nil {
			http.Error(writer, "request body is too large or unreadable", http.StatusRequestEntityTooLarge)
			return
		}
		ctx, cancel := context.WithTimeout(req.Context(), 10*time.Second)
		defer cancel()
		stop := context.AfterFunc(engine.ctx, cancel)
		defer stop()
		scope := &resourceScope{}
		child := &VM{engine: engine, ctx: ctx, resources: scope}
		typ := httpDriverTypes()["Request"]
		request := nativeObject(child, typ, map[string]Value{
			"method": StringValue(req.Method), "path": StringValue(req.URL.Path), "body": StringValue(string(body)),
			"header": driverFunc(typ.Members["header"], func(vm *VM, args []Value) (Value, error) { return StringValue(req.Header.Get(args[0].Str)), nil }),
			"query":  driverFunc(typ.Members["query"], func(vm *VM, args []Value) (Value, error) { return StringValue(req.URL.Query().Get(args[0].Str)), nil }),
		})
		result, runErr := child.runFunction(handler.Function, []Value{request})
		cleanupErr := scope.close()
		if runErr == nil {
			runErr = cleanupErr
		}
		if runErr != nil {
			log.Printf("Typhon HTTP handler: %v", runErr)
			http.Error(writer, "internal server error", http.StatusInternalServerError)
			return
		}
		if result.Kind != ValueNativeObject || !result.NativeObject.Type.Equal(httpDriverTypes()["Response"]) {
			http.Error(writer, "invalid handler response", http.StatusInternalServerError)
			return
		}
		attrs := result.NativeObject.Attrs
		headers := result.NativeObject.data.(http.Header)
		for name, values := range headers {
			writer.Header()[name] = values
		}
		writer.WriteHeader(int(attrs["status"].Int))
		_, _ = io.WriteString(writer, attrs["body"].Str)
	})
}

func nativeHTTPServe(vm *VM, args []Value) (Value, error) {
	handler := args[1]
	types := httpDriverTypes()
	if handler.Kind != ValueFunction || len(handler.Function.Signature.Params) != 1 || !handler.Function.Signature.Params[0].Equal(types["Request"]) || handler.Function.Signature.Return == nil || !handler.Function.Signature.Return.Equal(types["Response"]) {
		return NoneValue(), fmt.Errorf("handler must have signature (Request) -> Response")
	}
	listener, err := net.Listen("tcp", args[0].Str)
	if err != nil {
		return NoneValue(), err
	}
	server := &http.Server{Handler: httpVMHandler(vm.engine, handler), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 * 1024}
	scope := vm.resourceScope()
	if err := scope.add(server, server.Close); err != nil {
		_ = listener.Close()
		return NoneValue(), err
	}
	defer scope.remove(server)
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	vm.engine.execMu.Unlock()
	select {
	case err = <-done:
	case <-vm.context().Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = server.Shutdown(shutdown)
		cancel()
		_ = server.Close()
		err = <-done
	}
	vm.engine.execMu.Lock()
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return NoneValue(), err
}
