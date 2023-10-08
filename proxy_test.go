package proxy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestProxy_ListenAndServe(t *testing.T) {
	want := "Test Response Body"

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, want)
	}))
	defer ts.Close()

	tsAddr := ts.Listener.Addr()
	p, addr, errc := newTestProxy(t, "tcp", "127.0.0.1:0", tsAddr.Network(), tsAddr.String())
	defer p.Flush()
	defer p.Close()

	resp, err := http.Get("http://" + addr)
	if err != nil {
		t.Fatalf("HTTP GET request error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status code: got: %v, want: 200", resp.StatusCode)
	}

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("error reading response body: %v", err)
	}

	if !bytes.Equal(got, []byte(want)) {
		t.Errorf("unexpected response: got: %s, want: %s", got, want)
	}

	if err := p.Close(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	if err := <-errc; !errors.Is(err, ErrProxyClosed) {
		t.Errorf("unexpected error: got: %v, want: %v", err, ErrProxyClosed)
	}
}

func TestProxy_ListenAndServe_twice(t *testing.T) {
	p, _, errc := newTestProxy(t, "tcp", "127.0.0.1:0", "tcp", "127.0.0.1:1234")
	defer p.Flush()
	defer p.Close()

	if err := p.ListenAndServe("tcp", "127.0.0.1:0", "tcp", "127.0.0.1:1234"); !errors.Is(err, ErrProxyListener) {
		t.Errorf("unexpected error: got: %v, want: %v", err, ErrProxyListener)
	}

	if err := p.Close(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	if err := <-errc; !errors.Is(err, ErrProxyClosed) {
		t.Errorf("unexpected error: got: %v, want: %v", err, ErrProxyClosed)
	}
}

func TestProxy_ListenAndServe_after_close(t *testing.T) {
	p, _, errc := newTestProxy(t, "tcp", "127.0.0.1:0", "tcp", "127.0.0.1:1234")
	defer p.Flush()

	if err := p.Close(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	if err := <-errc; !errors.Is(err, ErrProxyClosed) {
		t.Errorf("unexpected error: got: %v, want: %v", err, ErrProxyClosed)
	}

	if err := p.ListenAndServe("tcp", "127.0.0.1:0", "tcp", "127.0.0.1:1234"); !errors.Is(err, ErrProxyClosed) {
		t.Errorf("unexpected error: got: %v, want: %v", err, ErrProxyClosed)
	}
}

func TestProxy_ListenAndServe_after_close_without_listening(t *testing.T) {
	p := NewProxy()
	defer p.Flush()

	if err := p.Close(); !errors.Is(err, ErrProxyListener) {
		t.Errorf("unexpected error: got: %v, want: %v", err, ErrProxyListener)
	}

	if err := p.ListenAndServe("tcp", "127.0.0.1:0", "tcp", "127.0.0.1:1234"); !errors.Is(err, ErrProxyClosed) {
		t.Errorf("unexpected error: got: %v, want: %v", err, ErrProxyClosed)
	}
}

func TestProxy_Serve_after_close(t *testing.T) {
	p, _, errc := newTestProxy(t, "tcp", "127.0.0.1:0", "tcp", "127.0.0.1:1234")
	defer p.Flush()

	if err := p.Close(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	if err := <-errc; !errors.Is(err, ErrProxyClosed) {
		t.Errorf("unexpected error: got: %v, want: %v", err, ErrProxyClosed)
	}

	if err := p.Serve(nil, "tcp", "127.0.0.1:1234"); !errors.Is(err, ErrProxyClosed) {
		t.Errorf("unexpected error: got: %v, want: %v", err, ErrProxyClosed)
	}
}

func TestProxy_Close_without_listening(t *testing.T) {
	p := NewProxy()
	defer p.Flush()

	if err := p.Close(); err != ErrProxyListener {
		t.Errorf("unexpected error: got: %v, want: %v", err, ErrProxyListener)
	}
}

func TestProxy_Close_twice(t *testing.T) {
	p, _, errc := newTestProxy(t, "tcp", "127.0.0.1:0", "tcp", "127.0.0.1:1234")
	defer p.Flush()

	if err := p.Close(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	if err := <-errc; !errors.Is(err, ErrProxyClosed) {
		t.Errorf("unexpected error: got: %v, want: %v", err, ErrProxyClosed)
	}

	if err := p.Close(); !errors.Is(err, ErrProxyClosed) {
		t.Errorf("unexpected error: got: %v, want: %v", err, ErrProxyClosed)
	}
}

func TestProxy_Close_twice_without_listening(t *testing.T) {
	p := NewProxy()
	defer p.Flush()

	if err := p.Close(); !errors.Is(err, ErrProxyListener) {
		t.Errorf("unexpected error: got: %v, want: %v", err, ErrProxyListener)
	}

	if err := p.Close(); !errors.Is(err, ErrProxyClosed) {
		t.Errorf("unexpected error: got: %v, want: %v", err, ErrProxyClosed)
	}
}

func TestProxy_Close_close_events_chan(t *testing.T) {
	p, _, errc := newTestProxyEvents(t, "tcp", "127.0.0.1:0")

	if err := p.Close(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	if err := <-errc; !errors.Is(err, ErrProxyClosed) {
		t.Errorf("unexpected error: got: %v, want: %v", err, ErrProxyClosed)
	}

	var n int
	for ev := range p.Events() {
		if ev.Kind != KindBeforeAccept {
			t.Errorf("unexpected event: got: %v, want: %v", ev.Kind, KindBeforeAccept)
		}
		n++
	}

	if n != 1 {
		t.Errorf("received multiple events: %v", n)
	}
}

func TestProxy_Flush(t *testing.T) {
	p, _, errc := newTestProxyEvents(t, "tcp", "127.0.0.1:0")

	if err := p.Close(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	if err := <-errc; !errors.Is(err, ErrProxyClosed) {
		t.Errorf("unexpected error: got: %v, want: %v", err, ErrProxyClosed)
	}

	p.Flush()

	if _, ok := <-p.Events(); ok {
		t.Errorf("events channel should be closed")
	}
}

func TestParseStream(t *testing.T) {
	tests := []struct {
		name       string
		stream     string
		want       Stream
		wantNilErr bool
	}{
		{
			name:   "valid",
			stream: "tcp::1234,unix:/path/to/socket",
			want: Stream{
				ListenNetwork: "tcp",
				ListenAddr:    ":1234",
				DialNetwork:   "unix",
				DialAddr:      "/path/to/socket",
			},
			wantNilErr: true,
		},
		{
			name:       "malformed",
			stream:     "tcp::1234:unix:/path/to/socket",
			want:       Stream{},
			wantNilErr: false,
		},
		{
			name:       "empty listener",
			stream:     ",unix:/path/to/socket",
			want:       Stream{},
			wantNilErr: false,
		},
		{
			name:       "empty target",
			stream:     "tcp::1234,",
			want:       Stream{},
			wantNilErr: false,
		},
		{
			name:       "empty network",
			stream:     "::1234,unix:/path/to/socket",
			want:       Stream{},
			wantNilErr: false,
		},
		{
			name:       "empty address",
			stream:     "tcp:,unix:/path/to/socket",
			want:       Stream{},
			wantNilErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseStream(tt.stream)

			if got != tt.want {
				t.Errorf("unexpected stream: got: %v, want: %v", got, tt.want)
			}

			if (err == nil) != tt.wantNilErr {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func newTestProxy(t *testing.T, listenNetwork, listenAddr, dialNetwork, dialAddr string) (p *Proxy, addr string, errs <-chan error) {
	ln, err := net.Listen(listenNetwork, listenAddr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	p = NewProxy()

	errc := make(chan error)
	go func() {
		errc <- p.Serve(ln, dialNetwork, dialAddr)
		close(errc)
	}()

loop:
	for {
		select {
		case ev := <-p.Events():
			if ev.Kind == KindBeforeAccept {
				break loop
			}
		case err := <-errc:
			p.Close()
			p.Flush()
			t.Fatalf("unexpected error: %v", err)
		}
	}

	return p, ln.Addr().String(), errc
}

func newTestProxyEvents(t *testing.T, listenNetwork, listenAddr string) (p *Proxy, addr string, errs <-chan error) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	p = NewProxy()

	errc := make(chan error)
	go func() {
		errc <- p.Serve(ln, ts.Listener.Addr().Network(), ts.Listener.Addr().String())
		close(errc)
	}()

	cli := &http.Client{Timeout: 5 * time.Millisecond}
loop:
	for {
		select {
		case <-time.After(5 * time.Millisecond):
			if _, err := cli.Get("http://" + ln.Addr().String()); err == nil {
				break loop
			}
		case err := <-errc:
			p.Close()
			p.Flush()
			t.Fatalf("unexpected error: %v", err)
		}
	}

	return p, ln.Addr().String(), errc
}
