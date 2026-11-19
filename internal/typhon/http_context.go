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

func writeHTTPStop(w http.ResponseWriter, server context.Context, err error, problems problemWriter) {
	switch {
	case server.Err() != nil:
		problems.write(w, http.StatusServiceUnavailable, "server shutting down")
	case errors.Is(err, context.DeadlineExceeded):
		problems.write(w, http.StatusGatewayTimeout, "request deadline exceeded")
	default:
		problems.write(w, statusClientClosedRequest, "request cancelled")
	}
}

func readHTTPRequestBody(w http.ResponseWriter, req *http.Request, ctx context.Context, limit int64) ([]byte, error) {
	controller := http.NewResponseController(w)
	stop := armSocketDeadline(ctx, controller.SetReadDeadline)
	defer stop()
	return io.ReadAll(http.MaxBytesReader(w, req.Body, limit))
}
