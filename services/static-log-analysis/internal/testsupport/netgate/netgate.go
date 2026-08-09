// Package netgate is a TCP proxy a test can close and reopen, so a dependency
// can be made unreachable without being destroyed.
//
// It exists because the obvious way to simulate a CockroachDB outage — stopping
// the container — does not work here. The testcontainers CockroachDB module
// runs the node with an in-memory store, so stopping it destroys every database
// it held, and a recovery test could no longer tell "the service recovered"
// from "the service wrote into a fresh empty database". Docker also republishes
// on a new host port after a restart, so every connection string handed out
// beforehand goes stale.
//
// Cutting the network instead is both easier and closer to the failure being
// modelled. From the service's side an unreachable database is exactly what an
// outage is, and from the data's side nothing happened at all, which is what
// makes "every record contributes exactly once afterwards" a meaningful claim.
package netgate

import (
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
)

// Gate forwards TCP connections to an upstream address until it is closed.
type Gate struct {
	upstream string
	listener net.Listener

	mu     sync.Mutex
	open   bool
	active map[net.Conn]struct{}

	wg       sync.WaitGroup
	stopOnce sync.Once
	stopped  chan struct{}
}

// New starts a gate in front of upstream. It begins open.
func New(upstream string) (*Gate, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("netgate: listen: %w", err)
	}
	gate := &Gate{
		upstream: upstream, listener: listener, open: true,
		active: make(map[net.Conn]struct{}), stopped: make(chan struct{}),
	}
	gate.wg.Add(1)
	go gate.accept()
	return gate, nil
}

// Addr is the address callers should connect to instead of the upstream.
func (g *Gate) Addr() string { return g.listener.Addr().String() }

// Close shuts the gate down entirely.
func (g *Gate) Close() {
	g.stopOnce.Do(func() {
		close(g.stopped)
		_ = g.listener.Close()
		g.Block()
	})
	g.wg.Wait()
}

// Block makes the upstream unreachable: every established connection is severed
// and every new one is refused immediately.
//
// Refusing rather than hanging is deliberate. A hung connect is a different
// failure — it is what a firewall does, not what a stopped process does — and
// conflating the two would let a test pass on a timeout path it never meant to
// exercise.
func (g *Gate) Block() {
	g.mu.Lock()
	g.open = false
	conns := make([]net.Conn, 0, len(g.active))
	for conn := range g.active {
		conns = append(conns, conn)
	}
	g.active = make(map[net.Conn]struct{})
	g.mu.Unlock()
	for _, conn := range conns {
		_ = conn.Close()
	}
}

// Unblock lets connections through again.
func (g *Gate) Unblock() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.open = true
}

// Blocked reports whether the gate is currently closed.
func (g *Gate) Blocked() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return !g.open
}

func (g *Gate) accept() {
	defer g.wg.Done()
	for {
		conn, err := g.listener.Accept()
		if err != nil {
			select {
			case <-g.stopped:
				return
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		g.mu.Lock()
		open := g.open
		g.mu.Unlock()
		if !open {
			// Accepting and immediately closing is what a listener with nothing
			// behind it looks like to a client.
			_ = conn.Close()
			continue
		}
		g.wg.Add(1)
		go g.forward(conn)
	}
}

func (g *Gate) forward(downstream net.Conn) {
	defer g.wg.Done()
	upstream, err := net.Dial("tcp", g.upstream)
	if err != nil {
		_ = downstream.Close()
		return
	}
	g.track(downstream, upstream)
	defer g.untrack(downstream, upstream)

	var pipes sync.WaitGroup
	pipes.Add(2)
	go func() { defer pipes.Done(); _, _ = io.Copy(upstream, downstream); _ = upstream.Close() }()
	go func() { defer pipes.Done(); _, _ = io.Copy(downstream, upstream); _ = downstream.Close() }()
	pipes.Wait()
}

func (g *Gate) track(conns ...net.Conn) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.open {
		// Blocked between accept and dial. Closing here keeps a connection from
		// slipping through the gate after it shut.
		for _, conn := range conns {
			_ = conn.Close()
		}
		return
	}
	for _, conn := range conns {
		g.active[conn] = struct{}{}
	}
}

func (g *Gate) untrack(conns ...net.Conn) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, conn := range conns {
		delete(g.active, conn)
	}
}
