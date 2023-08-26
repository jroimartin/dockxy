package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/jroimartin/proxy"
)

func TestRun(t *testing.T) {
	const nproxies = 5

	var args []string
	resps := make(map[proxy.Stream][]byte)
	for i := 0; i < nproxies; i++ {
		resp := fmt.Sprintf("response from server %v", i)

		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, resp)
		}))
		defer ts.Close()

		tsAddr := ts.Listener.Addr()
		s := fmt.Sprintf("tcp:127.0.0.1:0,%v:%v", tsAddr.Network(), tsAddr)
		args = append(args, s)

		stream, err := proxy.ParseStream(s)
		if err != nil {
			t.Fatalf("could not parse stream %q: %v", s, err)
		}

		resps[stream] = []byte(resp)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pg, errc := waitRun(ctx, args)

	for stream, want := range resps {
		p := pg.Proxy(stream)
		if p == nil {
			t.Fatalf("no proxy for %v", stream)
		}

		resp, err := http.Get(fmt.Sprintf("http://%v", p.Addr()))
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

		if !bytes.Equal(got, want) {
			t.Errorf("unexpected reponse: got: %s, want: %s", got, want)
		}
	}

	cancel()

	if err := <-errc; err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestRun_invalid_arg(t *testing.T) {
	if err := run(context.Background(), nil, []string{"tcp::0"}); err == nil {
		t.Fatalf("run returned nil error")
	}
}

func waitRun(ctx context.Context, args []string) (*proxy.Group, <-chan error) {
	pg := &proxy.Group{}

	var wg sync.WaitGroup
	pg.BeforeAccept = func() error {
		wg.Done()
		return nil
	}

	wg.Add(len(args))
	errc := make(chan error)
	go func() { errc <- run(ctx, pg, args) }()
	wg.Wait()

	return pg, errc
}
