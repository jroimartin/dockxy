package proxy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

var discardLogger = log.New(io.Discard, "", 0)

func TestProxyListenAndServe(t *testing.T) {
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
		t.Errorf("unexpected reponse: got: %s, want: %s", got, want)
	}

	if err := p.Close(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	if err := <-errc; !errors.Is(err, ErrProxyClosed) {
		t.Errorf("unexpected error: got: %v, want: %v", err, ErrProxyClosed)
	}
}

func TestProxyListenAndServe_twice(t *testing.T) {
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

func TestProxyListenAndServe_after_close(t *testing.T) {
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

func TestProxyListenAndServe_after_close_without_listening(t *testing.T) {
	p := &Proxy{ErrorLog: discardLogger}

	if err := p.Close(); err != ErrProxyNotListening {
		t.Errorf("unexpected error: got: %v, want: %v", err, ErrProxyNotListening)
	}

	if err := p.ListenAndServe("tcp", "127.0.0.1:0", "tcp", "127.0.0.1:1234"); !errors.Is(err, ErrProxyClosed) {
		t.Errorf("unexpected error: got: %v, want: %v", err, ErrProxyClosed)
	}
}

func TestProxyServe_after_close(t *testing.T) {
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

func TestProxyClose_without_listening(t *testing.T) {
	p := &Proxy{ErrorLog: discardLogger}

	if err := p.Close(); err != ErrProxyNotListening {
		t.Errorf("unexpected error: got: %v, want: %v", err, ErrProxyNotListening)
	}
}

func TestProxyClose_twice(t *testing.T) {
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

func TestProxyBeforeAccept(t *testing.T) {
	wantErr := errors.New("BeforeAccept error")

	p := &Proxy{ErrorLog: discardLogger}
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

func newTestProxy(listenNetwork, listenAddr, dialNetwork, dialAddr string) (*Proxy, <-chan error) {
	p := &Proxy{ErrorLog: discardLogger}

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
