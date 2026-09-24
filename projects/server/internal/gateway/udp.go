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

// datagramLenPrefix is the size of the big-endian length that frames each
// datagram on a udp flow's tunnel stream.
const datagramLenPrefix = 2

// Flow caps. UDP source addresses are free to forge and cost an attacker one
// packet each, while every flow costs the gateway a tunnel stream, a
// return-path goroutine and a slot until the idle sweep (FlowTimeout, a minute
// by default) retires it. Without caps a spray of spoofed sources grows memory
// without bound. Datagrams that would open a flow beyond a cap are dropped,
// which is what UDP promises anyway; established flows are unaffected.
const (
	// maxUDPFlowsPerBind caps concurrent flows on one tunnel's port.
	maxUDPFlowsPerBind = 1024
	// maxUDPFlowsPerIP caps concurrent flows from one source IP (across its
	// ports), so a single host cannot take every slot on a tunnel.
	maxUDPFlowsPerIP = 64
)

// udpBufPool recycles maxDatagram-sized buffers for the per-flow return path.
// A flow spends most of its life blocked waiting for the agent, so it takes a
// buffer only once a datagram's length has arrived, rather than pinning 64 KiB
// per flow for its whole lifetime.
var udpBufPool = sync.Pool{New: func() any {
	b := make([]byte, maxDatagram)
	return &b
}}

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

	// maxFlows and maxFlowsPerIP are maxUDPFlowsPerBind and maxUDPFlowsPerIP;
	// fields only so tests can lower them.
	maxFlows      int
	maxFlowsPerIP int

	ports *portPool[*udpBind]
}

type udpBind struct {
	port int
	conn *net.UDPConn
	stop context.CancelFunc

	mu    sync.Mutex
	flows map[string]*udpFlow
	perIP map[string]int // live flow count per source IP; guarded by mu
}

type udpFlow struct {
	tconn    core.TunnelConn
	ip       string
	lastSeen time.Time
}

// removeFlowLocked forgets the flow at key and releases its per-IP slot. It
// reports whether the flow was present. b.mu must be held.
func (b *udpBind) removeFlowLocked(key string) (*udpFlow, bool) {
	fl, ok := b.flows[key]
	if !ok {
		return nil, false
	}
	delete(b.flows, key)
	if b.perIP[fl.ip]--; b.perIP[fl.ip] <= 0 {
		delete(b.perIP, fl.ip)
	}
	return fl, true
}

// hasRoomLocked reports whether a new flow from ip fits under both caps. b.mu
// must be held.
func (f *udpForwarder) hasRoomLocked(b *udpBind, ip string) bool {
	return len(b.flows) < f.maxFlows && b.perIP[ip] < f.maxFlowsPerIP
}

func newUDPForwarder(cfg *config.Config, logger *slog.Logger) *udpForwarder {
	if !cfg.UDP.Enabled {
		return nil
	}
	return &udpForwarder{
		cfg:           cfg,
		logger:        logger.With(slog.String("component", "udp")),
		maxFlows:      maxUDPFlowsPerBind,
		maxFlowsPerIP: maxUDPFlowsPerIP,
		ports:         newPortPool[*udpBind](cfg.UDP.PortMin, cfg.UDP.PortMax),
	}
}

// bind allocates a UDP port, listens on it, and starts forwarding datagrams for
// sess. It returns the public host:port the agent should advertise. A port that
// the OS reports as unavailable is skipped rather than failing the whole bind.
func (f *udpForwarder) bind(sess *session) (string, error) {
	ctx, cancel := context.WithCancel(context.Background())
	b, port, ok := f.ports.acquire(sess, func(port int) (*udpBind, error) {
		conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP(f.cfg.UDP.ListenHost), Port: port})
		if err != nil {
			return nil, err
		}
		return &udpBind{port: port, conn: conn, stop: cancel,
			flows: make(map[string]*udpFlow), perIP: make(map[string]int)}, nil
	}, func(port int, err error) {
		f.logger.Warn("udp port unavailable, skipping", slog.Int("port", port), slog.Any("error", err))
	})
	if !ok {
		cancel()
		return "", errNoUDPPorts
	}

	go f.readLoop(ctx, sess, b)
	go f.sweep(ctx, b)

	host := f.cfg.UDP.Advertise(f.cfg.Tunnel.BaseDomain)
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}

// release closes sess's listener and every flow, returning its port to the
// pool. Idempotent.
func (f *udpForwarder) release(sess *session) {
	b, ok := f.ports.release(sess)
	if !ok {
		return
	}
	b.stop()
	_ = b.conn.Close()
	b.mu.Lock()
	for _, fl := range b.flows {
		_ = fl.tconn.Close()
	}
	b.flows = map[string]*udpFlow{}
	b.perIP = map[string]int{}
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
			f.dropFlow(b, addr.String(), flow)
		}
	}
}

// flowFor returns the flow for a client address, opening a new tunnel stream and
// its return-path reader on first use. It returns nil if a stream cannot open
// or the new flow would exceed a flow cap.
func (f *udpForwarder) flowFor(ctx context.Context, sess *session, b *udpBind, addr *net.UDPAddr) *udpFlow {
	key := addr.String()
	ip := addr.IP.String()
	b.mu.Lock()
	if fl, ok := b.flows[key]; ok {
		fl.lastSeen = time.Now()
		b.mu.Unlock()
		return fl
	}
	// Check before opening a stream so a flood of new sources costs nothing
	// on the tunnel.
	if !f.hasRoomLocked(b, ip) {
		b.mu.Unlock()
		f.logger.Debug("udp flow cap reached; dropping datagram",
			slog.Int("port", b.port), slog.String("source_ip", ip))
		return nil
	}
	b.mu.Unlock()

	tconn, err := sess.OpenRaw(ctx)
	if err != nil {
		f.logger.Debug("could not open raw stream for udp flow", slog.Any("error", err))
		return nil
	}
	fl := &udpFlow{tconn: tconn, ip: ip, lastSeen: time.Now()}

	b.mu.Lock()
	// Another datagram may have raced us; keep the winner and discard our stream.
	if existing, ok := b.flows[key]; ok {
		b.mu.Unlock()
		_ = tconn.Close()
		return existing
	}
	if !f.hasRoomLocked(b, ip) {
		b.mu.Unlock()
		_ = tconn.Close()
		return nil
	}
	b.flows[key] = fl
	b.perIP[ip]++
	b.mu.Unlock()

	// Relay datagrams coming back from the agent to this client address.
	go f.returnLoop(b, addr, fl)
	return fl
}

// returnLoop reads length-delimited datagrams from the agent and writes each
// back to the public client, until the stream closes or the bind is torn down.
func (f *udpForwarder) returnLoop(b *udpBind, addr *net.UDPAddr, fl *udpFlow) {
	for {
		err := f.relayDatagram(b, addr, fl)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				f.logger.Debug("udp return path ended", slog.Any("error", err))
			}
			f.dropFlow(b, addr.String(), fl)
			return
		}
	}
}

// relayDatagram waits for one datagram from the agent and writes it to the
// public client. The pooled buffer is taken only after the length prefix has
// arrived, so an idle flow holds no buffer.
func (f *udpForwarder) relayDatagram(b *udpBind, addr *net.UDPAddr, fl *udpFlow) error {
	n, err := readDatagramLen(fl.tconn)
	if err != nil {
		return err
	}
	bp := udpBufPool.Get().(*[]byte)
	defer udpBufPool.Put(bp)
	buf := (*bp)[:n]
	if _, err := io.ReadFull(fl.tconn, buf); err != nil {
		return err
	}
	if _, err := b.conn.WriteToUDP(buf, addr); err != nil {
		return err
	}
	return nil
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
					b.removeFlowLocked(key)
				}
			}
			b.mu.Unlock()
			for _, fl := range stale {
				_ = fl.tconn.Close()
			}
		}
	}
}

// dropFlow retires fl. It removes the map entry at key only if it is still fl:
// a flow the sweep already retired may have been replaced by a fresh flow from
// the same address, and the old flow's return loop must not evict it.
func (f *udpForwarder) dropFlow(b *udpBind, key string, fl *udpFlow) {
	b.mu.Lock()
	if cur, ok := b.flows[key]; ok && cur == fl {
		b.removeFlowLocked(key)
	}
	b.mu.Unlock()
	_ = fl.tconn.Close()
}

// writeDatagram frames one datagram as a 2-byte big-endian length prefix plus
// the payload, written in a single call so concurrent flows never interleave a
// header and its body on the stream.
func writeDatagram(w io.Writer, p []byte) error {
	if len(p) > maxDatagram {
		return errors.New("gateway: udp datagram exceeds maximum size")
	}
	frame := make([]byte, datagramLenPrefix+len(p))
	binary.BigEndian.PutUint16(frame, uint16(len(p)))
	copy(frame[datagramLenPrefix:], p)
	_, err := w.Write(frame)
	return err
}

// readDatagramLen reads one datagram's 2-byte length prefix. A length above
// maxDatagram cannot be a real UDP payload and fails the flow.
func readDatagramLen(r io.Reader) (int, error) {
	var hdr [datagramLenPrefix]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, err
	}
	n := int(binary.BigEndian.Uint16(hdr[:]))
	if n > maxDatagram {
		return 0, errors.New("gateway: udp datagram length exceeds the maximum")
	}
	return n, nil
}
