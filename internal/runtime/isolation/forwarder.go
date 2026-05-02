package isolation

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
)

// Forwarder is a TCP relay from a local listener to a fixed upstream address.
// Used to bridge in-netns workload traffic to clawvisor endpoints (proxy + API)
// because the workload's iptables only permits the bridge gateway IP.
type Forwarder struct {
	target   string
	listener net.Listener
	addr     string
	wg       sync.WaitGroup
	closed   atomic.Bool
}

// StartForwarder binds a TCP listener at bindAddr (host:port; port may be 0)
// and forwards every accepted connection to target. The returned Forwarder
// must be Closed to release the listener and stop accept loops.
func StartForwarder(ctx context.Context, bindAddr, target string) (*Forwarder, error) {
	if bindAddr == "" {
		return nil, fmt.Errorf("forwarder: bind address required")
	}
	if target == "" {
		return nil, fmt.Errorf("forwarder: target address required")
	}
	lc := net.ListenConfig{}
	ln, err := lc.Listen(ctx, "tcp", bindAddr)
	if err != nil {
		return nil, fmt.Errorf("forwarder: listen %s: %w", bindAddr, err)
	}
	f := &Forwarder{
		target:   target,
		listener: ln,
		addr:     ln.Addr().String(),
	}
	f.wg.Add(1)
	go f.acceptLoop()
	return f, nil
}

// Addr returns the actual host:port the forwarder is bound to.
func (f *Forwarder) Addr() string { return f.addr }

// Port returns the actual port the forwarder is bound to.
func (f *Forwarder) Port() int {
	if tcp, ok := f.listener.Addr().(*net.TCPAddr); ok {
		return tcp.Port
	}
	return 0
}

// Close stops the forwarder and releases its listener.
func (f *Forwarder) Close() error {
	if !f.closed.CompareAndSwap(false, true) {
		return nil
	}
	err := f.listener.Close()
	f.wg.Wait()
	return err
}

func (f *Forwarder) acceptLoop() {
	defer f.wg.Done()
	for {
		conn, err := f.listener.Accept()
		if err != nil {
			if f.closed.Load() {
				return
			}
			return
		}
		go f.handle(conn)
	}
}

func (f *Forwarder) handle(client net.Conn) {
	defer client.Close()
	upstream, err := net.Dial("tcp", f.target)
	if err != nil {
		return
	}
	defer upstream.Close()
	var wg sync.WaitGroup
	wg.Add(2)
	go pipe(&wg, upstream, client)
	go pipe(&wg, client, upstream)
	wg.Wait()
}

func pipe(wg *sync.WaitGroup, dst, src net.Conn) {
	defer wg.Done()
	_, _ = io.Copy(dst, src)
	if cw, ok := dst.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}
