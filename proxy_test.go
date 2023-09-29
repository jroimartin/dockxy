package proxy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestProxy_ListenAndServe(t *testing.T) {
	want := "Test Response Body"

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, want)
	}))
	defer ts.Close()

	tsAddr := ts.Listener.Addr()
	p, errc := newTestProxy("tcp", "127.0.0.1:0", tsAddr.Network(), tsAddr.String())

	resp, err := http.Get("http://" + p.Addr().String())
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
	p, errc := newTestProxy("tcp", "127.0.0.1:0", "tcp", "127.0.0.1:1234")

	if err := p.ListenAndServe("tcp", "127.0.0.1:0", "tcp", "127.0.0.1:1234"); !errors.Is(err, ErrProxyListening) {
		t.Errorf("unexpected error: got: %v, want: %v", err, ErrProxyListening)
	}

	if err := p.Close(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	if err := <-errc; !errors.Is(err, ErrProxyClosed) {
		t.Errorf("unexpected error: got: %v, want: %v", err, ErrProxyClosed)
	}
}

func TestProxy_ListenAndServe_after_close(t *testing.T) {
	p, errc := newTestProxy("tcp", "127.0.0.1:0", "tcp", "127.0.0.1:1234")

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
	p := &Proxy{}

	if err := p.Close(); err != ErrProxyNotListening {
		t.Errorf("unexpected error: got: %v, want: %v", err, ErrProxyNotListening)
	}

	if err := p.ListenAndServe("tcp", "127.0.0.1:0", "tcp", "127.0.0.1:1234"); !errors.Is(err, ErrProxyClosed) {
		t.Errorf("unexpected error: got: %v, want: %v", err, ErrProxyClosed)
	}
}

func TestProxy_Serve_after_close(t *testing.T) {
	p, errc := newTestProxy("tcp", "127.0.0.1:0", "tcp", "127.0.0.1:1234")

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
	p := &Proxy{}

	if err := p.Close(); err != ErrProxyNotListening {
		t.Errorf("unexpected error: got: %v, want: %v", err, ErrProxyNotListening)
	}
}

func TestProxy_Close_twice(t *testing.T) {
	p, errc := newTestProxy("tcp", "127.0.0.1:0", "tcp", "127.0.0.1:1234")

	if err := p.Close(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	if err := <-errc; !errors.Is(err, ErrProxyClosed) {
		t.Errorf("unexpected error: got: %v, want: %v", err, ErrProxyClosed)
	}

	if err := p.Close(); !errors.Is(err, ErrProxyNotListening) {
		t.Errorf("unexpected error: got: %v, want: %v", err, ErrProxyNotListening)
	}
}

func TestProxy_BeforeAccept(t *testing.T) {
	wantErr := errors.New("BeforeAccept error")

	p := &Proxy{}
	p.BeforeAccept = func() error {
		return wantErr
	}

	if err := p.ListenAndServe("tcp", "127.0.0.1:0", "tcp", "127.0.0.1:1234"); !errors.Is(err, wantErr) {
		t.Errorf("unexpected error: got: %v, want: %v", err, wantErr)
	}

	if err := p.Close(); !errors.Is(err, ErrProxyNotListening) {
		t.Errorf("unexpected error: got: %v, want: %v", err, ErrProxyNotListening)
	}
}

func TestProxy_Addr(t *testing.T) {
	p := &Proxy{}

	if addr := p.Addr(); addr != nil {
		t.Errorf("Unexpected address: %v", addr)
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen error: %v", err)
	}
	want := l.Addr()

	var wg sync.WaitGroup
	p.BeforeAccept = func() error {
		wg.Done()
		return nil
	}

	errc := make(chan error)
	wg.Add(1)
	go func() { errc <- p.Serve(l, "tcp", "127.0.0.1:1234") }()
	wg.Wait()

	if got := p.Addr(); got != want {
		t.Errorf("unexpected address: got: %v, want: %v", got, want)
	}

	if err := p.Close(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	if addr := p.Addr(); addr != nil {
		t.Errorf("Unexpected address: %v", addr)
	}

	if err := <-errc; !errors.Is(err, ErrProxyClosed) {
		t.Errorf("unexpected error: got: %v, want: %v", err, ErrProxyClosed)
	}
}

func newTestProxy(listenNetwork, listenAddr, dialNetwork, dialAddr string) (*Proxy, <-chan error) {
	p := &Proxy{}

	var wg sync.WaitGroup
	p.BeforeAccept = func() error {
		wg.Done()
		return nil
	}

	errc := make(chan error)
	wg.Add(1)
	go func() { errc <- p.ListenAndServe(listenNetwork, listenAddr, dialNetwork, dialAddr) }()
	wg.Wait()

	return p, errc
}
