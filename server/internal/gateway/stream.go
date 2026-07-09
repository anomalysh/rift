package gateway

import (
	"fmt"
	"io"
	"sync"

	"github.com/anomaly-sh/rift/server/internal/core"
	"github.com/anomaly-sh/rift/server/internal/tunnelproto"
)

// errSessionClosed means the tunnel died with the stream in flight.
var errSessionClosed = fmt.Errorf("gateway: tunnel session closed: %w", core.ErrTunnelUnavailable)

// stream is one in-flight proxied request/response pair.
type stream struct {
	id uint64

	// head carries the single ResponseHead. Buffered so the read loop never
	// blocks handing it over.
	head chan tunnelproto.ResponseHead

	// body carries response chunks. Bounded: see session.deliverBody for the
	// head-of-line tradeoff this bound implies.
	body chan []byte

	// done is closed exactly once, when the stream is finished or aborted.
	done chan struct{}

	closeOnce sync.Once
	endOnce   sync.Once

	mu  sync.Mutex
	err error // set before done is closed; nil means a clean end
}

func newStream(id uint64, bufferSize int) *stream {
	return &stream{
		id:   id,
		head: make(chan tunnelproto.ResponseHead, 1),
		body: make(chan []byte, bufferSize),
		done: make(chan struct{}),
	}
}

// abort terminates the stream with err. The first caller wins, so a reset that
// races the session shutdown reports whichever reason arrived first.
func (s *stream) abort(err error) {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.err = err
		s.mu.Unlock()
		close(s.done)
	})
}

// endBody signals a clean end of the response body.
func (s *stream) endBody() {
	s.endOnce.Do(func() { close(s.body) })
}

func (s *stream) reason() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// bodyReader adapts the stream's chunk channel to an io.ReadCloser suitable
// for http.Response.Body.
type bodyReader struct {
	st      *stream
	sess    *session
	cur     []byte
	closed  bool
	drained bool
}

func (r *bodyReader) Read(p []byte) (int, error) {
	if r.closed {
		return 0, io.ErrClosedPipe
	}
	for len(r.cur) == 0 {
		if r.drained {
			return 0, io.EOF
		}
		// An abort outranks buffered chunks: the response is incomplete and
		// the caller must not mistake a truncated body for a whole one.
		select {
		case <-r.st.done:
			if err := r.st.reason(); err != nil {
				return 0, err
			}
			return 0, io.EOF
		default:
		}

		select {
		case chunk, ok := <-r.st.body:
			if !ok {
				r.drained = true
				return 0, io.EOF
			}
			r.cur = chunk
		case <-r.st.done:
			if err := r.st.reason(); err != nil {
				return 0, err
			}
			return 0, io.EOF
		}
	}
	n := copy(p, r.cur)
	r.cur = r.cur[n:]
	return n, nil
}

// Close releases the stream. If the body was not fully read, the agent is told
// to cancel the local request rather than keep streaming into a dead socket.
func (r *bodyReader) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true

	incomplete := !r.drained
	select {
	case <-r.st.done:
		incomplete = false // already aborted or ended; nothing to cancel
	default:
	}

	r.st.abort(nil)
	r.sess.forgetStream(r.st.id)

	if incomplete {
		r.sess.sendReset(r.st.id, tunnelproto.StreamReset{
			Code:    tunnelproto.ResetClientDisconnected,
			Message: "public client closed the connection",
		})
	}
	return nil
}
