package proxy

import (
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
)

var (
	// ErrGroupClosed is sent to the channel returned by the
	// *Group.ListenAndServe method after a call to *Group.Close.
	ErrGroupClosed = errors.New("Group closed")

	// ErrDuplicatedStream is returned by the
	// *Group.ListenAndServe method when it is called with
	// duplicated streams.
	ErrDuplicatedStream = errors.New("duplicated stream")
)

// Group represents a group of proxy servers.
type Group struct {
	// ErrorLog specifies an optional logger for errors. If nil,
	// logging is done via the log package's standard logger.
	ErrorLog *log.Logger

	inClose      atomic.Bool
	proxiesGroup sync.WaitGroup

	mu      sync.Mutex
	proxies map[Stream]*Proxy
}

// NewGroup initializes and returns a new [Group].
func NewGroup() *Group {
	return &Group{
		proxies: make(map[Stream]*Proxy),
	}
}

// ListenAndServe establishes the specified data streams. The returned
// [Event] channel can be used to receive events published by the
// [Proxy]'s. The returned error channel can be used to receive the
// errors coming from the [Group] and the [Proxy]'s.
func (pg *Group) ListenAndServe(streams ...Stream) (<-chan Event, <-chan error) {
	evc := make(chan Event)
	errc := make(chan error)

	if pg.closing() {
		go func() {
			errc <- ErrGroupClosed
			close(errc)
		}()
		return evc, errc
	}

	go func() {
		var wg sync.WaitGroup
		for _, stream := range streams {
			stream := stream
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := pg.handleStream(stream, evc); err != nil {
					errc <- err
				}
			}()
		}
		wg.Wait()

		if pg.closing() {
			errc <- ErrGroupClosed
		}

		close(errc)
	}()

	return evc, errc
}

func (pg *Group) handleStream(stream Stream, events chan<- Event) error {
	p := NewProxy()
	p.ErrorLog = pg.ErrorLog

	if err := pg.trackProxy(stream, p, true); err != nil {
		return err
	}
	defer pg.trackProxy(stream, p, false) //nolint:errcheck

	go func() {
		for ev := range p.Events() {
			events <- ev
		}
	}()

	if err := p.ListenAndServe(stream.ListenNetwork, stream.ListenAddr, stream.DialNetwork, stream.DialAddr); !errors.Is(err, ErrProxyClosed) {
		return err
	}
	return nil
}

func (pg *Group) trackProxy(stream Stream, p *Proxy, add bool) error {
	pg.mu.Lock()
	defer pg.mu.Unlock()

	if add {
		if pg.closing() {
			return ErrGroupClosed
		}

		_, dup := pg.proxies[stream]
		if dup {
			return ErrDuplicatedStream
		}

		pg.proxies[stream] = p
		pg.proxiesGroup.Add(1)
	} else {
		delete(pg.proxies, stream)
		pg.proxiesGroup.Done()
	}
	return nil
}

// Close closes all the established data streams.
func (pg *Group) Close() error {
	pg.inClose.Store(true)
	err := pg.closeProxies()
	pg.proxiesGroup.Wait()
	return err
}

func (pg *Group) closeProxies() error {
	pg.mu.Lock()
	defer pg.mu.Unlock()

	var errs []error
	for s, p := range pg.proxies {
		if err := p.Close(); err != nil {
			errs = append(errs, fmt.Errorf("error closing proxy: %v: %v", s, err))
		}
		delete(pg.proxies, s)
	}
	return errors.Join(errs...)
}

func (pg *Group) closing() bool {
	return pg.inClose.Load()
}
