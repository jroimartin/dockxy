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
	// This error is also returned by *Group.Close if the Group
	// has been closed.
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

	inClose atomic.Bool

	// mu enforces the following:
	//   - *Group.Close cannot be called twice.
	//   - *Group.ListenAndServe cannot be called after
	//     *Group.Close is called.
	//   - No more stream batches are sent on batches after
	//     *Proxy.Close is called.
	//   - No more streams are handled after *Proxy.Close is
	//     called.
	mu         sync.Mutex
	batches    chan Batch
	batchGroup sync.WaitGroup
	proxies    map[Stream]*Proxy
}

// NewGroup initializes and returns a new [Group].
func NewGroup() *Group {
	pg := &Group{
		batches: make(chan Batch),
		proxies: make(map[Stream]*Proxy),
	}

	go pg.handleBatches()

	return pg
}

// handleBatches runs in its own goroutine and handles the stream
// batches created by the calls to [*Group.ListenAndServe].
func (pg *Group) handleBatches() {
	for batch := range pg.batches {
		batch := batch
		go pg.handleBatch(batch)
	}
}

// handleBatch handle a single stream batch.
func (pg *Group) handleBatch(batch Batch) {
	var streamGroup sync.WaitGroup
	for _, stream := range batch.streams {
		stream := stream
		streamGroup.Add(1)
		go func() {
			defer streamGroup.Done()
			var eventGroup sync.WaitGroup
			if err := pg.handleStream(stream, batch.evc, &eventGroup); err != nil {
				batch.errc <- err
			}
			eventGroup.Wait()
		}()
	}
	streamGroup.Wait()

	if pg.closing() {
		batch.errc <- ErrGroupClosed
	}

	close(batch.evc)
	close(batch.errc)
}

// handleStream handles a stream.
func (pg *Group) handleStream(stream Stream, evc chan<- Event, eventGroup *sync.WaitGroup) error {
	p := NewProxy()
	p.ErrorLog = pg.ErrorLog
	defer p.Close()

	if err := pg.trackProxy(stream, p, true); err != nil {
		return err
	}
	defer pg.trackProxy(stream, p, false) //nolint:errcheck

	eventGroup.Add(1)
	go func() {
		defer eventGroup.Done()
		for ev := range p.Events() {
			evc <- ev
		}
	}()

	if err := p.ListenAndServe(stream.ListenNetwork, stream.ListenAddr, stream.DialNetwork, stream.DialAddr); !errors.Is(err, ErrProxyClosed) {
		return fmt.Errorf("proxy listen and serve: %w", err)
	}
	return nil
}

// trackProxy tracks the specified proxy. If add is true, it is added
// to the proxy list. Otherwise, it is removed from the proxy list.
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
	} else {
		delete(pg.proxies, stream)
	}
	return nil
}

// ListenAndServe establishes the specified data streams. It returns a
// [Batch] that can be used to receive events and errors coming from
// the [Group] and the [Proxy]'s that handle the provided data
// streams.
func (pg *Group) ListenAndServe(streams ...Stream) Batch {
	pg.mu.Lock()
	defer pg.mu.Unlock()

	evc := make(chan Event)
	errc := make(chan error)

	batch := Batch{streams: streams, evc: evc, errc: errc}

	if pg.closing() {
		go func() {
			errc <- ErrGroupClosed
			close(evc)
			close(errc)
		}()
		return batch
	}

	pg.batchGroup.Add(1)
	go func() {
		defer pg.batchGroup.Done()
		pg.batches <- batch
	}()

	return batch
}

// Close closes all the established data streams. After calling
// [*Group.Close] all the events and errors related to the [Batch]'s
// returned by successive calls to [*Group.ListenAndServe] should be
// consumed to avoid leaking resources. [Batch.Flush] is a helper for
// this.
func (pg *Group) Close() error {
	pg.mu.Lock()
	defer pg.mu.Unlock()

	if pg.closing() {
		return ErrGroupClosed
	}

	pg.inClose.Store(true)

	pg.batchGroup.Wait()
	close(pg.batches)

	return pg.closeProxies()
}

// closeProxies closes all the proxies. This function is called from
// [*Group.Close], so it expects Group.mu to be held.
func (pg *Group) closeProxies() error {
	var errs []error
	for s, p := range pg.proxies {
		if err := p.Close(); err != nil {
			errs = append(errs, fmt.Errorf("error closing proxy: %v: %v", s, err))
		}
		delete(pg.proxies, s)
	}
	return errors.Join(errs...)
}

// closing returns whether the Group has been closed.
func (pg *Group) closing() bool {
	return pg.inClose.Load()
}

// A Batch is a group of streams created by a call to
// [*Group.ListenAndServe].
type Batch struct {
	streams []Stream
	evc     chan Event
	errc    chan error
}

// Events returns a channel that can be used to receive events related
// to the [Batch].
func (b Batch) Events() <-chan Event {
	return b.evc
}

// Errors returns a channel that can be used to receive errors related
// to the [Batch].
func (b Batch) Errors() <-chan error {
	return b.errc
}

// Flush discards all the events and errors related to the [Batch].
func (b Batch) Flush() {
	for {
		select {
		case _, ok := <-b.errc:
			if !ok {
				return
			}
		case _, ok := <-b.evc:
			if !ok {
				return
			}
		}
	}
}
