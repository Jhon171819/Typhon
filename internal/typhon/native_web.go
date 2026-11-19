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
	auth    webAuthenticator
	policy  *webPolicy
	action  string
	rate    *webRouteRate
	doc     *webRouteDoc
	upload  *httpUploadPolicy
	sse     *httpSSEPolicy
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
	browser   webBrowserPolicy
	docs      webDocsConfig
	hasSSE    bool
}

type webRequest struct {
	path        string
	rawQuery    string
	params      map[string]string
	id          string
	sendID      bool
	state       map[string]string
	principal   verifiedRequestIdentity
	bearer      string
	rateHeaders http.Header
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
	router.Members["require_auth"] = nativeFuncType("web.require_auth", []Type{StrType(), StrType(), authDriverTypes()["Authenticator"]}, NoneType(), 0)
	router.Members["authorize"] = nativeFuncType("web.authorize", []Type{StrType(), StrType(), authorizationPolicyType(), StrType()}, NoneType(), 0)
	strings := listType(StrType())
	router.Members["use_cors"] = nativeFuncType("web.use_cors", []Type{strings, strings, strings, BoolType()}, NoneType(), 1)
	router.Members["use_security_headers"] = nativeFuncType("web.use_security_headers", []Type{StrType(), StrType(), IntType()}, NoneType(), 3)
	router.Members["rate_limit"] = nativeFuncType("web.rate_limit", []Type{StrType(), StrType(), webRateLimiterType()}, NoneType(), 0)
	schema := unionType(validationTypes()["Schema"], NoneType())
	router.Members["document"] = nativeFuncType("web.document",
		[]Type{StrType(), StrType(), StrType(), strings, dictType(IntType(), schema), schema, schema}, NoneType(), 2)
	router.Members["configure_docs"] = nativeFuncType("web.configure_docs", []Type{StrType(), StrType(), BoolType()}, NoneType(), 1)
	router.Members["stream_upload"] = nativeFuncType("web.stream_upload", []Type{StrType(), StrType(), IntType(), IntType(), IntType()}, NoneType(), 2)
	router.Members["enable_sse"] = nativeFuncType("web.enable_sse", []Type{StrType(), IntType(), IntType()}, NoneType(), 2)
	types := map[string]Type{
		"Router":         router,
		"Next":           next,
		"router":         nativeFuncType("web.router", nil, router, 0),
		"problem":        nativeFuncType("web.problem", []Type{httpTypes["Request"], IntType()}, httpTypes["Response"], 0),
		"reject":         nativeFuncType("web.reject", []Type{IntType()}, exceptionType(errorKind), 0),
		"validate":       nativeFuncType("web.validate", []Type{httpTypes["Request"], validationTypes()["Schema"], AnyType()}, unionType(httpTypes["Response"], NoneType()), 0),
		"session_cookie": nativeFuncType("web.session_cookie", []Type{StrType(), StrType(), StrType(), IntType()}, StrType(), 2),
		"RateLimiter":    webRateLimiterType(),
		"rate_limiter": nativeFuncType("web.rate_limiter",
			[]Type{redisDriverTypes()["Connection"], StrType(), IntType(), IntType(), BoolType(), dictType(StrType(), IntType())}, webRateLimiterType(), 2),
	}
	webPaginationTypes(types)
	return types
}

func webDriverExports(types map[string]Type) map[string]Value {
	exports := driverTypeSymbols(types)
	exports["paginator"] = makeWebPaginator(types)
	exports["rate_limiter"] = driverFunc(types["rate_limiter"], makeWebRateLimiter)
	exports["session_cookie"] = driverFunc(types["session_cookie"], func(vm *VM, args []Value) (Value, error) {
		return webSessionCookie(args)
	})
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
		return Value{Kind: ValueException, Exception: rejection.exception()}, nil
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
			"require_auth": driverFunc(typ.Members["require_auth"], func(vm *VM, args []Value) (Value, error) {
				if router.frozen {
					return NoneValue(), errors.New("router was already started")
				}
				route := router.routes[[2]string{args[0].Str, args[1].Str}]
				if route == nil || route.auth != nil {
					return NoneValue(), errors.New("authentication requires a registered route without an authenticator")
				}
				if err := vm.checkNativeOwner(args[2].NativeObject); err != nil {
					return NoneValue(), err
				}
				auth, ok := args[2].NativeObject.data.(webAuthenticator)
				if !ok {
					return NoneValue(), errors.New("invalid route authenticator")
				}
				route.auth = auth
				return NoneValue(), nil
			}),
			"authorize": driverFunc(typ.Members["authorize"], func(vm *VM, args []Value) (Value, error) {
				if router.frozen {
					return NoneValue(), errors.New("router was already started")
				}
				route := router.routes[[2]string{args[0].Str, args[1].Str}]
				if route == nil || route.auth == nil || route.policy != nil {
					return NoneValue(), errors.New("authorization requires a registered authenticated route without a policy")
				}
				if err := vm.checkNativeOwner(args[2].NativeObject); err != nil {
					return NoneValue(), err
				}
				policy, ok := args[2].NativeObject.data.(*webPolicy)
				if !ok || !policy.actions[args[3].Str] {
					return NoneValue(), errors.New("route action must be defined in the authorization policy")
				}
				route.policy, route.action = policy, args[3].Str
				return NoneValue(), nil
			}),
			"use_cors": driverFunc(typ.Members["use_cors"], func(vm *VM, args []Value) (Value, error) {
				if router.frozen || router.browser.cors != nil {
					return NoneValue(), errors.New("configure CORS once before starting the router")
				}
				policy, err := newWebCORS(args)
				router.browser.cors = policy
				return NoneValue(), err
			}),
			"use_security_headers": driverFunc(typ.Members["use_security_headers"], func(vm *VM, args []Value) (Value, error) {
				if router.frozen || router.browser.security != nil {
					return NoneValue(), errors.New("configure security headers once before starting the router")
				}
				policy, err := newWebSecurityHeaders(args)
				router.browser.security = policy
				return NoneValue(), err
			}),
			"rate_limit": driverFunc(typ.Members["rate_limit"], func(vm *VM, args []Value) (Value, error) {
				if router.frozen {
					return NoneValue(), errors.New("router was already started")
				}
				route := router.routes[[2]string{args[0].Str, args[1].Str}]
				if route == nil || route.auth == nil || route.rate != nil {
					return NoneValue(), errors.New("rate limiting requires a registered authenticated route without a limiter")
				}
				limiter, ok := args[2].NativeObject.data.(*webRateLimiter)
				if !ok {
					return NoneValue(), errors.New("invalid route rate limiter")
				}
				route.rate = limiter.bind(args[0].Str, args[1].Str)
				return NoneValue(), nil
			}),
			"document": driverFunc(typ.Members["document"], func(vm *VM, args []Value) (Value, error) {
				return NoneValue(), router.document(vm, args)
			}),
			"configure_docs": driverFunc(typ.Members["configure_docs"], func(vm *VM, args []Value) (Value, error) {
				if router.frozen || !validAuthText(args[0].Str, 256) || !validAuthText(args[1].Str, 80) {
					return NoneValue(), errors.New("configure OpenAPI title and version before starting the router")
				}
				router.docs = webDocsConfig{title: args[0].Str, version: args[1].Str, serve: len(args) == 3 && args[2].Bool}
				return NoneValue(), nil
			}),
			"stream_upload": driverFunc(typ.Members["stream_upload"], func(vm *VM, args []Value) (Value, error) {
				if router.frozen {
					return NoneValue(), errors.New("router was already started")
				}
				route := router.routes[[2]string{args[0].Str, args[1].Str}]
				if route == nil || route.upload != nil || route.sse != nil || args[0].Str == "HEAD" {
					return NoneValue(), errors.New("stream upload requires a registered non-HEAD route without an upload policy")
				}
				total, part, parts := args[2].Int, args[2].Int, int64(16)
				if len(args) > 3 {
					part = args[3].Int
				}
				if len(args) > 4 {
					parts = args[4].Int
				}
				if total < 1 || total > maxHTTPStreamBytes || part < 1 || part > total || parts < 1 || parts > 100 {
					return NoneValue(), errors.New("invalid HTTP streaming upload limits")
				}
				route.upload = &httpUploadPolicy{total: total, part: part, parts: int(parts)}
				return NoneValue(), nil
			}),
			"enable_sse": driverFunc(typ.Members["enable_sse"], func(vm *VM, args []Value) (Value, error) {
				if router.frozen {
					return NoneValue(), errors.New("router was already started")
				}
				if err := configureSSE(router.routes[[2]string{http.MethodGet, args[0].Str}], args); err != nil {
					return NoneValue(), err
				}
				router.hasSSE = true
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

func webRequestSegments(path string) ([]string, error) {
	segments, err := webSegments(path)
	if err != nil {
		return nil, err
	}
	for i, segment := range segments {
		decoded, err := url.PathUnescape(segment)
		if err != nil || !safeWebSegment(decoded) {
			return nil, errors.New("invalid request path")
		}
		segments[i] = decoded
	}
	return segments, nil
}

func (r *webRouter) requestRoute(path, method string) *webRoute {
	segments, err := webRequestSegments(path)
	if err == nil {
		if node := r.root.match(segments); node != nil {
			return node.methods[method]
		}
	}
	return nil
}

func (r *webRouter) dispatch(vm *VM, request Value) (Value, error) {
	r.frozen = true
	state, ok := request.NativeObject.data.(*webRequest)
	if !ok {
		return NoneValue(), errors.New("invalid router request")
	}
	segments, err := webRequestSegments(state.path)
	if err != nil {
		return r.problem(vm, state, 400, "invalid request path", nil)
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
	if route.auth != nil {
		principal, err := route.auth.verify(vm.context(), state.bearer)
		if stop := vm.executionError(); stop != nil {
			return NoneValue(), stop
		}
		conflictingIdentity := state.principal.subject != "" &&
			(state.principal.subject != principal.subject || state.principal.tenant != principal.tenant)
		if errors.Is(err, errInvalidBearer) || err == nil && conflictingIdentity {
			headers := http.Header{"Www-Authenticate": []string{"Bearer"}, "Cache-Control": []string{"no-store"}}
			return r.problem(vm, state, 401, "authentication is required", headers)
		}
		if err != nil {
			logWebFailure(state.id, route.pattern, 503, err)
			return r.problem(vm, state, 503, "authentication temporarily unavailable", nil)
		}
		state.principal = principal
		request.NativeObject.Attrs["identity"] = StringValue(principal.subject)
		request.NativeObject.Attrs["tenant"] = optionalWebString(principal.tenant, principal.tenant != "")
		if route.policy != nil {
			allowed := route.policy.allows(principal, route.action)
			logWebAuthorization(state.id, route.pattern, route.action, allowed)
			if !allowed {
				return r.problem(vm, state, 403, "permission denied", http.Header{"Cache-Control": {"no-store"}})
			}
		}
		if route.rate != nil {
			if principal.tenant == "" {
				return r.problem(vm, state, 403, "rate limiting requires a verified tenant", nil)
			}
			headers, allowed, err := route.rate.take(vm, principal)
			if stop := vm.executionError(); stop != nil {
				return NoneValue(), stop
			}
			stats := vm.engine.httpMetrics.Load()
			switch {
			case err != nil:
				if stats != nil {
					stats.rateErrors.Add(1)
				}
				logWebFailure(state.id, route.pattern, 503, err)
				if !route.rate.limiter.failOpen {
					return r.problem(vm, state, 503, "rate limit temporarily unavailable", nil)
				}
			case !allowed:
				if stats != nil {
					stats.rateDenied.Add(1)
				}
				return r.problem(vm, state, 429, "too many requests", headers)
			default:
				if stats != nil {
					stats.rateAllowed.Add(1)
				}
				state.rateHeaders = headers
			}
		}
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
