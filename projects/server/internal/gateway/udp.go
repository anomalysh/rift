package gateway

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/anomalysh/rift/projects/server/internal/config"
	"github.com/anomalysh/rift/projects/server/internal/core"
)

// errNoUDPPorts means every port in the configured UDP range is in use.
var errNoUDPPorts = errors.New("gateway: no free udp ports in the configured range")

// maxDatagram bounds a single UDP payload carried over the tunnel. It is the
// largest a UDP datagram can be (65535 minus the 8-byte UDP and 20-byte IPv4
// headers), and also the cap the length-delimited framing enforces so a
// corrupt length cannot make either side allocate without bound.
const maxDatagram = 65507

// udpForwarder accepts public UDP datagrams on a per-tunnel port and forwards
// each client flow to the agent as a length-delimited datagram stream over a
// raw tunnel stream. It owns allocation of the configured UDP port range.
//
// UDP has no connections, so a "flow" is one client source address. The first
// datagram from an address opens a raw stream to the agent; later datagrams
// reuse it; an idle flow is retired after cfg.UDP.FlowTimeout.
type udpForwarder struct {
	cfg    *config.Config
	logger *slog.Logger

	mu    sync.Mutex
	free  []int
	binds map[*session]*udpBind
}

type udpBind struct {
	port int
	conn *net.UDPConn
	stop context.CancelFunc

	mu    sync.Mutex
	flows map[string]*udpFlow
}

type udpFlow struct {
	tconn    core.TunnelConn
	lastSeen time.Time
}

func newUDPForwarder(cfg *config.Config, logger *slog.Logger) *udpForwarder {
	if !cfg.UDP.Enabled {
		return nil
	}
	free := make([]int, 0, cfg.UDP.PortMax-cfg.UDP.PortMin+1)
	for p := cfg.UDP.PortMin; p <= cfg.UDP.PortMax; p++ {
		free = append(free, p)
	}
	return &udpForwarder{
		cfg:    cfg,
		logger: logger.With(slog.String("component", "udp")),
		free:   free,
		binds:  make(map[*session]*udpBind),
	}
}

// bind allocates a UDP port, listens on it, and starts forwarding datagrams for
// sess. It returns the public host:port the agent should advertise.
func (f *udpForwarder) bind(sess *session) (string, error) {
	f.mu.Lock()

	var (
		port int
		conn *net.UDPConn
	)
	for len(f.free) > 0 {
		port = f.free[0]
		f.free = f.free[1:]
		addr := &net.UDPAddr{IP: net.ParseIP(f.cfg.UDP.ListenHost), Port: port}
		c, err := net.ListenUDP("udp", addr)
		if err != nil {
			f.logger.Warn("udp port unavailable, skipping", slog.Int("port", port), slog.Any("error", err))
			continue
		}
		conn = c
		break
	}
	if conn == nil {
		f.mu.Unlock()
		return "", errNoUDPPorts
	}

	ctx, cancel := context.WithCancel(context.Background())
	b := &udpBind{port: port, conn: conn, stop: cancel, flows: make(map[string]*udpFlow)}
	f.binds[sess] = b
	f.mu.Unlock()

	go f.readLoop(ctx, sess, b)
	go f.sweep(ctx, b)

	host := f.cfg.UDP.Advertise(f.cfg.Tunnel.BaseDomain)
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}

// release closes sess's listener and every flow, returning its port to the
// pool. Idempotent.
func (f *udpForwarder) release(sess *session) {
	f.mu.Lock()
	b, ok := f.binds[sess]
	if !ok {
		f.mu.Unlock()
		return
	}
	delete(f.binds, sess)
	f.free = append(f.free, b.port)
	f.mu.Unlock()

	b.stop()
	_ = b.conn.Close()
	b.mu.Lock()
	for _, fl := range b.flows {
		_ = fl.tconn.Close()
	}
	b.flows = map[string]*udpFlow{}
	b.mu.Unlock()
}

// readLoop reads public datagrams and forwards each to its flow's tunnel stream.
func (f *udpForwarder) readLoop(ctx context.Context, sess *session, b *udpBind) {
	buf := make([]byte, maxDatagram)
	for {
		n, addr, err := b.conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-ctx.Done():
			default:
				f.logger.Debug("udp read ended", slog.Int("port", b.port), slog.Any("error", err))
			}
			return
		}
		flow := f.flowFor(ctx, sess, b, addr)
		if flow == nil {
			continue
		}
		if err := writeDatagram(flow.tconn, buf[:n]); err != nil {
			f.logger.Debug("udp forward to agent failed", slog.Any("error", err))
			f.dropFlow(b, addr.String())
		}
	}
}

// flowFor returns the flow for a client address, opening a new tunnel stream and
// its return-path reader on first use. It returns nil if a stream cannot open.
func (f *udpForwarder) flowFor(ctx context.Context, sess *session, b *udpBind, addr *net.UDPAddr) *udpFlow {
	key := addr.String()
	b.mu.Lock()
	if fl, ok := b.flows[key]; ok {
		fl.lastSeen = time.Now()
		b.mu.Unlock()
		return fl
	}
	b.mu.Unlock()

	tconn, err := sess.OpenRaw(ctx)
	if err != nil {
		f.logger.Debug("could not open raw stream for udp flow", slog.Any("error", err))
		return nil
	}
	fl := &udpFlow{tconn: tconn, lastSeen: time.Now()}

	b.mu.Lock()
	// Another datagram may have raced us; keep the winner and discard our stream.
	if existing, ok := b.flows[key]; ok {
		b.mu.Unlock()
		_ = tconn.Close()
		return existing
	}
	b.flows[key] = fl
	b.mu.Unlock()

	// Relay datagrams coming back from the agent to this client address.
	go f.returnLoop(b, addr, fl)
	return fl
}

// returnLoop reads length-delimited datagrams from the agent and writes each
// back to the public client, until the stream closes or the bind is torn down.
func (f *udpForwarder) returnLoop(b *udpBind, addr *net.UDPAddr, fl *udpFlow) {
	buf := make([]byte, maxDatagram)
	for {
		n, err := readDatagram(fl.tconn, buf)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				f.logger.Debug("udp return read ended", slog.Any("error", err))
			}
			f.dropFlow(b, addr.String())
			return
		}
		if _, err := b.conn.WriteToUDP(buf[:n], addr); err != nil {
			f.logger.Debug("udp write to client failed", slog.Any("error", err))
			f.dropFlow(b, addr.String())
			return
		}
	}
}

// sweep periodically retires flows idle past the configured timeout.
func (f *udpForwarder) sweep(ctx context.Context, b *udpBind) {
	interval := f.cfg.UDP.FlowTimeout / 2
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cutoff := time.Now().Add(-f.cfg.UDP.FlowTimeout)
			var stale []*udpFlow
			b.mu.Lock()
			for key, fl := range b.flows {
				if fl.lastSeen.Before(cutoff) {
					stale = append(stale, fl)
					delete(b.flows, key)
				}
			}
			b.mu.Unlock()
			for _, fl := range stale {
				_ = fl.tconn.Close()
			}
		}
	}
}

func (f *udpForwarder) dropFlow(b *udpBind, key string) {
	b.mu.Lock()
	fl, ok := b.flows[key]
	if ok {
		delete(b.flows, key)
	}
	b.mu.Unlock()
	if ok {
		_ = fl.tconn.Close()
	}
}

// writeDatagram frames one datagram as a 2-byte big-endian length prefix plus
// the payload, written in a single call so concurrent flows never interleave a
// header and its body on the stream.
func writeDatagram(w io.Writer, p []byte) error {
	if len(p) > maxDatagram {
		return errors.New("gateway: udp datagram exceeds maximum size")
	}
	frame := make([]byte, 2+len(p))
	binary.BigEndian.PutUint16(frame[:2], uint16(len(p)))
	copy(frame[2:], p)
	_, err := w.Write(frame)
	return err
}

// readDatagram reads one length-delimited datagram into buf, returning its
// length. A length larger than buf is a framing error and fails the flow rather
// than reading unbounded.
func readDatagram(r io.Reader, buf []byte) (int, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, err
	}
	n := int(binary.BigEndian.Uint16(hdr[:]))
	if n > len(buf) {
		return 0, errors.New("gateway: udp datagram length exceeds buffer")
	}
	if _, err := io.ReadFull(r, buf[:n]); err != nil {
		return 0, err
	}
	return n, nil
}
