// Package proxy implements a proxy server.
package proxy

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"
)

var (
	// ErrProxyClosed is returned by the *Proxy.Serve and
	// *Proxy.ListenAndServe methods after a call to *Proxy.Close.
	ErrProxyClosed = errors.New("Proxy closed")

	// ErrProxyNotListening is returned by the *Proxy.Close method
	// if the listener has not been set yet or if it has been
	// already closed.
	ErrProxyNotListening = errors.New("Proxy is not listening")

	// ErrProxyListening is returned by the *Proxy.Serve and
	// *Proxy.ListenAndServe methods if the Proxy is already
	// listening.
	ErrProxyListening = errors.New("Proxy listening")
)

// Proxy represents a proxy server.
type Proxy struct {
	// ErrorLog specifies an optional logger for errors. If nil,
	// logging is done via the log package's standard logger.
	ErrorLog *log.Logger

	// BeforeAccept is called when the listener is ready, just
	// before accepting connections.
	BeforeAccept func() error

	inClose atomic.Bool

	mu       sync.Mutex
	conn     map[net.Conn]struct{}
	listener net.Listener

	listenerGroup sync.WaitGroup
}

// ListenAndServe listens on the specified "listen network address"
// and then calls [*Proxy.Serve] to start forwarding traffic to the
// provided "dial network address".
func (p *Proxy) ListenAndServe(listenNetwork, listenAddress, dialNetwork, dialAddress string) error {
	if p.closing() {
		return ErrProxyClosed
	}

	l, err := net.Listen(listenNetwork, listenAddress)
	if err != nil {
		return fmt.Errorf("proxy: listen %v:%v: %w", listenNetwork, listenAddress, err)
	}
	return p.Serve(l, dialNetwork, dialAddress)
}

// Serve forwards traffic to the specified "dial network address". It
// always returns a non-nil error and closes l.
func (p *Proxy) Serve(l net.Listener, dialNetwork, dialAddress string) error {
	if err := p.trackListener(l, true); err != nil {
		return err
	}
	defer p.trackListener(l, false) //nolint:errcheck

	if p.BeforeAccept != nil {
		if err := p.BeforeAccept(); err != nil {
			return err
		}
	}

	for {
		listenConn, err := l.Accept()
		if err != nil {
			if p.closing() {
				return ErrProxyClosed
			}
			return fmt.Errorf("proxy: accept %v: %w", l.Addr(), err)
		}
		go p.serve(listenConn, dialNetwork, dialAddress)
	}
}

func (p *Proxy) serve(listenConn net.Conn, dialNetwork, dialAddress string) {
	p.trackConn(listenConn, true)
	defer p.trackConn(listenConn, false)

	dialConn, err := net.Dial(dialNetwork, dialAddress)
	if err != nil {
		p.logf("proxy: error dialing %v:%v: %v", dialNetwork, dialAddress, err)
		listenConn.Close()
		return
	}
	p.trackConn(dialConn, true)
	defer p.trackConn(dialConn, false)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		io.Copy(dialConn, listenConn) //nolint:errcheck
		dialConn.Close()
		wg.Done()
	}()
	go func() {
		io.Copy(listenConn, dialConn) //nolint:errcheck
		listenConn.Close()
		wg.Done()
	}()
	wg.Wait()
}

// Addr returns the network address of the internal [net.Listener].
func (p *Proxy) Addr() net.Addr {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.listener == nil {
		return nil
	}

	return p.listener.Addr()
}

// Close closes the internal [net.Listener] and any active connection.
func (p *Proxy) Close() error {
	p.inClose.Store(true)

	err := p.closeListener()
	p.listenerGroup.Wait()
	p.closeConn()

	return err
}

func (p *Proxy) trackListener(l net.Listener, set bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if set {
		if p.closing() {
			return ErrProxyClosed
		}
		if p.listener != nil {
			return ErrProxyListening
		}
		p.listener = l
		p.listenerGroup.Add(1)
	} else {
		p.listener = nil
		p.listenerGroup.Done()
	}
	return nil
}

func (p *Proxy) closeListener() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.listener == nil {
		return ErrProxyNotListening
	}
	return p.listener.Close()
}

func (p *Proxy) trackConn(conn net.Conn, add bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.conn == nil {
		p.conn = make(map[net.Conn]struct{})
	}

	if add {
		p.conn[conn] = struct{}{}
	} else {
		delete(p.conn, conn)
	}
}

func (p *Proxy) closeConn() {
	p.mu.Lock()
	defer p.mu.Unlock()

	for c := range p.conn {
		c.Close()
		delete(p.conn, c)
	}
}

func (p *Proxy) closing() bool {
	return p.inClose.Load()
}

func (p *Proxy) logf(format string, args ...any) {
	if p.ErrorLog != nil {
		p.ErrorLog.Printf(format, args...)
	} else {
		log.Printf(format, args...)
	}
}
