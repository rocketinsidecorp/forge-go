package proxy

import (
	"net"
	"sync"
)

// Defaults of the run flags that bound what one client can make the proxy
// hold. They match Forge-Rust's forge-proxy, which caps a listener at 10,000
// connections.
const (
	// DefaultMaxHeaderBytes is the -max-header-bytes default: 64 KiB of
	// request line and headers (net/http's own default is 1 MiB).
	DefaultMaxHeaderBytes = 64 << 10
	// MinMaxHeaderBytes and MaxMaxHeaderBytes bound -max-header-bytes: below
	// 4 KiB ordinary cookies would be refused, above 1 MiB the limit
	// protects nothing.
	MinMaxHeaderBytes = 4 << 10
	MaxMaxHeaderBytes = 1 << 20
	// DefaultMaxConnections is the -max-connections default, per listener.
	DefaultMaxConnections = 10_000
)

// limitListener admits at most n open connections at a time. Admission
// happens before Accept, so while every slot is taken further clients wait
// in the kernel's listen backlog instead of becoming goroutines and file
// descriptors; a slot is freed when its connection closes. (x/net's
// netutil.LimitListener does the same; it is not a dependency of Forge.)
type limitListener struct {
	net.Listener
	slots chan struct{}
	done  chan struct{}
	once  sync.Once
}

func newLimitListener(l net.Listener, n int) *limitListener {
	return &limitListener{Listener: l, slots: make(chan struct{}, n), done: make(chan struct{})}
}

func (l *limitListener) Accept() (net.Conn, error) {
	select {
	case l.slots <- struct{}{}:
	case <-l.done:
		return nil, net.ErrClosed
	}
	conn, err := l.Listener.Accept()
	if err != nil {
		<-l.slots
		return nil, err
	}
	return &limitConn{Conn: conn, release: func() { <-l.slots }}, nil
}

func (l *limitListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return l.Listener.Close()
}

type limitConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *limitConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}
