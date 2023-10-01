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
)

func TestProxy_ListenAndServe(t *testing.T) {
	want := "Test Response Body"

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, want)
	}))
	defer ts.Close()

	tsAddr := ts.Listener.Addr()
	p, addr, errc := newTestProxy("tcp", "127.0.0.1:0", tsAddr.Network(), tsAddr.String())

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
	p, _, errc := newTestProxy("tcp", "127.0.0.1:0", "tcp", "127.0.0.1:1234")

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
	p, _, errc := newTestProxy("tcp", "127.0.0.1:0", "tcp", "127.0.0.1:1234")

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

	if err := p.Close(); err != ErrProxyListener {
		t.Errorf("unexpected error: got: %v, want: %v", err, ErrProxyListener)
	}

	if err := p.ListenAndServe("tcp", "127.0.0.1:0", "tcp", "127.0.0.1:1234"); !errors.Is(err, ErrProxyClosed) {
		t.Errorf("unexpected error: got: %v, want: %v", err, ErrProxyClosed)
	}
}

func TestProxy_Serve_after_close(t *testing.T) {
	p, _, errc := newTestProxy("tcp", "127.0.0.1:0", "tcp", "127.0.0.1:1234")

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

	if err := p.Close(); err != ErrProxyListener {
		t.Errorf("unexpected error: got: %v, want: %v", err, ErrProxyListener)
	}
}

func TestProxy_Close_twice(t *testing.T) {
	p, _, errc := newTestProxy("tcp", "127.0.0.1:0", "tcp", "127.0.0.1:1234")

	if err := p.Close(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	if err := <-errc; !errors.Is(err, ErrProxyClosed) {
		t.Errorf("unexpected error: got: %v, want: %v", err, ErrProxyClosed)
	}

	if err := p.Close(); !errors.Is(err, ErrProxyListener) {
		t.Errorf("unexpected error: got: %v, want: %v", err, ErrProxyListener)
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

func newTestProxy(listenNetwork, listenAddr, dialNetwork, dialAddr string) (p *Proxy, addr string, errs <-chan error) {
	errc := make(chan error)

	ln, err := net.Listen(listenNetwork, listenAddr)
	if err != nil {
		go func() { errc <- err }()
		return nil, "", errc
	}

	p = NewProxy()
	go func() { errc <- p.Serve(ln, dialNetwork, dialAddr) }()

	waitBeforeAccept(p.Events(), 1)

	return p, ln.Addr().String(), errc
}

func waitBeforeAccept(events <-chan Event, n int) {
	for ev := range events {
		if ev.Kind == KindBeforeAccept {
			n--
		}
		if n == 0 {
			break
		}
	}
}
