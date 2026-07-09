package tunnelproto

import (
	"bytes"
	"errors"
	"testing"
)

func TestEncodeDecodeRoundTrip(t *testing.T) {
	cases := []struct {
		name     string
		typ      FrameType
		streamID uint64
		payload  []byte
	}{
		{"control", FrameControl, ControlStreamID, []byte(`{"type":"ping"}`)},
		{"req head", FrameReqHead, 1, []byte(`{"method":"GET"}`)},
		{"empty body chunk", FrameResBody, 7, []byte{}},
		{"nil payload", FrameReqEnd, 42, nil},
		{"binary body", FrameResBody, 1 << 40, []byte{0x00, 0xff, 0x7f, 0x80}},
		{"max payload", FrameReqBody, 3, bytes.Repeat([]byte{0xab}, MaxPayloadBytes)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := Encode(tc.typ, tc.streamID, tc.payload)
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			if want := HeaderSize + len(tc.payload); len(raw) != want {
				t.Fatalf("encoded length = %d, want %d", len(raw), want)
			}
			got, err := Decode(raw)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if got.Type != tc.typ || got.StreamID != tc.streamID {
				t.Fatalf("got type=%v stream=%d, want type=%v stream=%d", got.Type, got.StreamID, tc.typ, tc.streamID)
			}
			if !bytes.Equal(got.Payload, tc.payload) {
				t.Fatalf("payload mismatch: got %d bytes, want %d", len(got.Payload), len(tc.payload))
			}
		})
	}
}

func TestEncodeRejectsOversizePayload(t *testing.T) {
	_, err := Encode(FrameReqBody, 1, make([]byte, MaxPayloadBytes+1))
	if !errors.Is(err, ErrPayloadTooLarge) {
		t.Fatalf("got %v, want ErrPayloadTooLarge", err)
	}
}

// Stream-ID discipline is what keeps control traffic from being mistaken for
// data on a busy connection, so both directions of the rule are enforced.
func TestStreamIDDiscipline(t *testing.T) {
	if _, err := Encode(FrameControl, 1, nil); !errors.Is(err, ErrControlStream) {
		t.Fatalf("control on stream 1: got %v, want ErrControlStream", err)
	}
	if _, err := Encode(FrameReqHead, ControlStreamID, nil); !errors.Is(err, ErrDataStream) {
		t.Fatalf("data on stream 0: got %v, want ErrDataStream", err)
	}
}

func TestDecodeRejectsMalformed(t *testing.T) {
	t.Run("short header", func(t *testing.T) {
		if _, err := Decode(make([]byte, HeaderSize-1)); !errors.Is(err, ErrShortFrame) {
			t.Fatalf("want ErrShortFrame, got %v", err)
		}
	})

	t.Run("length lies about payload", func(t *testing.T) {
		raw, err := Encode(FrameReqBody, 1, []byte("hello"))
		if err != nil {
			t.Fatal(err)
		}
		raw = raw[:len(raw)-1] // truncate payload, leave declared length at 5
		if _, err := Decode(raw); !errors.Is(err, ErrLengthMismatch) {
			t.Fatalf("want ErrLengthMismatch, got %v", err)
		}
	})
}

// A peer speaking a newer protocol may send frames we do not know. Decoding
// must succeed so the caller can skip them rather than tear down the tunnel.
func TestDecodeAllowsUnknownFrameType(t *testing.T) {
	raw, err := Encode(FrameReqBody, 9, []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	raw[0] = 0xEE

	got, err := Decode(raw)
	if err != nil {
		t.Fatalf("Decode of unknown type: %v", err)
	}
	if got.Type.Known() {
		t.Fatal("0xEE should not be a known frame type")
	}
}

func TestWriteToMatchesEncode(t *testing.T) {
	payload := []byte("streamed chunk")
	want, err := Encode(FrameResBody, 5, payload)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := WriteTo(&buf, FrameResBody, 5, payload); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatal("WriteTo output differs from Encode output")
	}
}

func TestControlRoundTrip(t *testing.T) {
	raw, err := EncodeControl(ControlHelloOK, HelloOK{
		TunnelID: "T1", Subdomain: "demo", Hostname: "demo.example.com",
		URL: "https://demo.example.com", HeartbeatIntervalMS: 15000,
	})
	if err != nil {
		t.Fatalf("EncodeControl: %v", err)
	}
	frame, err := Decode(raw)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if frame.Type != FrameControl || frame.StreamID != ControlStreamID {
		t.Fatalf("unexpected control framing: %v/%d", frame.Type, frame.StreamID)
	}
	env, err := DecodeControl(frame.Payload)
	if err != nil {
		t.Fatalf("DecodeControl: %v", err)
	}
	if env.Type != ControlHelloOK {
		t.Fatalf("got control type %q, want %q", env.Type, ControlHelloOK)
	}
	var ok HelloOK
	if err := UnmarshalPayload(env, &ok); err != nil {
		t.Fatalf("UnmarshalPayload: %v", err)
	}
	if ok.Subdomain != "demo" || ok.HeartbeatIntervalMS != 15000 {
		t.Fatalf("payload round-trip mismatch: %+v", ok)
	}
}

func TestDecodeControlRejectsMissingType(t *testing.T) {
	if _, err := DecodeControl([]byte(`{"payload":{}}`)); err == nil {
		t.Fatal("expected error for envelope without a type")
	}
}
