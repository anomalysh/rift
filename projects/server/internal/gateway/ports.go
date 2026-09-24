package gateway

import "sync"

// portPool hands out ports from a configured range to per-tunnel public
// listeners and remembers which session holds which bind. The tcp and udp
// forwarders each own one; B is the forwarder's per-tunnel bind state.
type portPool[B any] struct {
	mu    sync.Mutex
	free  []int
	binds map[*session]portBind[B]
}

type portBind[B any] struct {
	port int
	bind B
}

func newPortPool[B any](portMin, portMax int) *portPool[B] {
	free := make([]int, 0, portMax-portMin+1)
	for p := portMin; p <= portMax; p++ {
		free = append(free, p)
	}
	return &portPool[B]{free: free, binds: make(map[*session]portBind[B])}
}

// acquire tries free ports in order until open succeeds on one, records the
// resulting bind for sess, and returns it with its port. ok is false when no
// port in the range could be opened.
//
// open runs under the pool lock, so two binds never race for the same port.
// A port open refuses (another process holds it, say) is skipped for this
// bind but stays in the pool, at the back: dropping it would shrink the range
// for the life of the process over what may be a momentary conflict.
func (p *portPool[B]) acquire(sess *session, open func(port int) (B, error), skipped func(port int, err error)) (b B, port int, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	var busy []int
	defer func() { p.free = append(p.free, busy...) }()

	for len(p.free) > 0 {
		port = p.free[0]
		p.free = p.free[1:]
		opened, err := open(port)
		if err != nil {
			skipped(port, err)
			busy = append(busy, port)
			continue
		}
		p.binds[sess] = portBind[B]{port: port, bind: opened}
		return opened, port, true
	}
	return b, 0, false
}

// release forgets sess's bind and returns its port to the back of the pool,
// so a just-freed port is the last to be handed out again. ok is false when
// sess holds no bind, which makes release idempotent for the caller.
func (p *portPool[B]) release(sess *session) (b B, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pb, ok := p.binds[sess]
	if !ok {
		return b, false
	}
	delete(p.binds, sess)
	p.free = append(p.free, pb.port)
	return pb.bind, true
}
