// Command pipecat is netcat for pipe: it connects stdin and stdout of two
// processes on two machines through a pipe connection.
//
// Start a signaling server somewhere both machines can reach (see
// examples/signaling), then on the first machine:
//
//	pipecat -signal https://signal.example.net/pipe -token "$BOB_TOKEN" -id bob listen
//
// and on the second:
//
//	echo hello | pipecat -signal https://signal.example.net/pipe -token "$ALICE_TOKEN" -id alice dial bob
//
// Whatever alice writes, bob reads, and the other way around. Because the
// connection is a net.Conn, anything that works over netcat works here: pipe a
// file, an SSH session (ProxyCommand), a tar stream, or a terminal.
//
// Without STUN or TURN the two peers can only meet when host addresses are
// directly reachable, which is true on one LAN and almost never across the
// internet. Add servers with -stun and -turn:
//
//	pipecat ... -stun stun:stun.l.google.com:19302 \
//	    -turn 'turn:relay.example.net:3478?transport=udp' -turn-user alice -turn-pass "$PASS" dial bob
//
// The -relay-only flag forces the connection through TURN, which is the way to
// check a relay actually works.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"time"

	"ella.to/pipe"
	"ella.to/pipe/signaling/sse"
)

func main() {
	signalURL := flag.String("signal", envOr("PIPE_SIGNAL_URL", ""), "signaling server URL (or set PIPE_SIGNAL_URL)")
	token := flag.String("token", envOr("PIPE_SIGNAL_TOKEN", ""), "bearer token for the signaling server (or set PIPE_SIGNAL_TOKEN)")
	id := flag.String("id", envOr("PIPE_ID", ""), "this peer's ID (or set PIPE_ID)")
	stun := flag.String("stun", envOr("PIPE_STUN", ""), "STUN URL, e.g. stun:stun.example.net:3478")
	turn := flag.String("turn", envOr("PIPE_TURN", ""), "TURN URL, e.g. turn:relay.example.net:3478?transport=udp")
	turnUser := flag.String("turn-user", envOr("PIPE_TURN_USERNAME", ""), "TURN username")
	turnPass := flag.String("turn-pass", envOr("PIPE_TURN_PASSWORD", ""), "TURN password")
	relayOnly := flag.Bool("relay-only", false, "use TURN relay candidates only")
	allow := flag.String("allow", "", "comma-separated peer IDs allowed to connect when listening; empty allows all")
	hold := flag.Bool("hold", false, "after stdin ends, keep reading from the peer until it closes instead of closing")
	keepAlive := flag.Duration("keepalive", 15*time.Second, "keepalive probe interval; 0 disables")
	timeout := flag.Duration("timeout", 60*time.Second, "dial timeout")
	verbose := flag.Bool("v", false, "log debug detail to stderr")
	flag.Usage = usage
	flag.Parse()

	if flag.NArg() < 1 {
		usage()
		os.Exit(2)
	}
	mode, args := flag.Arg(0), flag.Args()[1:]

	if err := run(options{
		signalURL: *signalURL,
		token:     *token,
		id:        pipe.PeerID(*id),
		stun:      *stun,
		turn:      *turn,
		turnUser:  *turnUser,
		turnPass:  *turnPass,
		relayOnly: *relayOnly,
		allow:     *allow,
		hold:      *hold,
		keepAlive: *keepAlive,
		timeout:   *timeout,
		verbose:   *verbose,
	}, mode, args); err != nil {
		fmt.Fprintln(os.Stderr, "pipecat:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `usage:
  pipecat [flags] listen            accept one connection, copy it to stdin/stdout
  pipecat [flags] dial <peer-id>    connect to peer-id, copy it to stdin/stdout

flags:
`)
	flag.PrintDefaults()
}

type options struct {
	signalURL string
	token     string
	id        pipe.PeerID
	stun      string
	turn      string
	turnUser  string
	turnPass  string
	relayOnly bool
	allow     string
	hold      bool
	keepAlive time.Duration
	timeout   time.Duration
	verbose   bool
}

func run(opts options, mode string, args []string) error {
	if opts.signalURL == "" {
		return errors.New("-signal is required")
	}
	if opts.id == "" {
		return errors.New("-id is required")
	}

	level := slog.LevelWarn
	if opts.verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	var servers []pipe.ICEServer
	if opts.stun != "" {
		servers = append(servers, pipe.ICEServer{URLs: []string{opts.stun}})
	}
	if opts.turn != "" {
		servers = append(servers, pipe.ICEServer{
			URLs:       []string{opts.turn},
			Username:   opts.turnUser,
			Credential: opts.turnPass,
		})
	}
	policy := pipe.ICETransportPolicyAll
	if opts.relayOnly {
		policy = pipe.ICETransportPolicyRelay
	}

	cfg := pipe.Config{
		ID:                 opts.id,
		Signaler:           &sse.Client{URL: opts.signalURL, Token: opts.token, Logger: log},
		ICEServers:         servers,
		ICETransportPolicy: policy,
		DialTimeout:        opts.timeout,
		KeepAlive:          pipe.KeepAliveConfig{Interval: opts.keepAlive},
		Logger:             log,
	}
	if opts.allow != "" {
		allowed := make(map[pipe.PeerID]bool)
		for _, p := range strings.Split(opts.allow, ",") {
			if p = strings.TrimSpace(p); p != "" {
				allowed[pipe.PeerID(p)] = true
			}
		}
		cfg.AllowPeer = func(peer pipe.PeerID) bool { return allowed[peer] }
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	ep, err := pipe.New(ctx, cfg)
	if err != nil {
		return err
	}
	defer ep.Close()

	var conn *pipe.Conn
	switch mode {
	case "listen":
		ln, err := ep.Listen()
		if err != nil {
			return err
		}
		defer ln.Close()
		fmt.Fprintf(os.Stderr, "pipecat: listening as %s\n", opts.id)

		accepted := make(chan *pipe.Conn, 1)
		errc := make(chan error, 1)
		go func() {
			c, err := ln.AcceptConn()
			if err != nil {
				errc <- err
				return
			}
			accepted <- c
		}()
		select {
		case conn = <-accepted:
		case err := <-errc:
			return err
		case <-ctx.Done():
			return nil
		}

	case "dial":
		if len(args) != 1 {
			return errors.New("dial needs exactly one peer ID")
		}
		conn, err = ep.Dial(ctx, pipe.PeerID(args[0]))
		if err != nil {
			return err
		}

	default:
		return fmt.Errorf("unknown mode %q; use listen or dial", mode)
	}
	defer conn.Close()

	st := conn.Stats()
	fmt.Fprintf(os.Stderr, "pipecat: connected to %s in %v via %s/%s\n",
		conn.PeerID(), st.ConnectDuration.Round(time.Millisecond), orUnknown(st.LocalCandidate), orUnknown(st.RemoteCandidate))

	return copyBoth(ctx, conn, opts.hold)
}

// copyBoth moves stdin to the connection and the connection to stdout until
// either side ends.
//
// By default the end of stdin closes the connection, like netcat with -N: pipe
// has no half-close, and Close waits for the peer to acknowledge what was
// written, so a piped file arrives whole and the peer reads EOF. With hold set
// the connection stays open after stdin ends until the peer closes, which is
// what a request-and-wait-for-the-reply exchange needs.
//
// The stdin copier is deliberately not waited for. When the peer closes first,
// stdin may be a terminal that never ends; the process exits and the operating
// system reclaims it.
func copyBoth(ctx context.Context, conn net.Conn, hold bool) error {
	fromPeer := make(chan error, 1)
	fromStdin := make(chan error, 1)

	go func() {
		_, err := io.Copy(os.Stdout, conn)
		fromPeer <- err
	}()
	go func() {
		_, err := io.Copy(conn, os.Stdin)
		fromStdin <- err
	}()

	var err error
	select {
	case err = <-fromPeer:
		// The peer closed or the connection failed; stdout has everything.
	case err = <-fromStdin:
		if err == nil && hold {
			select {
			case err = <-fromPeer:
			case <-ctx.Done():
			}
		}
	case <-ctx.Done():
	}
	_ = conn.Close()

	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

func orUnknown(c pipe.CandidateType) string {
	if c == pipe.CandidateUnknown {
		return "unknown"
	}
	return string(c)
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
