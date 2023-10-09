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

	if err := run(ctx, flag.Args()); err != nil {
		log.Fatalf("error: %v", err)
	}
}

// runEvents is set by tests to receive the events published by the
// [proxy.Group] created by [run].
var runEvents chan proxy.Event

func run(ctx context.Context, args []string) error {
	streams, err := parseArgs(args)
	if err != nil {
		return fmt.Errorf("parse arguments: %v", err)
	}

	pg := proxy.NewGroup()
	batch := pg.ListenAndServe(streams...)
	defer batch.Flush()
	defer pg.Close()

	var closedEvc bool
	for {
		select {
		case <-ctx.Done():
			if pg.Close(); err != nil {
				log.Printf("error: close: %v", err)
			}
			if err := <-batch.Errors(); !errors.Is(err, proxy.ErrGroupClosed) {
				return fmt.Errorf("listen and serve: %v", err)
			}
			return nil
		case ev, ok := <-batch.Events():
			if runEvents == nil || closedEvc {
				continue
			}

			if ok {
				runEvents <- ev
			} else {
				close(runEvents)
				closedEvc = true
			}
		case err := <-batch.Errors():
			return fmt.Errorf("proxy group returned unexpectedly: %v", err)
		}
	}
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
