// Package proxy implements a proxy server.
package proxy

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"sync/atomic"
)

var (
	// ErrProxyClosed is returned by the *Proxy.Serve and
	// *Proxy.ListenAndServe methods after a call to *Proxy.Close.
	ErrProxyClosed = errors.New("Proxy closed")

	// ErrProxyListener is returned in the following
	// circumstances:
	//   - *Proxy.Close is called before the listener has been set
	//     or after it has been closed.
	//   - *Proxy.Serve or *Proxy.ListenAndServe is called after
	//     the Proxy is already listening.
	ErrProxyListener = errors.New("Proxy listener error")
)

// Proxy represents a proxy server.
type Proxy struct {
	// ErrorLog specifies an optional logger for errors. If nil,
	// logging is done via the log package's standard logger.
	ErrorLog *log.Logger

	inClose       atomic.Bool
	listenerGroup sync.WaitGroup
	evc           chan Event

	mu       sync.Mutex
	conn     map[net.Conn]struct{}
	listener net.Listener
}

// NewProxy initializes and returns a new [Proxy].
func NewProxy() *Proxy {
	return &Proxy{
		conn: make(map[net.Conn]struct{}),
		evc:  make(chan Event),
	}
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

	p.sendEvent(Event{
		Kind: KindBeforeAccept,
		Data: Stream{
			ListenNetwork: l.Addr().Network(),
			ListenAddr:    l.Addr().String(),
			DialNetwork:   dialNetwork,
			DialAddr:      dialAddress,
		},
	})

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
		defer wg.Done()
		io.Copy(dialConn, listenConn) //nolint:errcheck
		dialConn.Close()
	}()
	go func() {
		defer wg.Done()
		io.Copy(listenConn, dialConn) //nolint:errcheck
		listenConn.Close()
	}()
	wg.Wait()
}

// Events returns an [Event] channel that can be used to receive
// events published by the [Proxy].
func (p *Proxy) Events() <-chan Event {
	return p.evc
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
			return ErrProxyListener
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
		return ErrProxyListener
	}
	return p.listener.Close()
}

func (p *Proxy) trackConn(conn net.Conn, add bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

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

func (p *Proxy) sendEvent(ev Event) {
	go func() { p.evc <- ev }()
}

func (p *Proxy) logf(format string, args ...any) {
	if p.ErrorLog != nil {
		p.ErrorLog.Printf(format, args...)
	} else {
		log.Printf(format, args...)
	}
}

// An Event represents an update in the internal state of the proxy.
// The type of the Data field depends on the kind of event.
type Event struct {
	Kind Kind
	Data any
}

// Kind is the kind of [Event].
type Kind int

const (
	// KindBeforeAccept refers to the event sent just before the
	// Proxy starts accepting connections. The Event.Data field
	// contains the related Stream.
	KindBeforeAccept Kind = iota
)

// Stream represents a bidirectional data stream.
type Stream struct {
	// ListenNetwork is the name of the network of the listener.
	ListenNetwork string

	// ListenAddr is the address of the listener.
	ListenAddr string

	// DialNetwork is the name of the dialed network.
	DialNetwork string

	// DialAddr is the dialed address.
	DialAddr string
}

// ParseStream parses a string representing a bidirectional data
// stream with the format:
//
//	<listen network>:<listen address>,<dial network>:<dial address>".
func ParseStream(s string) (Stream, error) {
	sides := strings.Split(s, ",")
	if len(sides) != 2 {
		return Stream{}, fmt.Errorf("malformed stream %q", s)
	}

	listenNetwork, listenAddr, err := parseAddr(sides[0])
	if err != nil {
		return Stream{}, fmt.Errorf("malformed listen side %q: %w", sides[0], err)
	}

	dialNetwork, dialAddr, err := parseAddr(sides[1])
	if err != nil {
		return Stream{}, fmt.Errorf("malformed dial side %q: %w", sides[1], err)
	}

	stream := Stream{
		ListenNetwork: listenNetwork,
		ListenAddr:    listenAddr,
		DialNetwork:   dialNetwork,
		DialAddr:      dialAddr,
	}

	return stream, nil
}

func parseAddr(s string) (network, addr string, err error) {
	i := strings.Index(s, ":")
	if i < 0 {
		return "", "", fmt.Errorf("malformed address")
	}

	network = s[:i]
	if network == "" {
		return "", "", errors.New("empty network")
	}

	addr = s[i+1:]
	if addr == "" {
		return "", "", errors.New("empty address")
	}

	return network, addr, nil
}

// String returns the string representation of the stream.
func (stream Stream) String() string {
	return fmt.Sprintf("%v:%v,%v:%v", stream.ListenNetwork, stream.ListenAddr, stream.DialNetwork, stream.DialAddr)
}
