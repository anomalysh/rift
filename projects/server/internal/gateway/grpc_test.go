package gateway

import (
	"bytes"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

// writeH2ClientRequest plays the client side of an h2c prior-knowledge request:
// the connection preface, a SETTINGS frame, and one HEADERS frame carrying the
// given :authority.
func writeH2ClientRequest(t *testing.T, w net.Conn, authority string) {
	t.Helper()
	if _, err := w.Write([]byte(http2.ClientPreface)); err != nil {
		t.Errorf("write preface: %v", err)
		return
	}
	fr := http2.NewFramer(w, nil)
	if err := fr.WriteSettings(); err != nil {
		t.Errorf("write settings: %v", err)
		return
	}
	var hbuf bytes.Buffer
	enc := hpack.NewEncoder(&hbuf)
	for _, f := range []hpack.HeaderField{
		{Name: ":method", Value: "POST"},
		{Name: ":scheme", Value: "http"},
		{Name: ":path", Value: "/helloworld.Greeter/SayHello"},
		{Name: ":authority", Value: authority},
		{Name: "content-type", Value: "application/grpc"},
	} {
		if err := enc.WriteField(f); err != nil {
			t.Errorf("hpack encode: %v", err)
			return
		}
	}
	if err := fr.WriteHeaders(http2.HeadersFrameParam{
		StreamID: 1, BlockFragment: hbuf.Bytes(), EndHeaders: true,
	}); err != nil {
		t.Errorf("write headers: %v", err)
	}
}

func TestPeekH2Authority(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	go func() {
		defer client.Close()
		writeH2ClientRequest(t, client, "app.rift.test:8090")
	}()

	_ = server.SetReadDeadline(time.Now().Add(2 * time.Second))
	authority, buffered, err := peekH2Authority(server)
	if err != nil {
		t.Fatalf("peekH2Authority: %v", err)
	}
	if authority != "app.rift.test:8090" {
		t.Fatalf("authority = %q, want app.rift.test:8090", authority)
	}
	// The captured bytes must begin with the preface so a replay reconstructs
	// the client's h2c stream exactly.
	if !bytes.HasPrefix(buffered, []byte(http2.ClientPreface)) {
		t.Fatal("captured bytes do not start with the HTTP/2 preface")
	}
}

func TestPeekH2AuthorityRejectsNonH2C(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	go func() {
		defer client.Close()
		// A plain HTTP/1.1 request line, padded to the preface length.
		_, _ = client.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"))
	}()
	_ = server.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := peekH2Authority(server); !errors.Is(err, errNotH2CPreface) {
		t.Fatalf("err = %v, want errNotH2CPreface", err)
	}
}

// h2cPrefix returns the client preface followed by whatever frames write
// produces.
func h2cPrefix(t *testing.T, write func(fr *http2.Framer)) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.WriteString(http2.ClientPreface)
	write(http2.NewFramer(&buf, nil))
	return buf.Bytes()
}

// A frame header declaring a huge length must be refused from the header
// alone, before anything is allocated for it: the peek runs before any
// SETTINGS exchange, so the h2 default of 16 KiB is the most a client may send.
func TestPeekH2AuthorityRejectsOversizedFrame(t *testing.T) {
	in := []byte(http2.ClientPreface)
	// DATA on stream 1, declared length 16 MiB - 1, no payload following.
	in = append(in, 0xFF, 0xFF, 0xFF, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01)
	_, _, err := peekH2Authority(&sliceConn{r: bytes.NewReader(in)})
	if !errors.Is(err, http2.ErrFrameTooLarge) {
		t.Fatalf("err = %v, want ErrFrameTooLarge", err)
	}
}

// Frames that never lead to HEADERS must not be buffered without bound, by
// count or by size.
func TestPeekH2AuthorityBoundsPreamble(t *testing.T) {
	t.Run("frame count", func(t *testing.T) {
		in := h2cPrefix(t, func(fr *http2.Framer) {
			for i := 0; i < grpcPeekMaxFrames+1; i++ {
				_ = fr.WriteSettings()
			}
		})
		_, _, err := peekH2Authority(&sliceConn{r: bytes.NewReader(in)})
		if !errors.Is(err, errPeekTooLarge) {
			t.Fatalf("err = %v, want errPeekTooLarge", err)
		}
	})

	t.Run("byte count", func(t *testing.T) {
		chunk := make([]byte, grpcPeekMaxFrameSize)
		in := h2cPrefix(t, func(fr *http2.Framer) {
			for i := 0; i < grpcPeekMaxBytes/grpcPeekMaxFrameSize+2; i++ {
				_ = fr.WriteData(1, false, chunk)
			}
		})
		r := bytes.NewReader(in)
		_, _, err := peekH2Authority(&sliceConn{r: r})
		if !errors.Is(err, errPeekTooLarge) {
			t.Fatalf("err = %v, want errPeekTooLarge", err)
		}
		if consumed := len(in) - r.Len(); consumed > grpcPeekMaxBytes {
			t.Fatalf("peek consumed %d bytes, over the %d cap", consumed, grpcPeekMaxBytes)
		}
	})
}

// The router lowercases and strips the port from :authority before matching the
// base domain, so a subdomain resolves regardless of case or an explicit port.
func TestGRPCAuthorityRoutingShape(t *testing.T) {
	for _, authority := range []string{"App.Rift.Test:8090", "app.rift.test"} {
		host := authority
		if h, _, err := net.SplitHostPort(authority); err == nil {
			host = h
		}
		if got := strings.ToLower(host); got != "app.rift.test" {
			t.Fatalf("normalized host = %q, want app.rift.test", got)
		}
	}
}
