package tunnelproto

import (
	"errors"
	"fmt"
)

// StreamResetError reports that a peer aborted a stream. It lives here rather
// than in the gateway so the ingress can map a reset onto an HTTP status
// without importing the transport that produced it.
type StreamResetError struct {
	Reset StreamReset
}

// NewStreamResetError wraps a reset for return through an io.Reader chain.
func NewStreamResetError(rs StreamReset) *StreamResetError {
	return &StreamResetError{Reset: rs}
}

func (e *StreamResetError) Error() string {
	if e.Reset.Message == "" {
		return fmt.Sprintf("tunnel stream reset: %s", e.Reset.Code)
	}
	return fmt.Sprintf("tunnel stream reset: %s: %s", e.Reset.Code, e.Reset.Message)
}

// ResetCodeOf extracts a reset code from anywhere in an error chain.
func ResetCodeOf(err error) (ResetCode, bool) {
	var rs *StreamResetError
	if errors.As(err, &rs) {
		return rs.Reset.Code, true
	}
	return "", false
}
