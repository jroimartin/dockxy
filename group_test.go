package proxy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"syscall"
	"testing"
)

func TestGroup_ListenAndServe(t *testing.T) {
	const nproxies = 5

	var streams []Stream
	resps := make(map[string][]byte)
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
		resps[tsAddr.String()] = []byte(resp)
	}

	pg := NewGroup()
	evc, errc := pg.ListenAndServe(streams...)

	pgStreams := make(map[string]string)
	n := nproxies
	for ev := range evc {
		if ev.Kind != KindBeforeAccept {
			continue
		}
		stream := ev.Data.(Stream)
		pgStreams[stream.DialAddr] = stream.ListenAddr
		n--
		if n == 0 {
			break
		}
	}

	for dialAddr, listenAddr := range pgStreams {
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
	resps := make(map[string][]byte, nproxies)
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
		resps[tsAddr.String()] = []byte(resp)
	}

	pg := NewGroup()

	var errcs []<-chan error
	pgStreams := make(map[string]string)
	for i := 0; i < nproxies; i += batchsz {
		sz := min(batchsz, len(streams)-i)
		batch := streams[i : i+sz]

		evc, errc := pg.ListenAndServe(batch...)
		n := sz
		for ev := range evc {
			if ev.Kind != KindBeforeAccept {
				continue
			}
			stream := ev.Data.(Stream)
			pgStreams[stream.DialAddr] = stream.ListenAddr
			n--
			if n == 0 {
				break
			}
		}
		errcs = append(errcs, errc)
	}

	for dialAddr, listenAddr := range pgStreams {
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

	pg := NewGroup()

	evcMux := make(chan Event)
	errcMux := make(chan error)
	for i := 0; i < ncalls; i++ {
		evc, errc := pg.ListenAndServe(stream)
		go func() {
			for ev := range evc {
				evcMux <- ev
			}
		}()
		go func() {
			for err := range errc {
				errcMux <- err
			}
		}()
	}

	waitBeforeAccept(evcMux, 1)

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

	for i := 0; i < ncalls-1; i++ {
		if !errors.Is(<-errcMux, ErrDuplicatedStream) {
			t.Errorf("unexpected error: got: %v, want: %v", err, ErrDuplicatedStream)
		}
	}

	if err := pg.Close(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	for i := 0; i < ncalls; i++ {
		if errors.Is(err, ErrGroupClosed) {
			t.Errorf("unexpected error: got: %v, want: %v", err, ErrGroupClosed)
		}
	}
}

func TestGroup_ListenAndServe_after_close(t *testing.T) {
	stream, err := ParseStream("tcp:127.0.0.1:0,tcp:127.0.0.1:1234")
	if err != nil {
		t.Fatalf("could not parse stream: %v", err)
	}

	pg := NewGroup()
	_, errc := pg.ListenAndServe(stream)

	if err := pg.Close(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	if err := <-errc; !errors.Is(err, ErrGroupClosed) {
		t.Errorf("unexpected error: got: %v, want: %v", err, ErrGroupClosed)
	}

	if _, errc := pg.ListenAndServe(stream); !errors.Is(<-errc, ErrGroupClosed) {
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

	pg := NewGroup()

	evc, errc := pg.ListenAndServe(stream1, stream2, stream1)

	waitBeforeAccept(evc, 2)

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

	pg := NewGroup()
	evc, errc := pg.ListenAndServe(stream)

	var listenAddr string
	for ev := range evc {
		if ev.Kind == KindBeforeAccept {
			listenAddr = ev.Data.(Stream).ListenAddr
			break
		}
	}

	stream, err = ParseStream(fmt.Sprintf("tcp:%v,tcp:127.0.0.1:1234", listenAddr))
	if err != nil {
		t.Fatalf("could not parse stream: %v", err)
	}

	if _, errc := pg.ListenAndServe(stream); !errors.Is(<-errc, syscall.EADDRINUSE) {
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
	pg := NewGroup()

	for i := 0; i < 2; i++ {
		if err := pg.Close(); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	}
}
