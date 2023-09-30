package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jroimartin/proxy"
)

func TestRun(t *testing.T) {
	const nproxies = 5

	var args []string
	resps := make(map[string][]byte)
	for i := 0; i < nproxies; i++ {
		resp := fmt.Sprintf("response from server %v", i)

		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, resp)
		}))
		defer ts.Close()

		tsAddr := ts.Listener.Addr()
		s := fmt.Sprintf("tcp:127.0.0.1:0,%v:%v", tsAddr.Network(), tsAddr)
		args = append(args, s)

		resps[tsAddr.String()] = []byte(resp)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pg := proxy.NewGroup()

	errc := make(chan error)
	evc := make(chan proxy.Event)
	go func() { errc <- run(ctx, pg, args, evc) }()

	streams := make(map[string]string)
	n := nproxies
	for ev := range evc {
		if ev.Kind != proxy.KindBeforeAccept {
			continue
		}
		stream := ev.Data.(proxy.Stream)
		streams[stream.DialAddr] = stream.ListenAddr
		n--
		if n == 0 {
			break
		}
	}

	for dialAddr, listenAddr := range streams {
		resp, err := http.Get("http://" + listenAddr)
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

		want := resps[dialAddr]
		if !bytes.Equal(got, want) {
			t.Errorf("unexpected response: got: %s, want: %s", got, want)
		}
	}

	cancel()

	if err := <-errc; err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestRun_invalid_arg(t *testing.T) {
	if err := run(context.Background(), nil, []string{"tcp::0"}, nil); err == nil {
		t.Fatalf("run returned nil error")
	}
}
