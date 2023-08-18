package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
)

func TestRun(t *testing.T) {
	const nproxies = 5

	var args []string
	resps := make(map[int][]byte)
	for i := 0; i < nproxies; i++ {
		resp := fmt.Sprintf("response from server %v", i)

		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, resp)
		}))
		defer ts.Close()

		tsAddr := ts.Listener.Addr()
		s := fmt.Sprintf("tcp:127.0.0.1:0,%v:%v", tsAddr.Network(), tsAddr)
		args = append(args, s)

		_, portstr, err := net.SplitHostPort(tsAddr.String())
		if err != nil {
			t.Fatalf("could not split host port: %v", err)
		}

		port, err := strconv.Atoi(portstr)
		if err != nil {
			t.Fatalf("invalid port %q: %v", portstr, err)
		}

		resps[port] = []byte(resp)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errc := waitRun(ctx, args)

	for port, want := range resps {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%v", port))
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
	if err := run(context.Background(), []string{"tcp::0"}); err == nil {
		t.Fatalf("run returned nil error")
	}
}

func waitRun(ctx context.Context, args []string) <-chan error {
	var wg sync.WaitGroup

	beforeAccept = wg.Done

	errc := make(chan error)
	wg.Add(1)
	go func() { errc <- run(ctx, args) }()
	wg.Wait()

	return errc
}
