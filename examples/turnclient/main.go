// Command turnclient moves data through a pipe connection that is forced onto
// a TURN relay, and reports what it measured.
//
// By default it starts its own STUN and TURN server in-process, so the whole
// thing is one command:
//
//	go run ./examples/turnclient
//
// Squeeze the relay to see the effect on a pipe connection. The rate is per
// relay socket per direction, and it is enforced by delaying and dropping
// datagrams — so SCTP inside the pipe meets real congestion, not a simulated
// number:
//
//	go run ./examples/turnclient -rate 512KiB -bytes 4MiB
//
// Point it at a server you started separately — `go run ./examples/turnserver`,
// or coturn, or a cloud TURN service — with the URL and credentials:
//
//	go run ./examples/turnclient -embedded=false \
//		-turn 'turn:127.0.0.1:3478?transport=udp' -user admin -pass admin
//
// Credentials are read from TUNNEL_TURN_USERNAME and TUNNEL_TURN_PASSWORD when
// the flags are empty, which is how they should reach a real deployment.
//
// Two things are worth noticing in the output. The candidate types report
// `relay/relay`, which is proof the bytes went through TURN rather than finding
// a local shortcut. And with a rate set, the relay counters show exactly how
// much was metered and how much was dropped.
//
// Do not expect application throughput to equal -rate. The transfer is an echo,
// so every byte crosses the relay four times — out through the client's
// allocation, in through the server's, and the same again on the way back — and
// each crossing is metered separately. Datagrams over budget are dropped, and
// SCTP inside the pipe responds to that loss by backing off, exactly as it
// would on a congested link. The measured number is what the application really
// gets, not what the token bucket was set to.
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"time"

	"ella.to/pipe"
	"ella.to/pipe/examples/internal/turnx"
	"ella.to/pipe/signaling/memory"
)

const (
	serverID = pipe.PeerID("relay-server")
	clientID = pipe.PeerID("relay-client")
)

func main() {
	embedded := flag.Bool("embedded", true, "run a STUN and TURN server in this process")
	turnURL := flag.String("turn", "", "TURN URL, e.g. turn:203.0.113.10:3478?transport=udp")
	stunURL := flag.String("stun", "", "optional STUN URL")
	user := flag.String("user", "admin", "TURN username (or set TUNNEL_TURN_USERNAME)")
	pass := flag.String("pass", "admin", "TURN password (or set TUNNEL_TURN_PASSWORD)")
	realm := flag.String("realm", turnx.DefaultRealm, "TURN realm; must match the server")
	relayOnly := flag.Bool("relay-only", true, "gather relay candidates only, so TURN cannot be bypassed")
	sizeSpec := flag.String("bytes", "4MiB", "how much data to transfer")
	rateSpec := flag.String("rate", "0", "relay throughput budget for -embedded, e.g. 512KiB; 0 is unlimited")
	burstSpec := flag.String("burst", "0", "token bucket depth for -embedded; 0 derives it from -rate")
	maxDelay := flag.Duration("max-delay", turnx.DefaultMaxDelay,
		"how long -embedded may hold a relayed packet for budget before dropping it")
	verbose := flag.Bool("v", false, "log debug detail")
	flag.Parse()

	cfg := options{
		embedded:  *embedded,
		turnURL:   *turnURL,
		stunURL:   *stunURL,
		user:      envOr("TUNNEL_TURN_USERNAME", *user),
		pass:      envOr("TUNNEL_TURN_PASSWORD", *pass),
		realm:     *realm,
		relayOnly: *relayOnly,
		sizeSpec:  *sizeSpec,
		rateSpec:  *rateSpec,
		burstSpec: *burstSpec,
		maxDelay:  *maxDelay,
		verbose:   *verbose,
	}

	if err := run(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "turnclient:", err)
		os.Exit(1)
	}
}

type options struct {
	embedded  bool
	turnURL   string
	stunURL   string
	user      string
	pass      string
	realm     string
	relayOnly bool
	sizeSpec  string
	rateSpec  string
	burstSpec string
	maxDelay  time.Duration
	verbose   bool
}

func run(opts options) error {
	level := slog.LevelInfo
	if opts.verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	size, err := turnx.ParseSize(opts.sizeSpec)
	if err != nil {
		return err
	}
	if size <= 0 {
		return errors.New("-bytes must be positive")
	}
	rateBytes, err := turnx.ParseSize(opts.rateSpec)
	if err != nil {
		return err
	}
	burst, err := turnx.ParseSize(opts.burstSpec)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// Either run the relay here or use the one the caller pointed us at.
	var relay *turnx.Server
	if opts.embedded {
		relay, err = turnx.Start(turnx.Config{
			Listen:   "127.0.0.1:0",
			Realm:    opts.realm,
			Users:    map[string]string{opts.user: opts.pass},
			Rate:     rateBytes,
			Burst:    burst,
			MaxDelay: opts.maxDelay,
			Logger:   log,
		})
		if err != nil {
			return err
		}
		defer relay.Close()

		opts.turnURL = relay.TURNURL()
		if opts.stunURL == "" {
			opts.stunURL = relay.STUNURL()
		}
	}
	if opts.turnURL == "" {
		return errors.New("either -embedded or -turn is required")
	}

	iceServers := []pipe.ICEServer{{
		URLs:     []string{opts.turnURL},
		Username: opts.user,
		// The credential is a long-term TURN password. It is never logged, and
		// pipe never puts it in an error or a metric label.
		Credential:     opts.pass,
		CredentialType: pipe.ICECredentialPassword,
	}}
	if opts.stunURL != "" {
		// A STUN-only entry carries no credentials.
		iceServers = append(iceServers, pipe.ICEServer{URLs: []string{opts.stunURL}})
	}

	policy := pipe.ICETransportPolicyAll
	if opts.relayOnly {
		// Relay-only gathering is the only way to be sure the transfer used TURN:
		// with host candidates available, ICE would prefer the direct path.
		policy = pipe.ICETransportPolicyRelay
	}

	fmt.Printf("turn:        %s\n", opts.turnURL)
	if opts.stunURL != "" {
		fmt.Printf("stun:        %s\n", opts.stunURL)
	}
	fmt.Printf("user:        %s (realm %s)\n", opts.user, opts.realm)
	fmt.Printf("policy:      %s\n", policy)
	if opts.embedded {
		fmt.Printf("relay rate:  %s\n", rateLabel(rateBytes))
	}
	fmt.Printf("transfer:    %s (echoed, so every byte crosses the relay four times)\n\n",
		turnx.FormatSize(size))

	// Signaling is separate from STUN and TURN: it carries the descriptions that
	// let the two peers find each other. The in-process hub keeps this example to
	// one command; a real deployment uses a networked signaler.
	hub := memory.New()

	newEndpoint := func(id pipe.PeerID) (*pipe.Endpoint, error) {
		return pipe.New(ctx, pipe.Config{
			ID:                 id,
			Signaler:           hub,
			ICEServers:         iceServers,
			ICETransportPolicy: policy,
			DialTimeout:        60 * time.Second,
			Logger:             log,
		})
	}

	server, err := newEndpoint(serverID)
	if err != nil {
		return fmt.Errorf("create the listening endpoint: %w", err)
	}
	defer server.Close()

	client, err := newEndpoint(clientID)
	if err != nil {
		return fmt.Errorf("create the dialing endpoint: %w", err)
	}
	defer client.Close()

	ln, err := server.Listen()
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer ln.Close()

	// The listening side echoes whatever it receives.
	accepted := make(chan error, 1)
	go func() { accepted <- echo(ln) }()

	started := time.Now()
	conn, err := client.Dial(ctx, serverID)
	if err != nil {
		return fmt.Errorf("dial over TURN: %w", err)
	}
	defer conn.Close()

	fmt.Printf("connected in %v\n", time.Since(started).Round(time.Millisecond))
	report(conn)

	elapsed, err := transfer(conn, int(size))
	if err != nil {
		return err
	}

	fmt.Printf("\ntransferred  %s round trip in %v\n",
		turnx.FormatSize(size), elapsed.Round(time.Millisecond))
	fmt.Printf("throughput   %s/s each way\n", turnx.FormatSize(int64(float64(size)/elapsed.Seconds())))
	report(conn)

	if relay != nil {
		fmt.Printf("\nrelay        %s\n", relay.Stats())
	}

	if err := conn.Close(); err != nil {
		return fmt.Errorf("close the connection: %w", err)
	}
	if err := ln.Close(); err != nil {
		return fmt.Errorf("close the listener: %w", err)
	}
	if err := <-accepted; err != nil {
		return err
	}
	return nil
}

// transfer writes size random bytes, reads them back, and verifies the digest.
func transfer(conn net.Conn, size int) (time.Duration, error) {
	payload := make([]byte, size)
	if _, err := rand.Read(payload); err != nil {
		return 0, err
	}
	want := sha256.Sum256(payload)

	if err := conn.SetDeadline(time.Now().Add(10 * time.Minute)); err != nil {
		return 0, err
	}

	started := time.Now()

	written := make(chan error, 1)
	go func() {
		_, err := conn.Write(payload)
		written <- err
	}()

	got := make([]byte, size)
	if _, err := io.ReadFull(conn, got); err != nil {
		return 0, fmt.Errorf("read the echo: %w", err)
	}
	elapsed := time.Since(started)

	if err := <-written; err != nil {
		return 0, fmt.Errorf("write: %w", err)
	}
	if sha256.Sum256(got) != want {
		return 0, errors.New("the echoed bytes do not match what was sent")
	}
	return elapsed, nil
}

// echo copies each accepted connection back onto itself until the listener
// closes.
func echo(ln net.Listener) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("accept: %w", err)
		}

		go func() {
			defer conn.Close()
			if _, err := io.Copy(conn, conn); err != nil &&
				!errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				fmt.Fprintln(os.Stderr, "echo:", err)
			}
		}()
	}
}

// report prints the parts of ConnStats that say how the connection is running.
func report(conn net.Conn) {
	tc, ok := conn.(*pipe.Conn)
	if !ok {
		return
	}

	s := tc.Stats()
	fmt.Printf("state        %s, candidates %s/%s, read %s, written %s\n",
		s.State,
		candidate(s.LocalCandidate), candidate(s.RemoteCandidate),
		turnx.FormatSize(int64(s.BytesRead)), turnx.FormatSize(int64(s.BytesWritten)))
}

func candidate(c pipe.CandidateType) string {
	if c == pipe.CandidateUnknown {
		return "unknown"
	}
	return string(c)
}

func rateLabel(rateBytes int64) string {
	if rateBytes <= 0 {
		return "unlimited"
	}
	return turnx.FormatSize(rateBytes) + "/s per direction"
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
