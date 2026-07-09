// Package tunnelproto implements the rift wire protocol v1.
//
// It is the single source of truth for framing on the Go side and mirrors
// cli/src/protocol.ts. See docs/PROTOCOL.md — any change here is a change to
// the contract and must be made on both sides.
package tunnelproto

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Version is the current protocol version this build speaks and advertises in
// the hello handshake. It is the maximum an agent may offer.
const Version = 1

// MinVersion is the oldest protocol version this build still accepts from an
// agent. A handshake is served when MinVersion <= agent.protocol_version <=
// Version, which gives a compatibility window for rolling client upgrades:
// bump Version for a new capability, keep MinVersion until old agents are gone,
// and only then raise MinVersion to drop them. Additive, omitempty fields do
// not need a Version bump because an older peer simply ignores them.
const MinVersion = 1

// Subprotocol is the WebSocket subprotocol both peers negotiate.
const Subprotocol = "rift.v1"

// FrameType discriminates the frame payload. Values are stable and must never
// be reused for a different meaning.
type FrameType uint8

const (
	// FrameControl carries a JSON ControlEnvelope on stream 0.
	FrameControl FrameType = 0x01

	// FrameReqHead carries a JSON RequestHead (gateway -> agent).
	FrameReqHead FrameType = 0x10
	// FrameReqBody carries a raw request body chunk (gateway -> agent).
	FrameReqBody FrameType = 0x11
	// FrameReqEnd signals end of request body (gateway -> agent).
	FrameReqEnd FrameType = 0x12

	// FrameResHead carries a JSON ResponseHead (agent -> gateway).
	FrameResHead FrameType = 0x20
	// FrameResBody carries a raw response body chunk (agent -> gateway).
	FrameResBody FrameType = 0x21
	// FrameResEnd signals end of response body (agent -> gateway).
	FrameResEnd FrameType = 0x22

	// FrameReset aborts a stream in either direction.
	FrameReset FrameType = 0x30
)

// String renders the frame type for logs.
func (t FrameType) String() string {
	switch t {
	case FrameControl:
		return "CONTROL"
	case FrameReqHead:
		return "REQ_HEAD"
	case FrameReqBody:
		return "REQ_BODY"
	case FrameReqEnd:
		return "REQ_END"
	case FrameResHead:
		return "RES_HEAD"
	case FrameResBody:
		return "RES_BODY"
	case FrameResEnd:
		return "RES_END"
	case FrameReset:
		return "RESET"
	default:
		return fmt.Sprintf("UNKNOWN(0x%02x)", uint8(t))
	}
}

// Known reports whether the frame type is defined by this protocol version.
// Unknown frames are ignored rather than fatal, so that a newer peer can add
// frame types without breaking an older one.
func (t FrameType) Known() bool {
	switch t {
	case FrameControl, FrameReqHead, FrameReqBody, FrameReqEnd,
		FrameResHead, FrameResBody, FrameResEnd, FrameReset:
		return true
	default:
		return false
	}
}

const (
	// HeaderSize is the fixed frame header length: type + stream_id + length.
	HeaderSize = 1 + 8 + 4

	// MaxPayloadBytes bounds a single frame payload. Senders chunk above this.
	MaxPayloadBytes = 1 << 20 // 1 MiB

	// MaxFrameBytes is the largest legal whole frame on the wire.
	MaxFrameBytes = HeaderSize + MaxPayloadBytes

	// ControlStreamID is the reserved stream for control frames.
	ControlStreamID uint64 = 0
)

// Errors returned by frame decoding.
var (
	ErrShortFrame      = errors.New("tunnelproto: frame shorter than header")
	ErrPayloadTooLarge = errors.New("tunnelproto: payload exceeds max frame size")
	ErrLengthMismatch  = errors.New("tunnelproto: declared length does not match payload")
	ErrControlStream   = errors.New("tunnelproto: control frame must use stream 0")
	ErrDataStream      = errors.New("tunnelproto: data frame must not use stream 0")
)

// Frame is a decoded wire frame. Payload aliases the caller's buffer on
// Decode; copy it if you retain it past the read loop.
type Frame struct {
	Type     FrameType
	StreamID uint64
	Payload  []byte
}

// Encode serialises the frame into a newly allocated buffer.
func Encode(t FrameType, streamID uint64, payload []byte) ([]byte, error) {
	if len(payload) > MaxPayloadBytes {
		return nil, fmt.Errorf("%w: %d > %d", ErrPayloadTooLarge, len(payload), MaxPayloadBytes)
	}
	if err := checkStreamID(t, streamID); err != nil {
		return nil, err
	}
	buf := make([]byte, HeaderSize+len(payload))
	buf[0] = byte(t)
	binary.BigEndian.PutUint64(buf[1:9], streamID)
	binary.BigEndian.PutUint32(buf[9:13], uint32(len(payload)))
	copy(buf[HeaderSize:], payload)
	return buf, nil
}

// Decode parses one whole frame from buf. The returned Payload aliases buf.
func Decode(buf []byte) (Frame, error) {
	if len(buf) < HeaderSize {
		return Frame{}, fmt.Errorf("%w: got %d bytes", ErrShortFrame, len(buf))
	}
	if len(buf) > MaxFrameBytes {
		return Frame{}, fmt.Errorf("%w: %d > %d", ErrPayloadTooLarge, len(buf), MaxFrameBytes)
	}
	f := Frame{
		Type:     FrameType(buf[0]),
		StreamID: binary.BigEndian.Uint64(buf[1:9]),
	}
	declared := binary.BigEndian.Uint32(buf[9:13])
	body := buf[HeaderSize:]
	if uint32(len(body)) != declared {
		return Frame{}, fmt.Errorf("%w: declared %d, got %d", ErrLengthMismatch, declared, len(body))
	}
	// Unknown frame types are surfaced to the caller, which ignores them.
	// Stream-ID discipline is only enforced for types we understand.
	if f.Type.Known() {
		if err := checkStreamID(f.Type, f.StreamID); err != nil {
			return Frame{}, err
		}
	}
	f.Payload = body
	return f, nil
}

// WriteTo serialises the frame directly into w, avoiding a second copy for
// large body chunks.
func WriteTo(w io.Writer, t FrameType, streamID uint64, payload []byte) error {
	if len(payload) > MaxPayloadBytes {
		return fmt.Errorf("%w: %d > %d", ErrPayloadTooLarge, len(payload), MaxPayloadBytes)
	}
	if err := checkStreamID(t, streamID); err != nil {
		return err
	}
	var hdr [HeaderSize]byte
	hdr[0] = byte(t)
	binary.BigEndian.PutUint64(hdr[1:9], streamID)
	binary.BigEndian.PutUint32(hdr[9:13], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(payload) == 0 {
		return nil
	}
	_, err := w.Write(payload)
	return err
}

func checkStreamID(t FrameType, streamID uint64) error {
	if t == FrameControl {
		if streamID != ControlStreamID {
			return ErrControlStream
		}
		return nil
	}
	if streamID == ControlStreamID {
		return ErrDataStream
	}
	return nil
}
