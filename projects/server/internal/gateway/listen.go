package gateway

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"time"
)

// Accept retry backoff, as net/http.Server does it. An Accept can fail for
// reasons that pass (the process is briefly out of file descriptors, a
// connection was aborted before it was accepted); ending the loop on the first
// one would leave the port dead while its tunnel still advertises it.
const (
	acceptBackoffMin = 5 * time.Millisecond
	acceptBackoffMax = time.Second
)

// acceptLoop accepts connections on ln and serves each on its own goroutine
// with handle. It returns nil once ctx is done (the owner closes ln then), or
// net.ErrClosed if ln was closed while ctx was still live. Any other Accept
// error is logged and retried after a growing pause.
func acceptLoop(ctx context.Context, ln net.Listener, logger *slog.Logger, handle func(net.Conn)) error {
	var backoff time.Duration
	for {
		conn, err := ln.Accept()
		if err == nil {
			backoff = 0
			go handle(conn)
			continue
		}
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, net.ErrClosed) {
			return err
		}
		backoff = min(max(2*backoff, acceptBackoffMin), acceptBackoffMax)
		logger.Warn("accept failed; retrying",
			slog.String("addr", ln.Addr().String()),
			slog.Duration("backoff", backoff),
			slog.Any("error", err))
		t := time.NewTimer(backoff)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return nil
		}
	}
}
