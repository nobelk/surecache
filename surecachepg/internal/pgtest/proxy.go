// SPDX-License-Identifier: Apache-2.0

package pgtest

import (
	"io"
	"net"
	"strconv"
	"sync"
	"testing"
)

// Proxy is a TCP proxy for outage tests (design section 11.3). Down resets every
// connection and refuses new ones; Up accepts again. It never pauses silently, because
// many drivers cannot bound a silent network pause.
type Proxy struct {
	target string
	ln     net.Listener

	mu    sync.Mutex
	down  bool
	conns map[net.Conn]struct{}
	wg    sync.WaitGroup
}

// NewProxy listens on a loopback port and forwards to host:port. It closes in
// t.Cleanup.
func NewProxy(t testing.TB, host string, port uint16) *Proxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	p := &Proxy{target: net.JoinHostPort(host, strconv.Itoa(int(port))), ln: ln, conns: map[net.Conn]struct{}{}}
	p.wg.Add(1)
	go p.accept()
	t.Cleanup(p.close)
	return p
}

// Addr returns the proxy's listening address.
func (p *Proxy) Addr() string {
	return p.ln.Addr().String()
}

// Down resets every open connection and refuses new ones until Up.
func (p *Proxy) Down() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.down = true
	for c := range p.conns {
		reset(c)
	}
	p.conns = map[net.Conn]struct{}{}
}

// Up accepts connections again.
func (p *Proxy) Up() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.down = false
}

func (p *Proxy) close() {
	_ = p.ln.Close()
	p.Down()
	p.wg.Wait()
}

func (p *Proxy) accept() {
	defer p.wg.Done()
	for {
		client, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.wg.Add(1)
		go p.serve(client)
	}
}

func (p *Proxy) serve(client net.Conn) {
	defer p.wg.Done()
	if !p.track(client) {
		reset(client)
		return
	}
	server, err := net.Dial("tcp", p.target)
	if err != nil {
		reset(client)
		return
	}
	if !p.track(server) {
		reset(client)
		reset(server)
		return
	}
	var once sync.Once
	closeBoth := func() {
		once.Do(func() {
			_ = client.Close()
			_ = server.Close()
		})
	}
	go func() {
		_, _ = io.Copy(server, client)
		closeBoth()
	}()
	_, _ = io.Copy(client, server)
	closeBoth()
	p.mu.Lock()
	delete(p.conns, client)
	delete(p.conns, server)
	p.mu.Unlock()
}

// track registers c unless the proxy is down.
func (p *Proxy) track(c net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.down {
		return false
	}
	p.conns[c] = struct{}{}
	return true
}

// reset closes c with an RST, so the peer sees a connection error at once.
func reset(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetLinger(0)
	}
	_ = c.Close()
}
