package proxy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"syscall"
	"testing"
)

func TestGroup_ListenAndServe(t *testing.T) {
	const nproxies = 5

	var streams []Stream
	resps := make(map[Stream][]byte)
	for i := 0; i < nproxies; i++ {
		resp := fmt.Sprintf("response from server %v", i)

		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, resp)
		}))
		defer ts.Close()

		tsAddr := ts.Listener.Addr()
		stream, err := ParseStream(fmt.Sprintf("tcp:127.0.0.1:0,%v:%v", tsAddr.Network(), tsAddr))
		if err != nil {
			t.Fatalf("could no parse stream: %v", err)
		}

		streams = append(streams, stream)
		resps[stream] = []byte(resp)
	}

	pg, errc := newTestGroup(streams...)

	urls := make(map[Stream]string)
	pg.mu.Lock()
	for s, p := range pg.proxies {
		urls[s] = "http://" + p.Addr().String()
	}
	pg.mu.Unlock()

	for s, u := range urls {
		resp, err := http.Get(u)
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

		want := resps[s]
		if !bytes.Equal(got, want) {
			t.Errorf("unexpected response: got: %s, want: %s", got, want)
		}
	}

	if err := pg.Close(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	if err := <-errc; !errors.Is(err, ErrGroupClosed) {
		t.Errorf("unexpected error: got: %v, want: %v", err, ErrGroupClosed)
	}
}

func TestGroup_ListenAndServe_multiple_calls(t *testing.T) {
	const (
		nproxies = 5
		batchsz  = 2
	)

	streams := make([]Stream, nproxies)
	resps := make(map[Stream][]byte, nproxies)
	for i := 0; i < nproxies; i++ {
		resp := fmt.Sprintf("response from server %v", i)

		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, resp)
		}))
		defer ts.Close()

		tsAddr := ts.Listener.Addr()
		stream, err := ParseStream(fmt.Sprintf("tcp:127.0.0.1:0,%v:%v", tsAddr.Network(), tsAddr))
		if err != nil {
			t.Fatalf("could no parse stream: %v", err)
		}

		streams[i] = stream
		resps[stream] = []byte(resp)
	}

	pg := &Group{}

	var wg sync.WaitGroup
	pg.BeforeAccept = func() error {
		wg.Done()
		return nil
	}

	var errcs []<-chan error
	for i := 0; i < nproxies; i += batchsz {
		sz := min(batchsz, len(streams)-i)
		batch := streams[i : i+sz]

		wg.Add(sz)
		errc := pg.ListenAndServe(batch...)
		errcs = append(errcs, errc)
	}

	wg.Wait()

	urls := make(map[Stream]string)
	pg.mu.Lock()
	for s, p := range pg.proxies {
		urls[s] = "http://" + p.Addr().String()
	}
	pg.mu.Unlock()

	for s, u := range urls {
		resp, err := http.Get(u)
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

		want := resps[s]
		if !bytes.Equal(got, want) {
			t.Errorf("unexpected response: got: %s, want: %s", got, want)
		}
	}

	if err := pg.Close(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	for _, errc := range errcs {
		if err := <-errc; !errors.Is(err, ErrGroupClosed) {
			t.Errorf("unexpected error: got: %v, want: %v", err, ErrGroupClosed)
		}
	}
}

func TestGroup_ListenAndServe_multiple_calls_one_stream(t *testing.T) {
	const (
		want   = "response from server"
		ncalls = 5
	)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, want)
	}))
	defer ts.Close()

	tsAddr := ts.Listener.Addr()
	stream, err := ParseStream(fmt.Sprintf("tcp:127.0.0.1:0,%v:%v", tsAddr.Network(), tsAddr))
	if err != nil {
		t.Fatalf("could no parse stream: %v", err)
	}

	pg := &Group{}

	var wg sync.WaitGroup
	pg.BeforeAccept = func() error {
		wg.Done()
		return nil
	}

	var errcs []<-chan error
	wg.Add(1) // There is only 1 different stream.
	for i := 0; i < ncalls; i++ {
		errc := pg.ListenAndServe(stream)
		errcs = append(errcs, errc)
	}

	wg.Wait()

	resp, err := http.Get(ts.URL)
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

	if string(got) != want {
		t.Errorf("unexpected response: got: %s, want: %s", got, want)
	}

	if err := pg.Close(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	var nDupErr, nCloseErr int
	for _, errc := range errcs {
		var seenDupErr, seenCloseErr bool
		for err := range errc {
			switch {
			case errors.Is(err, ErrDuplicatedStream):
				if seenDupErr || seenCloseErr {
					t.Errorf("unexpected ErrDuplicatedStream error")
				}
				seenDupErr = true
				nDupErr++
			case errors.Is(err, ErrGroupClosed):
				if seenCloseErr {
					t.Errorf("unexpected ErrGroupClosed error")
				}
				seenCloseErr = true
				nCloseErr++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}
	}

	if nDupErr != ncalls-1 {
		t.Errorf("unexpected number of ErrDuplicatedStream errors: got: %v, want: %v", nDupErr, ncalls-1)
	}

	if nCloseErr != ncalls {
		t.Errorf("unexpected number of ErrGroupClosed errors: got: %v, want: %v", nCloseErr, ncalls)
	}
}

func TestGroup_ListenAndServe_after_close(t *testing.T) {
	stream, err := ParseStream("tcp:127.0.0.1:0,tcp:127.0.0.1:1234")
	if err != nil {
		t.Fatalf("could not parse stream: %v", err)
	}

	pg, errc := newTestGroup(stream)

	if err := pg.Close(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	if err := <-errc; !errors.Is(err, ErrGroupClosed) {
		t.Errorf("unexpected error: got: %v, want: %v", err, ErrGroupClosed)
	}

	if err := <-pg.ListenAndServe(stream); !errors.Is(err, ErrGroupClosed) {
		t.Errorf("unexpected error: got: %v, want: %v", err, ErrGroupClosed)
	}
}

func TestGroup_ListenAndServe_duplicated_stream(t *testing.T) {
	stream1, err := ParseStream("tcp:127.0.0.1:0,tcp:127.0.0.1:1234")
	if err != nil {
		t.Fatalf("could not parse stream: %v", err)
	}
	stream2, err := ParseStream("tcp:127.0.0.1:0,tcp:127.0.0.1:1235")
	if err != nil {
		t.Fatalf("could not parse stream: %v", err)
	}

	pg := &Group{}

	var wg sync.WaitGroup
	pg.BeforeAccept = func() error {
		wg.Done()
		return nil
	}

	wg.Add(2) // There are 2 different streams.
	errc := pg.ListenAndServe(stream1, stream2, stream1)
	wg.Wait()

	if err := <-errc; !errors.Is(err, ErrDuplicatedStream) {
		t.Errorf("unexpected error: got: %v, want: %v", err, ErrDuplicatedStream)
	}

	if err := pg.Close(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	for err := range errc {
		if !errors.Is(err, ErrGroupClosed) {
			t.Errorf("unexpected error: got: %v, want: %v", err, ErrGroupClosed)
		}
	}
}

func TestGroup_ListenAndServe_duplicated_listener(t *testing.T) {
	stream, err := ParseStream("tcp:127.0.0.1:0,tcp:127.0.0.1:1234")
	if err != nil {
		t.Fatalf("could not parse stream: %v", err)
	}

	pg, errc := newTestGroup(stream)

	p := pg.Proxy(stream)
	if p == nil {
		t.Fatalf("could not get proxy")
	}

	stream, err = ParseStream(fmt.Sprintf("tcp:%v,tcp:127.0.0.1:1234", p.Addr()))
	if err != nil {
		t.Fatalf("could not parse stream: %v", err)
	}

	if err := <-pg.ListenAndServe(stream); !errors.Is(err, syscall.EADDRINUSE) {
		t.Errorf("unexpected error: got: %v, want: %v", err, syscall.EADDRINUSE)
	}

	if err := pg.Close(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	if err := <-errc; !errors.Is(err, ErrGroupClosed) {
		t.Errorf("unexpected error: got: %v, want: %v", err, ErrGroupClosed)
	}
}

func TestGroup_Close_twice(t *testing.T) {
	pg := &Group{}

	for i := 0; i < 2; i++ {
		if err := pg.Close(); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	}
}

func TestGroup_BeforeAccept(t *testing.T) {
	wantErr := errors.New("BeforeAccept error")

	stream, err := ParseStream("tcp:127.0.0.1:0,tcp:127.0.0.1:1234")
	if err != nil {
		t.Fatalf("could not parse stream: %v", err)
	}

	pg := &Group{}
	pg.BeforeAccept = func() error {
		return wantErr
	}

	if err := <-pg.ListenAndServe(stream); !errors.Is(err, wantErr) {
		t.Errorf("unexpected error: got: %v, want: %v", err, wantErr)
	}

	if err := pg.Close(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestGroup_Proxy(t *testing.T) {
	stream, err := ParseStream("tcp:127.0.0.1:0,tcp:127.0.0.1:1234")
	if err != nil {
		t.Fatalf("could not parse stream: %v", err)
	}

	pg, errc := newTestGroup(stream)

	if p := pg.Proxy(stream); p == nil {
		t.Errorf("could not get proxy")
	}

	if err := pg.Close(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	if err := <-errc; !errors.Is(err, ErrGroupClosed) {
		t.Errorf("unexpected error: got: %v, want: %v", err, ErrGroupClosed)
	}
}

func TestGroup_Proxy_after_close(t *testing.T) {
	stream, err := ParseStream("tcp:127.0.0.1:0,tcp:127.0.0.1:1234")
	if err != nil {
		t.Fatalf("could not parse stream: %v", err)
	}

	pg, errc := newTestGroup(stream)

	if err := pg.Close(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	if p := pg.Proxy(stream); p != nil {
		t.Errorf("unexpected proxy: %v", p)
	}

	if err := <-errc; !errors.Is(err, ErrGroupClosed) {
		t.Errorf("unexpected error: got: %v, want: %v", err, ErrGroupClosed)
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
				listenNetwork: "tcp",
				listenAddr:    ":1234",
				dialNetwork:   "unix",
				dialAddr:      "/path/to/socket",
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

func newTestGroup(streams ...Stream) (*Group, <-chan error) {
	pg := &Group{}

	var wg sync.WaitGroup
	pg.BeforeAccept = func() error {
		wg.Done()
		return nil
	}

	wg.Add(len(streams))
	errc := pg.ListenAndServe(streams...)
	wg.Wait()

	return pg, errc
}
