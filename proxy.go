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
	// This error is also returned by *Proxy.Close if the Proxy
	// has been closed.
	ErrProxyClosed = errors.New("Proxy closed")

	// ErrProxyListener is returned by the *Proxy.Serve or
	// *Proxy.ListenAndServe methods when they are called after
	// the Proxy is already listening. This error is also returned
	// by *Proxy.Close if the listener has not been set.
	ErrProxyListener = errors.New("Proxy listener error")
)

// Proxy represents a proxy server.
type Proxy struct {
	// ErrorLog specifies an optional logger for errors. If nil,
	// logging is done via the log package's standard logger.
	ErrorLog *log.Logger

	inClose atomic.Bool

	// mu enforces the following:
	//   - *Proxy.Close cannot be called twice.
	//   - *Proxy.setListener cannot be called twice or after
	//     *Proxy.Close is called.
	//   - *Proxy.ListenAndServe and *Proxy.Serve cannot be called
	//     twice (the listener cannot be set again) or after
	//     *Proxy.Close is called.
	//   - No more events are sent on evc after *Proxy.Close is
	//     called.
	//   - No more connections are handled after *Proxy.Close is
	//     called.
	mu          sync.Mutex
	listener    net.Listener
	listenGroup sync.WaitGroup
	evc         chan Event
	eventGroup  sync.WaitGroup
	conn        map[net.Conn]struct{}
}

// NewProxy initializes and returns a new [Proxy].
func NewProxy() *Proxy {
	return &Proxy{
		evc:  make(chan Event),
		conn: make(map[net.Conn]struct{}),
	}
}

// ListenAndServe listens on the specified "listen network address"
// and then calls [*Proxy.Serve] to start forwarding traffic to the
// provided "dial network address".
func (p *Proxy) ListenAndServe(listenNetwork, listenAddress, dialNetwork, dialAddress string) error {
	l, err := net.Listen(listenNetwork, listenAddress)
	if err != nil {
		return fmt.Errorf("listen %v:%v: %w", listenNetwork, listenAddress, err)
	}
	return p.Serve(l, dialNetwork, dialAddress)
}

// Serve forwards traffic to the specified "dial network address". It
// always returns a non-nil error and closes l.
func (p *Proxy) Serve(l net.Listener, dialNetwork, dialAddress string) error {
	if err := p.setListener(l); err != nil {
		return fmt.Errorf("set listener: %w", err)
	}
	defer p.listenGroup.Done()

	go p.sendEvent(Event{
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
			return fmt.Errorf("accept %v: %w", l.Addr(), err)
		}
		go p.serve(listenConn, dialNetwork, dialAddress)
	}
}

// setListener sets the listener.
func (p *Proxy) setListener(l net.Listener) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closing() {
		return ErrProxyClosed
	}

	if p.listener != nil {
		return ErrProxyListener
	}

	p.listenGroup.Add(1)
	p.listener = l
	return nil
}

// serve handles a connection.
func (p *Proxy) serve(listenConn net.Conn, dialNetwork, dialAddress string) {
	defer listenConn.Close()

	if err := p.trackConn(listenConn, true); err != nil {
		return
	}
	defer p.trackConn(listenConn, false) //nolint:errcheck

	dialConn, err := net.Dial(dialNetwork, dialAddress)
	if err != nil {
		p.logf("proxy: error dialing %v:%v: %v", dialNetwork, dialAddress, err)
		return
	}
	defer dialConn.Close()

	if err := p.trackConn(dialConn, true); err != nil {
		return
	}
	defer p.trackConn(dialConn, false) //nolint:errcheck

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

// trackConn tracks the specified connection. If add is true, it is
// added to the connection list. Otherwise, it is removed from the
// connection list.
func (p *Proxy) trackConn(conn net.Conn, add bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if add {
		if p.closing() {
			return ErrProxyClosed
		}
		p.conn[conn] = struct{}{}
	} else {
		delete(p.conn, conn)
	}
	return nil
}

// Events returns an [Event] channel that can be used to receive
// events published by the [Proxy].
func (p *Proxy) Events() <-chan Event {
	return p.evc
}

// Close closes the internal [net.Listener] and any active connection.
// After calling [*Proxy.Close] all the events in the channel returned
// by [*Proxy.Events] should be consumed to avoid leaking resources.
// [*Proxy.Flush] is a helper for this.
func (p *Proxy) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closing() {
		return ErrProxyClosed
	}

	p.inClose.Store(true)

	err := p.closeListener()
	p.listenGroup.Wait()
	p.closeConn()

	// There can be events waiting for being consumed. So, create
	// a goroutine to wait until all of them are sent before
	// closing the channel.
	go p.closeEvents()

	return err
}

// closeListener closes the listener if it has been set. This function
// is called from [*Proxy.Close], so it expects Proxy.mu to be held.
func (p *Proxy) closeListener() error {
	if p.listener == nil {
		return ErrProxyListener
	}
	return p.listener.Close()
}

// closeConn closes all the connections and deletes them from the
// connection list. This function is called from [*Proxy.Close], so it
// expects Proxy.mu to be held.
func (p *Proxy) closeConn() {
	for c := range p.conn {
		c.Close()
		delete(p.conn, c)
	}
}

// closeEvents closes de events channel after all the events have been
// consumed.
func (p *Proxy) closeEvents() {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.eventGroup.Wait()
	close(p.evc)
}

// closing returns whether the proxy has been closed.
func (p *Proxy) closing() bool {
	return p.inClose.Load()
}

// sendEvent sends an event on the events channel.
func (p *Proxy) sendEvent(ev Event) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closing() {
		return
	}

	p.eventGroup.Add(1)
	go func() {
		defer p.eventGroup.Done()
		p.evc <- ev
	}()
}

// Flush discards all the events in the channel returned by
// [*Proxy.Events].
func (p *Proxy) Flush() {
	for {
		if _, ok := <-p.evc; !ok {
			return
		}
	}
}

// logf logs the specified message using Proxy.ErrorLog.
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
		return Stream{}, fmt.Errorf("malformed stream: %v", s)
	}

	listenNetwork, listenAddr, err := parseStreamSide(sides[0])
	if err != nil {
		return Stream{}, fmt.Errorf("malformed listen side %q: %w", sides[0], err)
	}

	dialNetwork, dialAddr, err := parseStreamSide(sides[1])
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

// parseStreamSide parses one side of a stream string.
func parseStreamSide(s string) (network, addr string, err error) {
	i := strings.Index(s, ":")
	if i < 0 {
		return "", "", errors.New("malformed address")
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
