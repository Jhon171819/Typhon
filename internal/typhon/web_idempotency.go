package typhon

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

const (
	maxIdempotencyRequest = 256 << 10
	maxIdempotencyBody    = 64 << 10
	maxIdempotencyRecord  = 96 << 10
)

const redisIdempotencyBegin = `local value = redis.call('GET', KEYS[1])
if value then
  if string.len(value) > tonumber(ARGV[3]) then
    return redis.error_reply('ERR idempotency record exceeds the limit')
  end
  return value
end
redis.call('PSETEX', KEYS[1], ARGV[2], ARGV[1])
return ''`

const redisIdempotencyFinish = `if redis.call('GET', KEYS[1]) ~= ARGV[1] then
  return 0
end
redis.call('PSETEX', KEYS[1], ARGV[3], ARGV[2])
return 1`

type webIdempotency struct {
	db        *redisConnection
	namespace string
	ttl       time.Duration
}

type idempotencyResponse struct {
	Status      int64
	Body        []byte
	ContentType string
	Location    string
}

func (r *webRouter) configureIdempotency(args []Value) error {
	if r.frozen {
		return errors.New("router was already started")
	}
	method, pattern := args[0].Str, args[1].Str
	route := r.routes[[2]string{method, pattern}]
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		return errors.New("idempotency requires a POST, PUT, PATCH or DELETE route")
	}
	if route == nil || route.policy == nil || route.idempotency != nil || route.upload != nil {
		return errors.New("idempotency requires a registered authorized route with a buffered body")
	}
	db, ok := args[2].NativeObject.data.(*redisConnection)
	namespace := args[3].Str
	seconds := int64(86400)
	if len(args) > 4 {
		seconds = args[4].Int
	}
	if !ok || args[2].NativeObject.request != nil || !validAuthText(namespace, 64) || seconds < 60 || seconds > 604800 {
		return errors.New("idempotency requires an Engine Redis connection, a namespace of 1 to 64 bytes, and TTL between 60 seconds and 7 days")
	}
	route.idempotency = &webIdempotency{db: db, namespace: namespace, ttl: time.Duration(seconds) * time.Second}
	return nil
}

func idempotencyDigest(parts ...string) string {
	hash := sha256.New()
	var length [8]byte
	for _, part := range parts {
		binary.BigEndian.PutUint64(length[:], uint64(len(part)))
		_, _ = hash.Write(length[:])
		_, _ = io.WriteString(hash, part)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func validIdempotencyKey(key string) bool {
	if len(key) == 0 || len(key) > 128 {
		return false
	}
	for _, ch := range key {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || strings.ContainsRune("-._~", ch)) {
			return false
		}
	}
	return true
}

func (p *webIdempotency) call(vm *VM, pipeline webPipeline) (Value, error) {
	state, err := authorizedTenantRequest(vm, pipeline.request)
	if err != nil {
		return NoneValue(), err
	}
	if expires := state.principal.expires; !expires.IsZero() && !time.Now().Before(expires) {
		return NoneValue(), (&webRejection{status: 403}).exception()
	}
	if !validIdempotencyKey(state.idempotencyKey) {
		return webProblemResponse(vm, 400, state.id, nil, nil)
	}
	attrs := pipeline.request.NativeObject.Attrs
	body := attrs["body"].Str
	if len(body) > maxIdempotencyRequest {
		return webProblemResponse(vm, 413, state.id, nil, nil)
	}
	key := "typhon:idempotency:" + idempotencyDigest(p.namespace, state.principal.tenant, state.principal.subject, state.idempotencyKey)
	fingerprint := idempotencyDigest(attrs["method"].Str, pipeline.route.pattern, state.path, state.rawQuery,
		state.principal.tenant, state.contentType, body)
	pending := "p:" + fingerprint + ":" + rand.Text()
	var previous string
	err = p.db.run(vm.context(), p.db.timeout, func(ctx context.Context) error {
		var err error
		previous, err = p.db.client.Eval(ctx, redisIdempotencyBegin, []string{key}, pending, p.ttl.Milliseconds(), maxIdempotencyRecord).Text()
		return err
	})
	if err != nil {
		logWebFailure(state.id, pipeline.route.pattern, 503, err)
		return webProblemResponse(vm, 503, state.id, nil, nil)
	}
	if previous != "" {
		return replayIdempotency(vm, state.id, fingerprint, previous)
	}

	oldFingerprint := attrs["idempotency_fingerprint"]
	attrs["idempotency_fingerprint"] = StringValue(fingerprint)
	response, err := vm.callValue(pipeline.route.handler, []Value{pipeline.request}, nil)
	attrs["idempotency_fingerprint"] = oldFingerprint
	if err != nil {
		// The operation might have committed. Preserve the reservation.
		return NoneValue(), err
	}
	snapshot, completed := snapshotIdempotency(response)
	if !completed {
		return response, nil
	}
	record := "c:" + fingerprint + ":" + snapshot
	var saved int64
	err = p.db.run(vm.context(), p.db.timeout, func(ctx context.Context) error {
		var err error
		saved, err = p.db.client.Eval(ctx, redisIdempotencyFinish, []string{key}, pending, record, p.ttl.Milliseconds()).Int64()
		return err
	})
	if err != nil || saved != 1 {
		log.Printf("Typhon idempotency id=%s route=%q outcome=completion_unconfirmed", state.id, pipeline.route.pattern)
		if stop := vm.executionError(); stop != nil {
			return NoneValue(), stop
		}
		return webProblemResponse(vm, 503, state.id, nil, nil)
	}
	return response, nil
}

func replayIdempotency(vm *VM, requestID, fingerprint, record string) (Value, error) {
	state, rest, ok := strings.Cut(record, ":")
	got, snapshot, valid := strings.Cut(rest, ":")
	if !ok || !valid || len(got) != 64 || (state != "p" && state != "c") {
		return webProblemResponse(vm, 503, requestID, nil, nil)
	}
	if got != fingerprint {
		return webProblemResponse(vm, 409, requestID, nil, http.Header{"Idempotency-Status": {"conflict"}})
	}
	if state == "p" {
		return webProblemResponse(vm, 409, requestID, nil, http.Header{
			"Idempotency-Status": {"in-progress"}, "Retry-After": {"1"},
		})
	}
	if snapshot == "" {
		return webProblemResponse(vm, 409, requestID, nil, http.Header{"Idempotency-Status": {"unreplayable"}})
	}
	if err := vm.chargeBytes(int64(len(snapshot))); err != nil {
		return NoneValue(), err
	}
	var response idempotencyResponse
	if err := json.Unmarshal([]byte(snapshot), &response); err != nil || !response.valid() {
		return webProblemResponse(vm, 503, requestID, nil, nil)
	}
	headers := http.Header{"Idempotency-Replayed": {"true"}}
	if response.ContentType != "" {
		headers.Set("Content-Type", response.ContentType)
	}
	if response.Location != "" {
		headers.Set("Location", response.Location)
	}
	return httpResponse(vm, response.Status, string(response.Body), headers)
}

func (r idempotencyResponse) valid() bool {
	return r.Status >= 200 && r.Status <= 299 && len(r.Body) <= maxIdempotencyBody &&
		len(r.ContentType) <= 256 && len(r.Location) <= 1024 &&
		validHTTPHeaderValue(r.ContentType) && validHTTPHeaderValue(r.Location)
}

func snapshotIdempotency(value Value) (string, bool) {
	if value.Kind != ValueNativeObject || value.NativeObject.Type.Name != "typhon.http.Response" {
		return "", false
	}
	object := value.NativeObject
	headers, ok := object.data.(http.Header)
	status, body := object.Attrs["status"].Int, object.Attrs["body"].Str
	if !ok || status >= 500 || status < 200 {
		return "", false
	}
	if status > 299 || len(body) > maxIdempotencyBody {
		return "", true
	}
	for key, values := range headers {
		if (key != "Content-Type" && key != "Location") || len(values) != 1 {
			return "", true
		}
	}
	response := idempotencyResponse{
		Status: status, Body: []byte(body), ContentType: headers.Get("Content-Type"), Location: headers.Get("Location"),
	}
	if !response.valid() {
		return "", true
	}
	raw, err := json.Marshal(response)
	if err != nil || len(raw) > maxIdempotencyRecord-80 {
		return "", true
	}
	return string(raw), true
}
