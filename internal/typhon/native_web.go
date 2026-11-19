package typhon

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const routerTypeName = "typhon.web.Router"
const maxWebRoutes = 256
const maxWebPath = 4096
const maxWebSegments = 32

type webRoute struct {
	handler Value
	names   []string
	pattern string
	steps   []webStep
}

type webNode struct {
	literal map[string]*webNode
	param   *webNode
	methods map[string]*webRoute
}

type webRouter struct {
	root      webNode
	module    *LoadedModule
	size      int
	frozen    bool
	routes    map[[2]string]*webRoute
	steps     []webStep
	apiErrors bool
}

type webRequest struct {
	path      string
	params    map[string]string
	id        string
	sendID    bool
	state     map[string]string
	principal verifiedRequestIdentity
}

func httpHandlerType() Type {
	return nativeFuncType("http.handler", []Type{{Kind: TypeNative, Name: "typhon.http.Request"}},
		Type{Kind: TypeNative, Name: "typhon.http.Response"}, 0)
}

func webDriverTypes() map[string]Type {
	httpTypes := httpDriverTypes()
	router := Type{Kind: TypeNative, Name: routerTypeName, Members: map[string]Type{
		"add":      nativeFuncType("web.add", []Type{StrType(), StrType(), httpHandlerType()}, NoneType(), 0),
		"dispatch": nativeFuncType("web.dispatch", []Type{httpTypes["Request"]}, httpTypes["Response"], 0),
	}}
	next := webNextType()
	middleware := nativeFuncType("web.middleware", []Type{httpTypes["Request"], next}, httpTypes["Response"], 0)
	router.Members["use"] = nativeFuncType("web.use", []Type{middleware}, NoneType(), 0)
	router.Members["use_on"] = nativeFuncType("web.use_on", []Type{StrType(), StrType(), middleware}, NoneType(), 0)
	router.Members["use_request_id"] = nativeFuncType("web.use_request_id", nil, NoneType(), 0)
	router.Members["use_logging"] = nativeFuncType("web.use_logging", nil, NoneType(), 0)
	router.Members["use_errors"] = nativeFuncType("web.use_errors", nil, NoneType(), 0)
	return map[string]Type{
		"Router":   router,
		"Next":     next,
		"router":   nativeFuncType("web.router", nil, router, 0),
		"problem":  nativeFuncType("web.problem", []Type{httpTypes["Request"], IntType()}, httpTypes["Response"], 0),
		"reject":   nativeFuncType("web.reject", []Type{IntType()}, exceptionType(errorKind), 0),
		"validate": nativeFuncType("web.validate", []Type{httpTypes["Request"], validationTypes()["Schema"], AnyType()}, unionType(httpTypes["Response"], NoneType()), 0),
	}
}

func webDriverExports(types map[string]Type) map[string]Value {
	exports := driverTypeSymbols(types)
	exports["problem"] = driverFunc(types["problem"], func(vm *VM, args []Value) (Value, error) {
		status := int(args[1].Int)
		if _, ok := webStatuses[status]; !ok {
			return NoneValue(), errors.New("unsupported HTTP problem status")
		}
		return webProblemResponse(vm, status, problemRequestID(args[0]), nil, nil)
	})
	exports["reject"] = driverFunc(types["reject"], func(vm *VM, args []Value) (Value, error) {
		status := int(args[0].Int)
		if _, ok := webStatuses[status]; !ok {
			return NoneValue(), errors.New("unsupported HTTP rejection status")
		}
		rejection := &webRejection{status: status}
		return Value{Kind: ValueException, Exception: &Exception{kind: errorKind, message: rejection.Error(), cause: rejection}}, nil
	})
	exports["validate"] = driverFunc(types["validate"], func(vm *VM, args []Value) (Value, error) {
		schema, err := validationSchemaFrom(vm, args[1])
		if err != nil {
			return NoneValue(), err
		}
		check := validationRun{vm: vm}
		if err := check.value(schema, args[2], "", 0); err != nil {
			return NoneValue(), err
		}
		if len(check.issues) == 0 {
			return NoneValue(), nil
		}
		return webProblemResponse(vm, 422, problemRequestID(args[0]), check.issues, nil)
	})
	exports["router"] = driverFunc(types["router"], func(vm *VM, args []Value) (Value, error) {
		router := &webRouter{module: vm.module, routes: map[[2]string]*webRoute{}}
		typ := types["Router"]
		value := nativeObject(vm, typ, map[string]Value{
			"add": driverFunc(typ.Members["add"], func(vm *VM, args []Value) (Value, error) {
				handler := args[2].Function
				if handler.Module != nil && handler.Module.Engine != vm.engine {
					return NoneValue(), errors.New("route handler belongs to another Engine")
				}
				return NoneValue(), router.add(args[0].Str, args[1].Str, args[2])
			}),
			"dispatch": driverFunc(typ.Members["dispatch"], func(vm *VM, args []Value) (Value, error) {
				return router.dispatch(vm, args[0])
			}),
			"use": driverFunc(typ.Members["use"], func(vm *VM, args []Value) (Value, error) {
				return NoneValue(), router.appendStep(&router.steps, webStep{handler: args[0]})
			}),
			"use_on": driverFunc(typ.Members["use_on"], func(vm *VM, args []Value) (Value, error) {
				route := router.routes[[2]string{args[0].Str, args[1].Str}]
				if route == nil {
					return NoneValue(), errors.New("middleware requires an already registered route")
				}
				return NoneValue(), router.appendStep(&route.steps, webStep{handler: args[2]})
			}),
			"use_request_id": driverFunc(typ.Members["use_request_id"], func(vm *VM, args []Value) (Value, error) {
				return NoneValue(), router.appendStep(&router.steps, webStep{native: webRequestID})
			}),
			"use_logging": driverFunc(typ.Members["use_logging"], func(vm *VM, args []Value) (Value, error) {
				return NoneValue(), router.appendStep(&router.steps, webStep{native: webLogging})
			}),
			"use_errors": driverFunc(typ.Members["use_errors"], func(vm *VM, args []Value) (Value, error) {
				if router.frozen {
					return NoneValue(), errors.New("router was already started")
				}
				router.apiErrors = true
				return NoneValue(), nil
			}),
		})
		value.NativeObject.data = router
		return value, nil
	})
	return exports
}

func routerValue(value Value) (*webRouter, bool) {
	if value.Kind != ValueNativeObject || value.NativeObject == nil {
		return nil, false
	}
	router, ok := value.NativeObject.data.(*webRouter)
	return router, ok
}

func webSegments(path string) ([]string, error) {
	if path == "/" {
		return nil, nil
	}
	if len(path) == 0 || len(path) > maxWebPath || path[0] != '/' || strings.Count(path, "/") > maxWebSegments {
		return nil, errors.New("invalid or oversized route path")
	}
	return strings.Split(path[1:], "/"), nil
}

func safeWebSegment(segment string) bool {
	return segment != "" && segment != "." && segment != ".." && utf8.ValidString(segment) &&
		!strings.ContainsAny(segment, "/\\%") && strings.IndexFunc(segment, unicode.IsControl) < 0
}

func (r *webRouter) add(method, pattern string, handler Value) error {
	if r.frozen {
		return errors.New("router was already started")
	}
	if r.size >= maxWebRoutes {
		return errors.New("route limit exceeded")
	}
	if method == "" || len(method) > 16 || strings.IndexFunc(method, func(c rune) bool { return c < 'A' || c > 'Z' }) >= 0 {
		return errors.New("expected an uppercase HTTP method")
	}
	segments, err := webSegments(pattern)
	if err != nil {
		return err
	}
	names := make([]string, len(segments))
	seen := map[string]bool{}
	for i, segment := range segments {
		if strings.HasPrefix(segment, "{") && strings.HasSuffix(segment, "}") {
			name := segment[1 : len(segment)-1]
			if !validImportIdentifier(name) || seen[name] {
				return errors.New("invalid or duplicate route parameter name")
			}
			seen[name] = true
			names[i] = name
		} else if !safeWebSegment(segment) || strings.ContainsAny(segment, "{}") {
			return errors.New("invalid literal route segment")
		}
	}
	node := &r.root
	for i, segment := range segments {
		if names[i] != "" {
			if node.param == nil {
				node.param = &webNode{}
			}
			node = node.param
			continue
		}
		if node.literal == nil {
			node.literal = map[string]*webNode{}
		}
		if node.literal[segment] == nil {
			node.literal[segment] = &webNode{}
		}
		node = node.literal[segment]
	}
	if _, exists := node.methods[method]; exists {
		return fmt.Errorf("conflicting %s route", method)
	}
	if node.methods == nil {
		node.methods = map[string]*webRoute{}
	}
	route := &webRoute{handler: handler, names: names, pattern: pattern}
	node.methods[method] = route
	if r.routes == nil {
		r.routes = map[[2]string]*webRoute{}
	}
	r.routes[[2]string{method, pattern}] = route
	r.size++
	return nil
}

func (n *webNode) match(segments []string) *webNode {
	if n == nil {
		return nil
	}
	if len(segments) == 0 {
		if len(n.methods) > 0 {
			return n
		}
		return nil
	}
	if literal := n.literal[segments[0]].match(segments[1:]); literal != nil {
		return literal
	}
	return n.param.match(segments[1:])
}

func (n *webNode) allowedMethods() string {
	allowed := make([]string, 0, len(n.methods)+2)
	for method := range n.methods {
		allowed = append(allowed, method)
	}
	if _, ok := n.methods[http.MethodOptions]; !ok {
		allowed = append(allowed, http.MethodOptions)
	}
	if _, get := n.methods[http.MethodGet]; get {
		if _, head := n.methods[http.MethodHead]; !head {
			allowed = append(allowed, http.MethodHead)
		}
	}
	sort.Strings(allowed)
	return strings.Join(allowed, ", ")
}

func (r *webRouter) dispatch(vm *VM, request Value) (Value, error) {
	r.frozen = true
	state, ok := request.NativeObject.data.(*webRequest)
	if !ok {
		return NoneValue(), errors.New("invalid router request")
	}
	segments, err := webSegments(state.path)
	if err != nil {
		return r.problem(vm, state, 400, "invalid request path", nil)
	}
	for i, segment := range segments {
		decoded, err := url.PathUnescape(segment)
		if err != nil || !safeWebSegment(decoded) {
			return r.problem(vm, state, 400, "invalid request path", nil)
		}
		segments[i] = decoded
	}
	node := r.root.match(segments)
	if node == nil {
		return r.problem(vm, state, 404, "not found", nil)
	}
	method := request.NativeObject.Attrs["method"].Str
	route, found := node.methods[method]
	if !found && method == http.MethodHead {
		route, found = node.methods[http.MethodGet]
	}
	if !found {
		headers := http.Header{"Allow": []string{node.allowedMethods()}}
		if method == http.MethodOptions {
			return httpResponse(vm, 204, "", headers)
		}
		return r.problem(vm, state, 405, "method not allowed", headers)
	}
	old := state.params
	state.params = map[string]string{}
	defer func() { state.params = old }()
	for i, name := range route.names {
		if name != "" {
			state.params[name] = segments[i]
		}
	}
	pipeline := webPipeline{route: route, routerSteps: r.steps, request: request, state: state}
	result, err := pipeline.run(vm, 0)
	var stop *executionStop
	if err == nil || !r.apiErrors || errors.As(err, &stop) || vm.context().Err() != nil {
		return result, err
	}
	status := webErrorStatus(err)
	logWebFailure(state.id, route.pattern, status, err)
	return webProblemResponse(vm, status, state.id, nil, nil)
}

func (r *webRouter) problem(vm *VM, state *webRequest, status int, message string, headers http.Header) (Value, error) {
	if r.apiErrors {
		return webProblemResponse(vm, status, state.id, nil, headers)
	}
	return httpResponse(vm, int64(status), message, headers)
}

func callHTTPHandler(vm *VM, handler, request Value) (Value, error) {
	if router, ok := routerValue(handler); ok {
		if err := vm.checkNativeOwner(handler.NativeObject); err != nil {
			return NoneValue(), err
		}
		return router.dispatch(vm, request)
	}
	return vm.callValue(handler, []Value{request}, nil)
}
