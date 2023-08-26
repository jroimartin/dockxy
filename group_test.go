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

func TestGroupListenAndServe(t *testing.T) {
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

	pg, errc := newTestGroup(streams)

	pg.mu.Lock()
	proxies := pg.proxies
	pg.mu.Unlock()

	for s, p := range proxies {
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

		want := resps[s]
		if !bytes.Equal(got, want) {
			t.Errorf("unexpected reponse: got: %s, want: %s", got, want)
		}
	}

	if err := pg.Close(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	if err := <-errc; !errors.Is(err, ErrGroupClosed) {
		t.Errorf("unexpected error: got: %v, want: %v", err, ErrGroupClosed)
	}
}

func TestGroupListenAndServe_after_close(t *testing.T) {
	stream, err := ParseStream("tcp:127.0.0.1:0,tcp:127.0.0.1:1234")
	if err != nil {
		t.Fatalf("could not parse stream: %v", err)
	}

	pg, errc := newTestGroup([]Stream{stream})

	if err := pg.Close(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	if err := <-errc; !errors.Is(err, ErrGroupClosed) {
		t.Errorf("unexpected error: got: %v, want: %v", err, ErrGroupClosed)
	}

	if err := <-pg.ListenAndServe([]Stream{stream}); !errors.Is(err, ErrGroupClosed) {
		t.Errorf("unexpected error: got: %v, want: %v", err, ErrGroupClosed)
	}
}

func TestGroupListenAndServe_duplicated_stream(t *testing.T) {
	stream1, err := ParseStream("tcp:127.0.0.1:0,tcp:127.0.0.1:1234")
	if err != nil {
		t.Fatalf("could not parse stream: %v", err)
	}
	stream2, err := ParseStream("tcp:127.0.0.1:0,tcp:127.0.0.1:1235")
	if err != nil {
		t.Fatalf("could not parse stream: %v", err)
	}

	pg := &Group{}

	if err := <-pg.ListenAndServe([]Stream{stream1, stream2, stream1}); !errors.Is(err, ErrDuplicatedStream) {
		t.Errorf("unexpected error: got: %v, want: %v", err, ErrDuplicatedStream)
	}

	if err := pg.Close(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestGroupListenAndServe_duplicated_listener(t *testing.T) {
	stream, err := ParseStream("tcp:127.0.0.1:0,tcp:127.0.0.1:1234")
	if err != nil {
		t.Fatalf("could not parse stream: %v", err)
	}

	pg, errc := newTestGroup([]Stream{stream})

	p := pg.Proxy(stream)
	if p == nil {
		t.Fatalf("could not get proxy")
	}

	stream, err = ParseStream(fmt.Sprintf("tcp:%v,tcp:127.0.0.1:1234", p.Addr()))
	if err != nil {
		t.Fatalf("could not parse stream: %v", err)
	}

	if err := <-pg.ListenAndServe([]Stream{stream}); !errors.Is(err, syscall.EADDRINUSE) {
		t.Errorf("unexpected error: got: %v, want: %v", err, syscall.EADDRINUSE)
	}

	if err := pg.Close(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	if err := <-errc; !errors.Is(err, ErrGroupClosed) {
		t.Errorf("unexpected error: got: %v, want: %v", err, ErrGroupClosed)
	}
}

func TestGroupClose_twice(t *testing.T) {
	pg := &Group{}

	for i := 0; i < 2; i++ {
		if err := pg.Close(); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	}
}

func TestGroupBeforeAccept(t *testing.T) {
	wantErr := errors.New("BeforeAccept error")

	stream, err := ParseStream("tcp:127.0.0.1:0,tcp:127.0.0.1:1234")
	if err != nil {
		t.Fatalf("could not parse stream: %v", err)
	}

	pg := &Group{}
	pg.BeforeAccept = func() error {
		return wantErr
	}

	if err := <-pg.ListenAndServe([]Stream{stream}); !errors.Is(err, wantErr) {
		t.Errorf("unexpected error: got: %v, want: %v", err, wantErr)
	}

	if err := pg.Close(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestGroupProxy(t *testing.T) {
	stream, err := ParseStream("tcp:127.0.0.1:0,tcp:127.0.0.1:1234")
	if err != nil {
		t.Fatalf("could not parse stream: %v", err)
	}

	pg, errc := newTestGroup([]Stream{stream})

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

func TestGroupProxy_after_close(t *testing.T) {
	stream, err := ParseStream("tcp:127.0.0.1:0,tcp:127.0.0.1:1234")
	if err != nil {
		t.Fatalf("could not parse stream: %v", err)
	}

	pg, errc := newTestGroup([]Stream{stream})

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

func newTestGroup(streams []Stream) (*Group, <-chan error) {
	pg := &Group{}

	var wg sync.WaitGroup
	pg.BeforeAccept = func() error {
		wg.Done()
		return nil
	}

	wg.Add(len(streams))
	errc := pg.ListenAndServe(streams)
	wg.Wait()

	return pg, errc
}
