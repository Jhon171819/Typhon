package typhon

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"
)

const defaultRequestTimeout = 10 * time.Second
const statusClientClosedRequest = 499

func httpRequestContext(parent, server context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	stop := context.AfterFunc(server, cancel)
	if server.Err() != nil {
		cancel()
	}
	return ctx, func() {
		stop()
		cancel()
	}
}

func writeHTTPStop(w http.ResponseWriter, server context.Context, err error) {
	switch {
	case server.Err() != nil:
		http.Error(w, "server shutting down", http.StatusServiceUnavailable)
	case errors.Is(err, context.DeadlineExceeded):
		http.Error(w, "request deadline exceeded", http.StatusGatewayTimeout)
	default:
		http.Error(w, "request cancelled", statusClientClosedRequest)
	}
}

func readHTTPRequestBody(w http.ResponseWriter, req *http.Request, ctx context.Context) ([]byte, error) {
	controller := http.NewResponseController(w)
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		// Some embedding ResponseWriters (including httptest recorders)
		// have no socket. Real net/http handlers support read deadlines.
		_ = controller.SetReadDeadline(time.Now())
		close(done)
	})
	defer func() {
		if !stop() {
			<-done
		}
	}()
	return io.ReadAll(http.MaxBytesReader(w, req.Body, maxHTTPBody))
}
