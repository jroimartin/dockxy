// Proxy establishes multiple bidirectional data streams between
// different network types.
//
// Usage:
//
//	proxy streams
//
// Streams is a space-separated list of streams. Each stream with the
// format:
//
//	<listen network>:<listen address>,<dial network>:<dial address>
//
// Example:
//
//	tcp:localhost:6006,unix:/path/to/socket
//
// This creates a proxy that listens on the TCP address localhost:6060
// and establishes a bidirectional data stream with the Unix address
// /path/to/socket.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"

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

	pg := proxy.NewGroup()

	if err := run(ctx, pg, flag.Args(), nil); err != nil {
		log.Fatalf("error: %v", err)
	}
}

func run(ctx context.Context, pg *proxy.Group, args []string, events chan<- proxy.Event) error {
	streams, err := parseArgs(args)
	if err != nil {
		return fmt.Errorf("parse arguments: %v", err)
	}

	evc, errc := pg.ListenAndServe(streams...)

	if events != nil {
		go func() {
			for ev := range evc {
				events <- ev
			}
		}()
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
Streams have the following format:

	<listen address>,<dial address>

Example:

	tcp:localhost:6006,unix:/path/to/socket

This creates a proxy that listens on the TCP address localhost:6060
and establishes a bidirectional data stream with the Unix address
/path/to/socket.
`)
	flag.PrintDefaults()
}
