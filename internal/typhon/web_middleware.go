package typhon

import (
	"crypto/rand"
	"errors"
	"log"
	"net/http"
	"time"
	"unicode/utf8"
)

const maxWebMiddleware = 16

type verifiedIdentityKey struct{}

// Only verified native code or the host populates this metadata.
type verifiedRequestIdentity struct {
	subject string
	tenant  string
	roles   []string
	scopes  map[string]bool
	expires time.Time
}

func newWebRequest(req *http.Request) *webRequest {
	principal, _ := req.Context().Value(verifiedIdentityKey{}).(verifiedRequestIdentity)
	if principal.subject == "" || len(principal.subject) > 256 || len(principal.tenant) > 256 ||
		!utf8.ValidString(principal.subject) || !utf8.ValidString(principal.tenant) {
		principal = verifiedRequestIdentity{}
	}
	state := &webRequest{path: req.URL.EscapedPath(), rawQuery: req.URL.RawQuery, id: rand.Text(), principal: principal,
		bearer: bearerToken(req.Header.Values("Authorization")), contentType: req.Header.Get("Content-Type")}
	if keys := req.Header.Values("Idempotency-Key"); len(keys) == 1 {
		state.idempotencyKey = keys[0]
	}
	return state
}

func webNextType() Type {
	response := httpDriverTypes()["Response"]
	return Type{Kind: TypeNative, Name: "typhon.web.Next", Members: map[string]Type{
		"call": nativeFuncType("web.next", nil, response, 0),
	}}
}

type webNativeStep uint8

const (
	webUserStep webNativeStep = iota
	webRequestID
	webLogging
)

type webStep struct {
	handler Value
	native  webNativeStep
}

func (r *webRouter) appendStep(steps *[]webStep, step webStep) error {
	if r.frozen {
		return errors.New("router was already started")
	}
	if len(*steps) >= maxWebMiddleware {
		return errors.New("middleware limit exceeded")
	}
	*steps = append(*steps, step)
	return nil
}

type webPipeline struct {
	route       *webRoute
	routerSteps []webStep
	request     Value
	state       *webRequest
}

func (p webPipeline) run(vm *VM, index int) (Value, error) {
	if err := vm.executionError(); err != nil {
		return NoneValue(), err
	}
	var step webStep
	switch {
	case index < len(p.routerSteps):
		step = p.routerSteps[index]
	case index-len(p.routerSteps) < len(p.route.steps):
		step = p.route.steps[index-len(p.routerSteps)]
	default:
		if p.route.idempotency != nil {
			return p.route.idempotency.call(vm, p)
		}
		return vm.callValue(p.route.handler, []Value{p.request}, nil)
	}
	switch step.native {
	case webRequestID:
		p.state.sendID = true
		return p.run(vm, index+1)
	case webLogging:
		start := time.Now()
		response, err := p.run(vm, index+1)
		method := p.request.NativeObject.Attrs["method"].Str
		if err != nil {
			log.Printf("Typhon HTTP id=%s method=%q route=%q outcome=error duration=%s", p.state.id, method, p.route.pattern, time.Since(start))
		} else {
			log.Printf("Typhon HTTP id=%s method=%q route=%q status=%d duration=%s", p.state.id, method, p.route.pattern, response.NativeObject.Attrs["status"].Int, time.Since(start))
		}
		return response, err
	}
	if err := vm.chargeBytes(valueSlotBytes); err != nil {
		return NoneValue(), err
	}
	nextType := webNextType()
	continuation := func(vm *VM) (Value, error) { return p.run(vm, index+1) }
	next := scopedNativeObject(vm, nextType, map[string]Value{
		"call": driverFunc(nextType.Members["call"], func(vm *VM, args []Value) (Value, error) {
			if continuation == nil {
				return NoneValue(), errors.New("middleware next has already been called or has expired")
			}
			run := continuation
			continuation = nil
			return run(vm)
		}),
	})
	defer func() { continuation = nil }()
	return vm.callValue(step.handler, []Value{p.request, next}, nil)
}

func optionalWebString(value string, exists bool) Value {
	if !exists {
		return NoneValue()
	}
	return StringValue(value)
}

func (state *webRequest) setState(vm *VM, key, value string) error {
	if !validImportIdentifier(key) || len(key) > 64 || len(value) > 1024 || !utf8.ValidString(value) {
		return errors.New("invalid request state key or value")
	}
	if _, exists := state.state[key]; !exists && len(state.state) >= 64 {
		return errors.New("request state key limit exceeded")
	}
	if err := vm.chargeBytes(valueSlotBytes + int64(len(key)+len(value))); err != nil {
		return err
	}
	if state.state == nil {
		state.state = map[string]string{}
	}
	state.state[key] = value
	return nil
}
