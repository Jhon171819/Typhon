package typhon

import (
	"crypto/rand"
	"errors"
	"log"
	"net/http"
)

type webStatus struct{ code, message string }

var webStatuses = map[int]webStatus{
	400: {"bad_request", "The request could not be parsed."},
	401: {"unauthorized", "Authentication is required."},
	403: {"forbidden", "The request is not permitted."},
	404: {"not_found", "The resource was not found."},
	405: {"method_not_allowed", "This method is not allowed."},
	408: {"request_body_timeout", "The request body timed out."},
	409: {"conflict", "The request conflicts with the current state."},
	413: {"payload_too_large", "The request body is too large."},
	422: {"validation_error", "The request violates its contract."},
	429: {"too_many_requests", "Too many requests."},
	499: {"client_closed_request", "The client cancelled the request."},
	500: {"internal_error", "An internal error occurred."},
	503: {"service_unavailable", "The service is temporarily unavailable."},
	504: {"timeout", "The request timed out."},
}

type webRejection struct{ status int }

func (r *webRejection) Error() string { return webStatuses[r.status].message }

func webProblemPayload(status int, id string, details []Value) Value {
	p := webStatuses[status]
	body := DictValue()
	for _, entry := range []DictEntry{
		{Key: StringValue("code"), Value: StringValue(p.code)},
		{Key: StringValue("message"), Value: StringValue(p.message)},
		{Key: StringValue("details"), Value: ListValue(details)},
		{Key: StringValue("request_id"), Value: StringValue(id)},
	} {
		_ = body.Dict.put(entry.Key, entry.Value)
	}
	return body
}

func webProblemResponse(vm *VM, status int, id string, details []Value, headers http.Header) (Value, error) {
	body, err := dumpJSON(vm, webProblemPayload(status, id, details))
	if err != nil {
		return NoneValue(), err
	}
	if headers == nil {
		headers = make(http.Header)
	}
	headers.Set("Content-Type", "application/problem+json")
	headers.Set("X-Request-ID", id)
	return httpResponse(vm, int64(status), body, headers)
}

func webErrorStatus(err error) int {
	var reject *webRejection
	if errors.As(err, &reject) {
		return reject.status
	}
	var syntax *JSONError
	if errors.As(err, &syntax) {
		if syntax.Offset > 0 {
			return 400
		}
		return 500
	}
	var ex *Exception
	if errors.As(err, &ex) {
		switch ex.kind {
		case validationKind:
			return 422
		case timeoutKind:
			return 504
		case connectionKind:
			return 503
		case databaseKind:
			var cause *NativeCause
			if errors.As(err, &cause) && cause.Driver() == "PostgreSQL" {
				if cause.Code() == "23505" {
					return 409
				}
				if len(cause.Code()) == 5 && cause.Code()[:2] == "08" {
					return 503
				}
			}
		}
	}
	return 500
}

func logWebFailure(id, pattern string, status int, err error) {
	kind, source, code := "", "", ""
	var ex *Exception
	if errors.As(err, &ex) {
		kind = ex.kind
	}
	var cause *NativeCause
	if errors.As(err, &cause) {
		source, code = cause.Driver(), cause.Code()
	}
	log.Printf("Typhon web error id=%s route=%q status=%d exception=%q driver=%q sqlstate=%q", id, pattern, status, kind, source, code)
}

type problemWriter struct {
	enabled bool
	id      string
	head    bool
}

func (p problemWriter) write(w http.ResponseWriter, status int, legacy string) {
	if !p.enabled {
		http.Error(w, legacy, status)
		return
	}
	if p.id == "" {
		p.id = rand.Text()
	}
	body, err := dumpJSON(nil, webProblemPayload(status, p.id, nil))
	if err != nil {
		http.Error(w, "internal server error", 500)
		return
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("X-Request-ID", p.id)
	w.WriteHeader(status)
	if !p.head {
		_, _ = w.Write([]byte(body))
	}
}

func problemRequestID(request Value) string {
	return request.NativeObject.Attrs["request_id"].Str
}
