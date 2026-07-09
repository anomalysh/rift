package gateway

import (
	"bytes"
	"crypto/tls"
	"net"
	"testing"
	"time"
)

// captureClientHello runs a real TLS client just far enough to emit its first
// flight, and returns the raw ClientHello record (5-byte header + handshake).
func captureClientHello(t *testing.T, serverName string) []byte {
	t.Helper()
	clientConn, serverConn := net.Pipe()

	got := make(chan []byte, 1)
	go func() {
		_ = serverConn.SetReadDeadline(time.Now().Add(2 * time.Second))
		buf := make([]byte, 4096)
		n, _ := serverConn.Read(buf) // the ClientHello record
		got <- append([]byte(nil), buf[:n]...)
		_ = serverConn.Close()
	}()

	client := tls.Client(clientConn, &tls.Config{
		ServerName:         serverName,
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS12,
	})
	// The handshake never completes (the peer is a bare pipe), but the client
	// writes its ClientHello before waiting for a reply.
	go func() { _ = client.Handshake() }()

	select {
	case rec := <-got:
		_ = clientConn.Close()
		return rec
	case <-time.After(3 * time.Second):
		_ = clientConn.Close()
		t.Fatal("timed out capturing ClientHello")
		return nil
	}
}

// sliceConn is a net.Conn whose reads drain a byte slice; only Read is used.
type sliceConn struct {
	net.Conn
	r *bytes.Reader
}

func (c *sliceConn) Read(p []byte) (int, error) { return c.r.Read(p) }

func TestPeekClientHelloExtractsSNI(t *testing.T) {
	const name = "myapp.rift.example.test"
	rec := captureClientHello(t, name)

	sni, buffered, err := peekClientHello(&sliceConn{r: bytes.NewReader(rec)})
	if err != nil {
		t.Fatalf("peekClientHello: %v", err)
	}
	if sni != name {
		t.Fatalf("sni = %q, want %q", sni, name)
	}
	// The whole record must be handed back for replay, byte for byte.
	if !bytes.Equal(buffered, rec) {
		t.Fatalf("buffered bytes (%d) do not match the record (%d)", len(buffered), len(rec))
	}
}

func TestSNIFromClientHelloDirect(t *testing.T) {
	const name = "another.rift.example.test"
	rec := captureClientHello(t, name)
	// Strip the 5-byte record header to get the handshake body.
	sni, err := sniFromClientHello(rec[5:])
	if err != nil {
		t.Fatalf("sniFromClientHello: %v", err)
	}
	if sni != name {
		t.Fatalf("sni = %q, want %q", sni, name)
	}
}

// A ClientHello parser reads bytes straight off the internet. Every truncation
// and corruption must return an error, never panic or read out of bounds.
func TestSNIParserIsPanicSafe(t *testing.T) {
	valid := captureClientHello(t, "fuzz.rift.example.test")[5:]

	// Truncate the valid hello at every length.
	for n := 0; n <= len(valid); n++ {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic on %d-byte prefix: %v", n, r)
				}
			}()
			_, _ = sniFromClientHello(valid[:n])
		}()
	}

	// A grab-bag of malformed inputs must all be handled without panicking.
	malformed := [][]byte{
		{},
		{0x01},
		{0x16, 0x03, 0x01},                   // record header only, no length
		{0x01, 0xff, 0xff, 0xff},             // ClientHello with absurd length
		{0x01, 0x00, 0x00, 0x00},             // ClientHello, zero length, nothing else
		bytes.Repeat([]byte{0xff}, 64),       // pure garbage
		append([]byte{0x01}, valid[1:10]...), // partial, cut mid-field
	}
	for i, b := range malformed {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic on malformed case %d: %v", i, r)
				}
			}()
			if _, err := sniFromClientHello(b); err == nil {
				t.Errorf("malformed case %d: expected an error", i)
			}
		}()
	}
}

func TestPeekClientHelloRejectsNonHandshake(t *testing.T) {
	// An HTTP request, say, is not a TLS handshake record.
	notTLS := []byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")
	if _, _, err := peekClientHello(&sliceConn{r: bytes.NewReader(notTLS)}); err == nil {
		t.Fatal("expected an error for a non-handshake first byte")
	}
}
