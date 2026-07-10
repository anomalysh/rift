package gateway

import (
	"bytes"
	"io"
	"testing"
)

func TestDatagramRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	payloads := [][]byte{
		[]byte("hello"),
		{},                               // an empty datagram is valid
		bytes.Repeat([]byte{0xAB}, 1500), // a jumbo-ish datagram
	}
	for _, p := range payloads {
		if err := writeDatagram(&buf, p); err != nil {
			t.Fatalf("writeDatagram: %v", err)
		}
	}

	// The three datagrams read back in order with their boundaries intact, even
	// though they were written into one contiguous buffer.
	out := make([]byte, maxDatagram)
	for i, want := range payloads {
		n, err := readDatagram(&buf, out)
		if err != nil {
			t.Fatalf("readDatagram %d: %v", i, err)
		}
		if !bytes.Equal(out[:n], want) {
			t.Fatalf("datagram %d = %q, want %q", i, out[:n], want)
		}
	}
	// The stream is now drained.
	if _, err := readDatagram(&buf, out); err != io.EOF {
		t.Fatalf("expected EOF after the last datagram, got %v", err)
	}
}

func TestReadDatagramRejectsOversizeLength(t *testing.T) {
	// A length prefix larger than the caller's buffer must fail closed, not read
	// unbounded.
	framed := []byte{0xFF, 0xFF} // declares 65535 bytes
	small := make([]byte, 16)
	if _, err := readDatagram(bytes.NewReader(framed), small); err == nil {
		t.Fatal("expected an error for a length exceeding the buffer")
	}
}

func TestWriteDatagramRejectsOversize(t *testing.T) {
	var buf bytes.Buffer
	if err := writeDatagram(&buf, make([]byte, maxDatagram+1)); err == nil {
		t.Fatal("expected an error for a datagram over the maximum size")
	}
}
