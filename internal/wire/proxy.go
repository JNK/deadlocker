package wire

import (
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Proxy is a single-actor MySQL proxy. Each actor gets its own listener, which
// makes correlating captured packets with the actor that sent them exact --
// there is no need to guess from source ports.
type Proxy struct {
	target  string
	actor   string
	onEvent func(Event)

	ln      net.Listener
	closed  atomic.Bool
	connSeq atomic.Int64

	wg sync.WaitGroup
}

// Listen binds an ephemeral loopback port that forwards to target.
func Listen(target, actor string, onEvent func(Event)) (*Proxy, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen for actor %s: %w", actor, err)
	}
	p := &Proxy{target: target, actor: actor, onEvent: onEvent, ln: ln}
	p.wg.Add(1)
	go p.accept()
	return p, nil
}

// Addr is the address clients should connect to.
func (p *Proxy) Addr() string { return p.ln.Addr().String() }

func (p *Proxy) accept() {
	defer p.wg.Done()
	for {
		client, err := p.ln.Accept()
		if err != nil {
			if p.closed.Load() {
				return
			}
			// A transient accept error should not kill the listener.
			var ne net.Error
			if ok := asNetError(err, &ne); ok && ne.Timeout() {
				continue
			}
			return
		}
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			p.handle(client)
		}()
	}
}

func asNetError(err error, out *net.Error) bool {
	ne, ok := err.(net.Error)
	if ok {
		*out = ne
	}
	return ok
}

func (p *Proxy) handle(client net.Conn) {
	defer client.Close()

	server, err := net.DialTimeout("tcp", p.target, 10*time.Second)
	if err != nil {
		p.emit(Event{
			ConnID: p.connID(0), Actor: p.actor, At: time.Now(),
			Kind: "ProxyError", Phase: "handshake",
			Summary: fmt.Sprintf("could not reach MySQL at %s: %v", p.target, err),
		})
		return
	}
	defer server.Close()

	// Nagle would batch our forwarded packets and distort the timing the whole
	// tool is built to show.
	setNoDelay(client)
	setNoDelay(server)

	id := p.connID(p.connSeq.Add(1))
	dec := newConnDecoder(id, p.actor)

	p.emit(Event{
		ConnID: id, Actor: p.actor, At: time.Now(), Index: 0,
		Phase: "handshake", Kind: "ConnectionOpened",
		Summary: fmt.Sprintf("TCP connection established to %s", p.target),
	})

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		p.pump(dec, ClientToServer, client, server)
		// Signal end-of-stream so the peer can finish cleanly.
		closeWrite(server)
	}()
	go func() {
		defer wg.Done()
		p.pump(dec, ServerToClient, server, client)
		closeWrite(client)
	}()
	wg.Wait()

	p.emit(Event{
		ConnID: id, Actor: p.actor, At: time.Now(), Index: dec.nextIndex(),
		Phase: "command", Kind: "ConnectionClosed",
		Summary: "connection closed",
	})
}

// pump forwards packets one at a time, decoding each after it has been passed
// on. Forwarding first keeps the proxy off the latency path: a statement that
// blocks on a lock blocks because of the server, not because of us.
func (p *Proxy) pump(dec *connDecoder, dir Direction, src, dst net.Conn) {
	for {
		pkt, err := readPacket(src)
		if err != nil {
			if err != io.EOF && err != io.ErrUnexpectedEOF {
				// Report only genuine protocol/transport faults, not the
				// ordinary close that ends every connection.
				if !isClosedConnErr(err) {
					p.emit(Event{
						ConnID: dec.connID, Actor: p.actor, At: time.Now(),
						Direction: dir, Phase: "command", Kind: "ProxyError",
						Summary: "read: " + err.Error(),
					})
				}
			}
			return
		}
		at := time.Now()
		if _, err := dst.Write(pkt.Raw); err != nil {
			if !isClosedConnErr(err) {
				p.emit(Event{
					ConnID: dec.connID, Actor: p.actor, At: time.Now(),
					Direction: dir, Phase: "command", Kind: "ProxyError",
					Summary: "write: " + err.Error(),
				})
			}
			return
		}
		p.emit(dec.decode(dir, pkt, at))
	}
}

func (d *connDecoder) nextIndex() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.index++
	return d.index
}

func (p *Proxy) connID(n int64) string {
	return fmt.Sprintf("%s#%d", p.actor, n)
}

func (p *Proxy) emit(ev Event) {
	if p.onEvent != nil {
		p.onEvent(ev)
	}
}

// Close stops accepting and waits for in-flight connections to drain.
func (p *Proxy) Close() error {
	if p.closed.Swap(true) {
		return nil
	}
	err := p.ln.Close()
	p.wg.Wait()
	return err
}

func setNoDelay(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}
}

func closeWrite(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
	}
}

func isClosedConnErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "use of closed network connection") ||
		strings.Contains(s, "connection reset by peer") ||
		strings.Contains(s, "broken pipe")
}
