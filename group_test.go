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
	batch := pg.ListenAndServe(streams...)
	defer batch.Flush()
	defer pg.Close()

	pgStreams := make(map[string]string)
	n := nproxies
loop:
	for {
		select {
		case ev := <-batch.Events():
			if ev.Kind != KindBeforeAccept {
				continue
			}
			stream := ev.Data.(Stream)
			pgStreams[stream.DialAddr] = stream.ListenAddr
			n--
			if n == 0 {
				break loop
			}
		case err := <-batch.Errors():
			t.Fatalf("unexpected error: %v", err)
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

	checkErrors(t, batch.Errors(), ErrGroupClosed, 1)
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
	defer pg.Close()

	var wg sync.WaitGroup
	errcMux := make(chan error)
	pgStreams := make(map[string]string)
	for i := 0; i < nproxies; i += batchsz {
		sz := min(batchsz, len(streams)-i)
		batch := pg.ListenAndServe(streams[i : i+sz]...)
		defer batch.Flush()
		defer pg.Close()

		n := sz
	loop:
		for {
			select {
			case ev := <-batch.Events():
				if ev.Kind != KindBeforeAccept {
					continue
				}
				stream := ev.Data.(Stream)
				pgStreams[stream.DialAddr] = stream.ListenAddr
				n--
				if n == 0 {
					break loop
				}
			case err := <-batch.Errors():
				t.Fatalf("unexpected error: %v", err)
			}
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			for err := range batch.Errors() {
				errcMux <- err
			}
		}()
	}
	go func() {
		wg.Wait()
		close(errcMux)
	}()

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

	checkErrors(t, errcMux, ErrGroupClosed, (nproxies+batchsz-1)/batchsz)
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
	defer pg.Close()

	var wg sync.WaitGroup
	evcMux := make(chan Event)
	errcMux := make(chan error)
	for i := 0; i < ncalls; i++ {
		batch := pg.ListenAndServe(stream)
		defer batch.Flush()
		defer pg.Close()

		wg.Add(2)
		go func() {
			defer wg.Done()
			for ev := range batch.Events() {
				evcMux <- ev
			}
		}()
		go func() {
			defer wg.Done()
			for err := range batch.Errors() {
				errcMux <- err
			}
		}()
	}
	go func() {
		wg.Wait()
		close(evcMux)
		close(errcMux)
	}()

	for nevs, nerrs := 0, 0; nevs < 1 || nerrs < ncalls-1; {
		select {
		case ev := <-evcMux:
			if ev.Kind == KindBeforeAccept {
				nevs++
			}
		case err := <-errcMux:
			if !errors.Is(err, ErrDuplicatedStream) {
				t.Errorf("unexpected error: got: %v, want: %v", err, ErrDuplicatedStream)
			} else {
				nerrs++
			}
		}
	}

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

	for err := range errcMux {
		if !errors.Is(err, ErrGroupClosed) {
			t.Errorf("unexpected error: got: %v, want: %v", err, ErrGroupClosed)
		}
	}

	if ev, ok := <-evcMux; ok {
		t.Errorf("events channel should be closed: got: %v", ev)
	}
}

func TestGroup_ListenAndServe_no_streams(t *testing.T) {
	pg := NewGroup()
	batch := pg.ListenAndServe()
	defer batch.Flush()
	defer pg.Close()

	if err, ok := <-batch.Errors(); ok {
		t.Errorf("errors channel should be closed: got: %v", err)
	}

	if ev, ok := <-batch.Events(); ok {
		t.Errorf("events channel should be closed: got: %v", ev)
	}

	if err := pg.Close(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	if err, ok := <-batch.Errors(); ok {
		t.Errorf("errors channel should be closed: got: %v", err)
	}

	if ev, ok := <-batch.Events(); ok {
		t.Errorf("events channel should be closed: got: %v", ev)
	}
}

func TestGroup_ListenAndServe_after_close(t *testing.T) {
	stream, err := ParseStream("tcp:127.0.0.1:0,tcp:127.0.0.1:1234")
	if err != nil {
		t.Fatalf("could not parse stream: %v", err)
	}

	pg := NewGroup()
	batch := pg.ListenAndServe(stream)
	defer batch.Flush()
	defer pg.Close()

	waitBeforeAccept(t, batch, 1)

	if err := pg.Close(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	checkErrors(t, batch.Errors(), ErrGroupClosed, 1)

	batch = pg.ListenAndServe(stream)
	defer batch.Flush()
	defer pg.Close()

	checkErrors(t, batch.Errors(), ErrGroupClosed, 1)
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
	batch := pg.ListenAndServe(stream1, stream2, stream1)
	defer batch.Flush()
	defer pg.Close()

	for nevs, nerrs := 0, 0; nevs < 2 || nerrs < 1; {
		select {
		case ev := <-batch.Events():
			if ev.Kind == KindBeforeAccept {
				nevs++
			}
		case err := <-batch.Errors():
			if !errors.Is(err, ErrDuplicatedStream) {
				t.Errorf("unexpected error: got: %v, want: %v", err, ErrDuplicatedStream)
			} else {
				nerrs++
			}
		}
	}

	if err := pg.Close(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	checkErrors(t, batch.Errors(), ErrGroupClosed, 1)
}

func TestGroup_ListenAndServe_duplicated_listener(t *testing.T) {
	stream, err := ParseStream("tcp:127.0.0.1:0,tcp:127.0.0.1:1234")
	if err != nil {
		t.Fatalf("could not parse stream: %v", err)
	}

	pg := NewGroup()
	batch := pg.ListenAndServe(stream)
	defer batch.Flush()
	defer pg.Close()

	var listenAddr string
loop:
	for {
		select {
		case ev := <-batch.Events():
			if ev.Kind == KindBeforeAccept {
				listenAddr = ev.Data.(Stream).ListenAddr
				break loop
			}
		case err := <-batch.Errors():
			t.Fatalf("unexpected error: %v", err)
		}
	}

	stream, err = ParseStream(fmt.Sprintf("tcp:%v,tcp:127.0.0.1:1234", listenAddr))
	if err != nil {
		t.Fatalf("could not parse stream: %v", err)
	}

	batch2 := pg.ListenAndServe(stream)
	defer batch2.Flush()
	defer pg.Close()

	if !errors.Is(<-batch2.Errors(), syscall.EADDRINUSE) {
		t.Errorf("unexpected error: got: %v, want: %v", err, syscall.EADDRINUSE)
	}

	if err := pg.Close(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	checkErrors(t, batch.Errors(), ErrGroupClosed, 1)
}

func TestGroup_Close_twice(t *testing.T) {
	pg := NewGroup()

	if err := pg.Close(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if err := pg.Close(); !errors.Is(err, ErrGroupClosed) {
		t.Errorf("unexpected error: got: %v, want: %v", err, ErrGroupClosed)
	}
}

func TestGroup_Close_close_chans(t *testing.T) {
	stream, err := ParseStream("tcp:127.0.0.1:0,tcp:127.0.0.1:1234")
	if err != nil {
		t.Fatalf("could not parse stream: %v", err)
	}

	pg := NewGroup()
	batch := pg.ListenAndServe(stream)
	defer batch.Flush()
	defer pg.Close()

	waitBeforeAccept(t, batch, 1)

	if err := pg.Close(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	var nerr int
loop:
	for {
		select {
		case ev := <-batch.Events():
			t.Errorf("unexpected event: %v", ev)
		case err := <-batch.Errors():
			if !errors.Is(err, ErrGroupClosed) {
				t.Errorf("unexpected error: got: %v, want: %v", err, ErrGroupClosed)
			}
			nerr++
		default:
			break loop
		}
	}

	if nerr > 1 {
		t.Errorf("received more than one error: %v", nerr)
	}
}

func TestBatch_Flush(t *testing.T) {
	stream, err := ParseStream("tcp:127.0.0.1:0,tcp:127.0.0.1:1234")
	if err != nil {
		t.Fatalf("could not parse stream: %v", err)
	}

	pg := NewGroup()
	batch := pg.ListenAndServe(stream)
	defer batch.Flush()
	defer pg.Close()

	waitBeforeAccept(t, batch, 1)

	if err := pg.Close(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	batch.Flush()

	if _, ok := <-batch.Events(); ok {
		t.Errorf("events channel should be closed")
	}

	if _, ok := <-batch.Errors(); ok {
		t.Errorf("errors channel should be closed")
	}
}

func waitBeforeAccept(t *testing.T, batch Batch, n int) {
	for {
		select {
		case ev := <-batch.Events():
			if ev.Kind == KindBeforeAccept {
				n--
			}
			if n == 0 {
				return
			}
		case err := <-batch.Errors():
			t.Fatalf("unexpected error: %v", err)
		}
	}
}

func checkErrors(t *testing.T, errc <-chan error, wantErr error, wantN int) {
	var n int
	for err := range errc {
		if !errors.Is(err, wantErr) {
			t.Errorf("unexpected error: got: %v, want: %v", err, wantErr)
		}
		n++
	}
	if n != wantN {
		t.Errorf("unexpected number of errors: got: %v, want: %v", n, wantN)
	}
}
