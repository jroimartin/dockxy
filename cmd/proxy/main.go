// Proxy establishes multiple bidirectional data streams between
// different network types.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"

	"github.com/jroimartin/proxy"
)

func main() {
	flag.Usage = usage
	flag.Parse()

	if flag.NArg() == 0 {
		usage()
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if err := run(ctx, flag.Args()); err != nil {
		log.Fatalf("error: %v", err)
	}
}

// beforeAccept is set by tests to know when docker-gw is accepting
// connections.
var beforeAccept func()

func run(ctx context.Context, args []string) error {
	streams, err := parseArgs(args)
	if err != nil {
		return fmt.Errorf("parse arguments: %v", err)
	}

	pg := &proxy.Group{}

	var wg sync.WaitGroup
	pg.BeforeAccept = func() error {
		wg.Done()
		return nil
	}

	wg.Add(len(streams))
	errc := make(chan error)
	go func() { errc <- pg.ListenAndServe(streams) }()
	wg.Wait()

	if beforeAccept != nil {
		beforeAccept()
	}

	select {
	case <-ctx.Done():
		if err := pg.Close(); err != nil {
			log.Printf("error: close: %v", err)
		}
	case err := <-errc:
		return fmt.Errorf("proxy group returned unexpectedly: %v", err)
	}

	if err := <-errc; !errors.Is(err, proxy.ErrGroupClosed) {
		return fmt.Errorf("listen and serve: %v", err)
	}
	return nil
}

func parseArgs(args []string) ([]proxy.Stream, error) {
	var streams []proxy.Stream
	for _, arg := range args {
		stream, err := proxy.ParseStream(arg)
		if err != nil {
			return nil, fmt.Errorf("parse stream %q: %v", arg, err)
		}
		streams = append(streams, stream)
	}
	return streams, nil
}

func usage() {
	fmt.Fprintf(os.Stderr, "usage: proxy streams\n")
	fmt.Fprintf(os.Stderr, `
Streams are specified using the format <listener>,<target>.

Example:

	proxy tcp:localhost:1111,unix:/first/socket tcp::2222,tcp:example.com:3333

The previous command will create two proxies. The first one listens on
localhost:1111 and forwards traffic to the Unix socket /first/socket.
The second proxy listens on 0.0.0.0:2222 and forwards traffic to
example.com:3333.
`)
	flag.PrintDefaults()
}
