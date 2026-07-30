// Command echo runs a two-peer echo service over a pipe.
//
// One peer listens, the other dials it by name, and the two exchange lines of
// text. Both peers live in this process and find each other through an
// in-process signaling hub, so the example needs no server, no STUN, and no
// network access:
//
//	go run ./examples/echo
//
// Notice what does not appear below: no SDP, no ICE candidates, no data
// channels. A pipe connection is a net.Conn, a pipe listener is a
// net.Listener, and the peers are named by string. Replacing the memory hub with
// a networked signaler is the only change needed to put the two peers on
// different machines.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"time"

	"ella.to/pipe"
	"ella.to/pipe/signaling/memory"
)

const (
	serverID = pipe.PeerID("echo-server")
	clientID = pipe.PeerID("echo-client")
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("echo: ")

	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// The hub is how the two peers learn about each other. Any transport that
	// implements pipe.Signaler works here; memory keeps the example to one
	// process.
	hub := memory.New()

	server, err := pipe.New(ctx, pipe.Config{ID: serverID, Signaler: hub})
	if err != nil {
		return fmt.Errorf("create the server endpoint: %w", err)
	}
	defer server.Close()

	client, err := pipe.New(ctx, pipe.Config{ID: clientID, Signaler: hub})
	if err != nil {
		return fmt.Errorf("create the client endpoint: %w", err)
	}
	defer client.Close()

	ln, err := server.Listen()
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer ln.Close()

	serving := make(chan struct{})
	go func() {
		defer close(serving)
		serve(ln)
	}()

	if err := speak(ctx, client); err != nil {
		return err
	}

	// Closing the listener ends the accept loop; the endpoint's Close then ends
	// any connection still open.
	if err := ln.Close(); err != nil {
		return fmt.Errorf("close the listener: %w", err)
	}
	<-serving
	return nil
}

// serve accepts connections until the listener closes.
func serve(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				log.Printf("accept: %v", err)
			}
			return
		}
		go echo(conn)
	}
}

// echo reads lines and writes them back upper-cased.
func echo(conn net.Conn) {
	defer conn.Close()

	log.Printf("serving %s", conn.RemoteAddr())

	r := bufio.NewReader(conn)
	for {
		if err := conn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
			log.Printf("set deadline: %v", err)
			return
		}

		line, err := r.ReadString('\n')
		if err != nil {
			// io.EOF means the peer closed deliberately. Anything else means the
			// connection was lost.
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				log.Printf("read from %s: %v", conn.RemoteAddr(), err)
			}
			return
		}

		if _, err := io.WriteString(conn, strings.ToUpper(line)); err != nil {
			log.Printf("write to %s: %v", conn.RemoteAddr(), err)
			return
		}
	}
}

// speak dials the server and exchanges a few lines with it.
func speak(ctx context.Context, ep *pipe.Endpoint) error {
	dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	conn, err := ep.Dial(dialCtx, serverID)
	if err != nil {
		return fmt.Errorf("dial %s: %w", serverID, err)
	}
	defer conn.Close()

	log.Printf("connected to %s as %s", conn.RemoteAddr(), conn.LocalAddr())

	if err := conn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		return fmt.Errorf("set deadline: %w", err)
	}

	r := bufio.NewReader(conn)
	for _, message := range []string{"hello", "pipe looks like a net.Conn", "goodbye"} {
		if _, err := fmt.Fprintf(conn, "%s\n", message); err != nil {
			return fmt.Errorf("send %q: %w", message, err)
		}

		reply, err := r.ReadString('\n')
		if err != nil {
			return fmt.Errorf("read the reply to %q: %w", message, err)
		}
		fmt.Printf("%-32s -> %s", message, reply)
	}

	// Closing here sends an orderly close, so the server's reader sees io.EOF
	// rather than a failure.
	if err := conn.Close(); err != nil {
		return fmt.Errorf("close the connection: %w", err)
	}
	return nil
}
