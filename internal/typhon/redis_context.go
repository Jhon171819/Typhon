package typhon

import (
	"context"
	"net"
	"sync/atomic"
	"time"
)

// go-redis sets socket deadlines from the operation context. An early
// cancellation still needs to wake blocked I/O. The owner serializes commands
// per RESP2 client.
type redisContextConn struct {
	net.Conn
	operation *atomic.Pointer[context.Context]
}

func (c *redisContextConn) Read(p []byte) (int, error) {
	return c.withCancellation(p, c.Conn.Read, c.Conn.SetReadDeadline)
}

func (c *redisContextConn) Write(p []byte) (int, error) {
	return c.withCancellation(p, c.Conn.Write, c.Conn.SetWriteDeadline)
}

func (c *redisContextConn) withCancellation(p []byte, run func([]byte) (int, error), deadline func(time.Time) error) (int, error) {
	active := c.operation.Load()
	if active == nil {
		return run(p)
	}
	ctx := *active
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		_ = deadline(time.Now())
		close(done)
	})
	n, err := run(p)
	if !stop() {
		<-done
	}
	if cancelled := ctx.Err(); cancelled != nil {
		// Returning cancellation makes go-redis discard an ambiguous stream.
		return n, cancelled
	}
	return n, err
}
