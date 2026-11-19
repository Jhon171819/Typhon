package typhon

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

const maxSSEConnections = 128
const maxSSEDataBytes = 16 << 10
const ssePollInterval = 250 * time.Millisecond
const sseWriteTimeout = 2 * time.Second

type httpSSEPolicy struct {
	duration, heartbeat time.Duration
}

type httpSSEEvent struct {
	id, kind, data string
}

type httpSSEResponse struct {
	producer, state Value
}

func httpSSEType() Type { return Type{Kind: TypeNative, Name: "typhon.http.Event"} }

func httpSSETypes(types map[string]Type) {
	eventType := httpSSEType()
	producer := nativeFuncType("http.sse.producer", []Type{StrType(), AnyType()}, unionType(eventType, NoneType()), 0)
	types["Event"] = eventType
	types["event"] = nativeFuncType("http.event", []Type{StrType(), StrType(), StrType()}, eventType, 0)
	types["sse"] = nativeFuncType("http.sse", []Type{producer, AnyType()}, types["Response"], 0)
}

func makeSSEEvent(vm *VM, args []Value) (Value, error) {
	id, kind, data := args[0].Str, args[1].Str, args[2].Str
	if id != "" && !validAuthText(id, 256) || kind != "" && !validPermissionName(kind) ||
		len(data) > maxSSEDataBytes || !utf8.ValidString(data) {
		return NoneValue(), errors.New("invalid or oversized server-sent event")
	}
	value := nativeObject(vm, httpSSEType(), nil)
	value.NativeObject.data = &httpSSEEvent{id: id, kind: kind, data: data}
	return value, nil
}

func makeSSEResponse(vm *VM, args []Value) (Value, error) {
	if vm.request == nil {
		return NoneValue(), errors.New("create an SSE response in an HTTP handler")
	}
	result, err := httpResponse(vm, http.StatusOK, "", sseResponseHeaders())
	if err != nil {
		return NoneValue(), err
	}
	result.NativeObject.request = vm.request
	result.NativeObject.data = &httpSSEResponse{producer: args[0], state: args[1]}
	return result, nil
}

func sseResponseHeaders() http.Header {
	return http.Header{
		"Content-Type":      {"text/event-stream; charset=utf-8"},
		"Cache-Control":     {"no-cache, no-transform"},
		"X-Accel-Buffering": {"no"},
	}
}

func configureSSE(route *webRoute, args []Value) error {
	if route == nil || route.sse != nil || route.upload != nil {
		return errors.New("SSE requires a registered GET route without a streaming policy")
	}
	duration, heartbeat := int64(300), int64(15)
	if len(args) > 1 {
		duration = args[1].Int
	}
	if len(args) > 2 {
		heartbeat = args[2].Int
	}
	if duration < 1 || duration > 1800 || heartbeat < 1 || heartbeat > 60 {
		return errors.New("SSE duration must be 1–1800 seconds and heartbeat must be 1–60 seconds")
	}
	route.sse = &httpSSEPolicy{duration: time.Duration(duration) * time.Second, heartbeat: time.Duration(heartbeat) * time.Second}
	return nil
}

func lastSSEEvent(req *http.Request) (string, bool) {
	values := req.Header.Values("Last-Event-ID")
	if len(values) == 0 {
		return "", true
	}
	if len(values) != 1 || values[0] != "" && !validAuthText(values[0], 256) {
		return "", false
	}
	return values[0], true
}

func (e *httpSSEEvent) text() string {
	var result strings.Builder
	if e.id != "" {
		result.WriteString("id: " + e.id + "\n")
	}
	if e.kind != "" {
		result.WriteString("event: " + e.kind + "\n")
	}
	normalized := strings.ReplaceAll(strings.ReplaceAll(e.data, "\r\n", "\n"), "\r", "\n")
	for line := range strings.SplitSeq(normalized, "\n") {
		result.WriteString("data: " + line + "\n")
	}
	result.WriteByte('\n')
	return result.String()
}

func writeSSEResponse(vm *VM, writer http.ResponseWriter, state *webRequest, lastID string, policy *httpSSEPolicy, stream *httpSSEResponse, callbackTimeout time.Duration) (bool, error) {
	ctx := vm.context()
	if err := vm.executionError(); err != nil {
		return false, err
	}
	controller := http.NewResponseController(writer)
	write := func(block string) error {
		deadline := time.Now().Add(sseWriteTimeout)
		if end, ok := ctx.Deadline(); ok && end.Before(deadline) {
			deadline = end
		}
		if err := controller.SetWriteDeadline(deadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return driverFailure("HTTP", connectionKind, err)
		}
		stop := armSocketDeadline(ctx, controller.SetWriteDeadline)
		defer stop()
		if _, err := io.WriteString(writer, block); err != nil {
			return driverFailure("HTTP", connectionKind, err)
		}
		if err := controller.Flush(); err != nil {
			return driverFailure("HTTP", connectionKind, err)
		}
		// The standard server timeout must not expire between heartbeats.
		_ = controller.SetWriteDeadline(time.Time{})
		return nil
	}
	setHTTPResponseHeaders(writer, state, sseResponseHeaders())
	if state.rateHeaders != nil {
		writer.Header().Set("Cache-Control", "no-store, no-transform")
	}
	writer.WriteHeader(http.StatusOK)
	if err := write(": connected\n\n"); err != nil {
		return true, err
	}
	keepalive := time.NewTicker(policy.heartbeat)
	defer keepalive.Stop()
	poll := time.NewTicker(ssePollInterval)
	defer poll.Stop()
	for {
		event, err := func() (*httpSSEEvent, error) {
			vm.engine.execMu.Lock()
			defer vm.engine.execMu.Unlock()
			if err := vm.executionError(); err != nil {
				return nil, err
			}
			callCtx, cancel := context.WithTimeout(ctx, callbackTimeout)
			vm.ctx = callCtx
			defer func() {
				cancel()
				vm.ctx = ctx
			}()
			value, err := vm.callValue(stream.producer, []Value{StringValue(lastID), stream.state}, nil)
			if err != nil || value.Kind == ValueNone {
				return nil, err
			}
			if value.Kind != ValueNativeObject || value.NativeObject == nil {
				return nil, errors.New("SSE producer must return Event or None")
			}
			if err := vm.checkNativeOwner(value.NativeObject); err != nil {
				return nil, err
			}
			result, ok := value.NativeObject.data.(*httpSSEEvent)
			if !ok {
				return nil, errors.New("SSE producer must return Event or None")
			}
			return result, nil
		}()
		if ctx.Err() != nil {
			return true, nil
		}
		if err != nil {
			return true, err
		}
		if event != nil && (event.id == "" || event.id != lastID) {
			block := event.text()
			vm.engine.execMu.Lock()
			err := vm.chargeBytes(int64(len(block)))
			vm.engine.execMu.Unlock()
			if err != nil {
				return true, err
			}
			if err := write(block); err != nil {
				if ctx.Err() != nil {
					return true, nil
				}
				return true, err
			}
			if event.id != "" {
				lastID = event.id
			}
		}
		select {
		case <-ctx.Done():
			return true, nil
		case <-poll.C:
		case <-keepalive.C:
			if err := write(": keep-alive\n\n"); err != nil && ctx.Err() == nil {
				return true, err
			}
		}
	}
}

func newHTTPSSEContext(req *http.Request, vm *VM, principal verifiedRequestIdentity, policy *httpSSEPolicy) (context.Context, context.CancelFunc) {
	duration := policy.duration
	if !principal.expires.IsZero() {
		duration = min(duration, time.Until(principal.expires))
	}
	return httpRequestContext(req.Context(), vm.engine.ctx, duration)
}
