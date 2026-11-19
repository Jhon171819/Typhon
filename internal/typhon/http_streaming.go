package typhon

import (
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"strings"
	"time"
)

const maxHTTPStreamBytes = 128 << 20
const maxHTTPStreamChunk = 64 << 10

type httpUploadPolicy struct {
	total, part int64
	parts       int
}

type httpInboundStream struct {
	ctx         context.Context
	body        io.ReadCloser
	transport   io.Reader
	contentType string
	controller  *http.ResponseController
	policy      httpUploadPolicy
	mode        string
	closed      bool
	ended       bool
}

func httpStreamTypes() (body, form, part Type) {
	bytes := unionType(scalarType("Bytes"), NoneType())
	read := nativeFuncType("http.stream.read", []Type{IntType()}, bytes, 0)
	close := nativeFuncType("http.stream.close", nil, NoneType(), 0)
	body = Type{Kind: TypeNative, Name: "typhon.http.Body", Members: map[string]Type{"read": read, "close": close}}
	part = Type{Kind: TypeNative, Name: "typhon.http.Part", Members: map[string]Type{
		"name": StrType(), "filename": StrType(), "content_type": StrType(), "read": read, "close": close,
	}}
	form = Type{Kind: TypeNative, Name: "typhon.http.Multipart", Members: map[string]Type{
		"next":  nativeFuncType("http.multipart.next", nil, unionType(part, NoneType()), 0),
		"close": close,
	}}
	return
}

func armSocketDeadline(ctx context.Context, set func(time.Time) error) func() {
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		_ = set(time.Now())
		close(done)
	})
	return func() {
		if !stop() {
			<-done
		}
	}
}

func (s *httpInboundStream) Read(buf []byte) (int, error) {
	if s.closed {
		return 0, io.ErrClosedPipe
	}
	if err := s.ctx.Err(); err != nil {
		return 0, err
	}
	stop := armSocketDeadline(s.ctx, s.controller.SetReadDeadline)
	defer stop()
	n, err := s.body.Read(buf)
	if err == io.EOF {
		s.ended = true
	}
	return n, err
}

func (s *httpInboundStream) close(context.Context) error {
	s.closed = true
	return nil
}

func (s *httpInboundStream) release() error {
	if !s.ended {
		// Send an early rejection while the peer can still read it. On
		// HTTP/1 the connection will be closed after this response.
		_ = s.controller.Flush()
		if s.controller.SetReadDeadline(time.Now().Add(150*time.Millisecond)) == nil {
			// Discard a bounded tail to avoid resetting TCP before the peer
			// receives the response. This data never reaches a Typhon handler.
			_, _ = io.CopyN(io.Discard, s.transport, 2<<20)
		}
		_ = s.controller.SetReadDeadline(time.Now())
		err := s.body.Close()
		var network net.Error
		if errors.As(err, &network) && network.Timeout() {
			// Closing an unread net/http body may try to drain the socket.
			// The deadline deliberately interrupts that drain.
			return nil
		}
		return driverFailure("HTTP", connectionKind, err)
	}
	return driverFailure("HTTP", connectionKind, s.body.Close())
}

func httpInputError(err error) error {
	var limit *http.MaxBytesError
	var network net.Error
	switch {
	case errors.As(err, &limit):
		return (&webRejection{status: http.StatusRequestEntityTooLarge}).exception()
	case errors.As(err, &network) && network.Timeout():
		return (&webRejection{status: http.StatusRequestTimeout}).exception()
	default:
		return (&webRejection{status: http.StatusBadRequest}).exception()
	}
}

func readHTTPChunk(vm *VM, reader io.Reader, size int64) (Value, error) {
	if size < 1 || size > maxHTTPStreamChunk {
		return NoneValue(), errors.New("HTTP stream chunk must be between 1 and 65536 bytes")
	}
	data := make([]byte, int(size))
	n := 0
	for n < len(data) {
		bytes, err := reader.Read(data[n:])
		n += bytes
		if err == io.EOF {
			break
		}
		if err != nil {
			return NoneValue(), httpInputError(err)
		}
		if bytes == 0 {
			return NoneValue(), httpInputError(io.ErrNoProgress)
		}
	}
	if n == 0 {
		return NoneValue(), nil
	}
	if err := vm.chargeBytes(valueSlotBytes); err != nil {
		return NoneValue(), err
	}
	// Small reads can retain the full allocated buffer until the chunk is released.
	if err := vm.chargeBytes(int64(cap(data) - n)); err != nil {
		return NoneValue(), err
	}
	return nativeScalarValue(vm, "Bytes", data[:n])
}

func (s *httpInboundStream) raw(vm *VM) (Value, error) {
	if s == nil || s.mode != "" {
		return NoneValue(), errors.New("HTTP streaming body is unavailable or has already been opened")
	}
	s.mode = "raw"
	typ, _, _ := httpStreamTypes()
	return scopedNativeObject(vm, typ, map[string]Value{
		"read": driverFunc(typ.Members["read"], func(vm *VM, args []Value) (Value, error) {
			return readHTTPChunk(vm, s, args[0].Int)
		}),
		"close": driverCloser(typ.Members["close"], s.close),
	}), nil
}

type httpMultipartPart struct {
	reader    *multipart.Part
	remaining int64
	ended     bool
	failure   error
}

func (p *httpMultipartPart) Read(buf []byte) (int, error) {
	if p.failure != nil {
		return 0, p.failure
	}
	if p.ended {
		return 0, io.EOF
	}
	n, err := p.reader.Read(buf[:min(int64(len(buf)), p.remaining+1)])
	p.remaining -= int64(n)
	if p.remaining < 0 {
		p.failure = &http.MaxBytesError{Limit: p.remaining + int64(n)}
		return 0, p.failure
	}
	if err == io.EOF {
		p.ended = true
	}
	return n, err
}

func (p *httpMultipartPart) close(context.Context) error {
	if _, err := io.Copy(io.Discard, p); err != nil {
		return httpInputError(err)
	}
	return nil
}

func (s *httpInboundStream) multipart(vm *VM) (Value, error) {
	if s == nil || s.mode != "" {
		return NoneValue(), errors.New("HTTP streaming body is unavailable or has already been opened")
	}
	media, params, err := mime.ParseMediaType(s.contentType)
	boundary := params["boundary"]
	if err != nil || media != "multipart/form-data" || len(boundary) == 0 || len(boundary) > 70 {
		return NoneValue(), httpInputError(errors.New("invalid multipart content type"))
	}
	s.mode = "multipart"
	reader := multipart.NewReader(s, boundary)
	_, form, part := httpStreamTypes()
	var current *httpMultipartPart
	count := 0
	return scopedNativeObject(vm, form, map[string]Value{
		"close": driverCloser(form.Members["close"], s.close),
		"next": driverFunc(form.Members["next"], func(vm *VM, args []Value) (Value, error) {
			if s.closed {
				return NoneValue(), errors.New("multipart stream is closed")
			}
			if current != nil {
				if err := current.close(s.ctx); err != nil {
					return NoneValue(), err
				}
				current = nil
			}
			raw, err := reader.NextRawPart()
			if err == io.EOF {
				// The epilogue is part of the same body limit.
				if _, err := io.Copy(io.Discard, s); err != nil {
					return NoneValue(), httpInputError(err)
				}
				return NoneValue(), nil
			}
			if err != nil {
				return NoneValue(), httpInputError(err)
			}
			count++
			if count > s.policy.parts {
				return NoneValue(), httpInputError(&http.MaxBytesError{Limit: int64(s.policy.parts)})
			}
			headerBytes := 0
			for key, values := range raw.Header {
				for _, value := range values {
					headerBytes += len(key) + len(value)
				}
			}
			if headerBytes > 8192 {
				return NoneValue(), httpInputError(&http.MaxBytesError{Limit: 8192})
			}
			disposition, parameters, err := mime.ParseMediaType(raw.Header.Get("Content-Disposition"))
			name, filename := parameters["name"], parameters["filename"]
			if err != nil || disposition != "form-data" || !validImportIdentifier(name) || len(name) > 80 ||
				filename != "" && (len(filename) > 255 || !safeWebSegment(filename) || strings.ContainsAny(filename, ":<>|?*")) ||
				raw.Header.Get("Content-Transfer-Encoding") != "" {
				return NoneValue(), httpInputError(errors.New("invalid multipart part"))
			}
			contentType := raw.Header.Get("Content-Type")
			if len(contentType) > 256 || !validHTTPHeaderValue(contentType) {
				return NoneValue(), httpInputError(errors.New("invalid multipart part content type"))
			}
			item := &httpMultipartPart{reader: raw, remaining: s.policy.part}
			current = item
			return scopedNativeObject(vm, part, map[string]Value{
				"name": StringValue(name), "filename": StringValue(filename), "content_type": StringValue(contentType),
				"read": driverFunc(part.Members["read"], func(vm *VM, args []Value) (Value, error) {
					if s.closed {
						return NoneValue(), errors.New("multipart stream is closed")
					}
					return readHTTPChunk(vm, item, args[0].Int)
				}),
				"close": driverFunc(part.Members["close"], func(vm *VM, args []Value) (Value, error) {
					if s.closed {
						return NoneValue(), errors.New("multipart stream is closed")
					}
					return NoneValue(), item.close(s.ctx)
				}),
			}), nil
		}),
	}), nil
}

type httpStreamingResponse struct {
	headers  http.Header
	producer Value
	state    Value
}

func makeHTTPStream(vm *VM, args []Value) (Value, error) {
	if vm.request == nil {
		return NoneValue(), errors.New("create a streaming response inside an HTTP handler")
	}
	status := args[0].Int
	if status == 204 || status == 205 || status == 304 {
		return NoneValue(), errors.New("streaming response requires a status that permits a body")
	}
	headers := make(http.Header)
	if len(args) == 4 {
		var err error
		headers, err = httpHeaders(args[3])
		if err != nil {
			return NoneValue(), err
		}
	}
	for _, name := range []string{"Content-Length", "Transfer-Encoding", "Connection", "Trailer"} {
		if len(headers.Values(name)) > 0 {
			return NoneValue(), errors.New("HTTP streaming manages framing headers")
		}
	}
	if headers.Get("Content-Type") == "" {
		headers.Set("Content-Type", "application/octet-stream")
	}
	result, err := httpResponse(vm, status, "", headers)
	if err != nil {
		return NoneValue(), err
	}
	result.NativeObject.request = vm.request
	result.NativeObject.data = &httpStreamingResponse{headers: headers, producer: args[1], state: args[2]}
	return result, nil
}

func writeHTTPStream(vm *VM, writer http.ResponseWriter, req *http.Request, state *webRequest, result Value, stream *httpStreamingResponse) (bool, error) {
	if err := vm.checkNativeOwner(result.NativeObject); err != nil {
		return false, err
	}
	if err := vm.executionError(); err != nil {
		return false, err
	}
	status := int(result.NativeObject.Attrs["status"].Int)
	started := false
	start := func() {
		setHTTPResponseHeaders(writer, state, stream.headers)
		writer.WriteHeader(status)
		started = true
	}
	if req.Method == http.MethodHead {
		start()
		return true, nil
	}
	controller := http.NewResponseController(writer)
	stop := armSocketDeadline(vm.context(), controller.SetWriteDeadline)
	defer stop()
	total := int64(0)
	for {
		chunk, err := func() (Value, error) {
			vm.engine.execMu.Lock()
			defer vm.engine.execMu.Unlock()
			if err := vm.executionError(); err != nil {
				return NoneValue(), err
			}
			return vm.callValue(stream.producer, []Value{stream.state}, nil)
		}()
		if err != nil {
			return started, err
		}
		if chunk.Kind == ValueNone {
			if !started {
				start()
			}
			return true, nil
		}
		scalar, ok := scalarData(chunk)
		if !ok {
			return started, errors.New("HTTP streaming producer must return Bytes or None")
		}
		data, ok := scalar.sql.([]byte)
		if !ok || len(data) == 0 || len(data) > maxHTTPStreamChunk || total+int64(len(data)) > maxHTTPStreamBytes {
			return started, errors.New("HTTP response stream exceeded its chunk or body limit")
		}
		total += int64(len(data))
		vm.engine.execMu.Lock()
		err = vm.chargeBytes(int64(len(data)))
		vm.engine.execMu.Unlock()
		if err != nil {
			return started, err
		}
		if !started {
			start()
		}
		if _, err := writer.Write(data); err != nil {
			return true, driverFailure("HTTP", connectionKind, err)
		}
		if err := controller.Flush(); err != nil {
			return true, driverFailure("HTTP", connectionKind, err)
		}
	}
}
