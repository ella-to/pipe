// Command signaling runs the HTTP signaling server from ella.to/pipe/signaling/sse.
//
// Two pipe endpoints on different machines need a third party to carry their
// offers, answers, and ICE candidates. This is that party. It moves small JSON
// envelopes between authenticated peers and never sees application data, which
// flows peer to peer (or through TURN) once the connection is up.
//
// Development, on one machine, trusting whatever peer ID a client claims:
//
//	go run ./examples/signaling -insecure-trust-peer-header
//
// Anything else uses bearer tokens. Each token belongs to one peer ID:
//
//	export PIPE_SIGNAL_TOKENS="alice=$(openssl rand -hex 24),bob=$(openssl rand -hex 24)"
//	go run ./examples/signaling -listen :8080
//
// Put TLS in front of it (a reverse proxy, or -tls-cert and -tls-key) before it
// leaves your network: bearer tokens are only as secret as the transport.
//
// Clients:
//
//	signaler := &sse.Client{URL: "https://signal.example.net/pipe", Token: os.Getenv("PIPE_SIGNAL_TOKEN")}
//	ep, err := pipe.New(ctx, pipe.Config{ID: "alice", Signaler: signaler})
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"ella.to/pipe"
	"ella.to/pipe/signaling/sse"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8080", "TCP address to serve HTTP on")
	path := flag.String("path", "/pipe", "URL path the signaling handler is mounted at")
	tokens := flag.String("tokens", "", "comma-separated peer=token list (or set PIPE_SIGNAL_TOKENS)")
	trustHeader := flag.Bool("insecure-trust-peer-header", false,
		"believe the X-Pipe-Peer header without a token; local development only")
	maxPeers := flag.Int("max-peers", 0, "bound on concurrently known peers; 0 is unlimited")
	offlineGrace := flag.Duration("offline-grace", sse.DefaultOfflineGrace,
		"how long a disconnected peer keeps its queue before it is forgotten")
	tlsCert := flag.String("tls-cert", "", "PEM certificate; with -tls-key, serve HTTPS directly")
	tlsKey := flag.String("tls-key", "", "PEM private key")
	verbose := flag.Bool("v", false, "log debug detail")
	flag.Parse()

	if err := run(config{
		listen:       *listen,
		path:         *path,
		tokens:       envOr("PIPE_SIGNAL_TOKENS", *tokens),
		trustHeader:  *trustHeader,
		maxPeers:     *maxPeers,
		offlineGrace: *offlineGrace,
		tlsCert:      *tlsCert,
		tlsKey:       *tlsKey,
		verbose:      *verbose,
	}); err != nil {
		fmt.Fprintln(os.Stderr, "signaling:", err)
		os.Exit(1)
	}
}

type config struct {
	listen       string
	path         string
	tokens       string
	trustHeader  bool
	maxPeers     int
	offlineGrace time.Duration
	tlsCert      string
	tlsKey       string
	verbose      bool
}

func run(cfg config) error {
	level := slog.LevelInfo
	if cfg.verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	var auth sse.Authenticator
	switch {
	case cfg.trustHeader:
		auth = sse.TrustPeerHeader()
		log.Warn("signaling: trusting the X-Pipe-Peer header; anyone can act as any peer")
	case cfg.tokens != "":
		table, err := parseTokens(cfg.tokens)
		if err != nil {
			return err
		}
		auth = sse.StaticTokens(table)
		log.Info("signaling: bearer tokens configured", "peers", len(table))
	default:
		return errors.New("configure -tokens (or PIPE_SIGNAL_TOKENS), or pass -insecure-trust-peer-header for local development")
	}

	srv, err := sse.NewServer(sse.Config{
		Authenticator: auth,
		MaxPeers:      cfg.maxPeers,
		OfflineGrace:  cfg.offlineGrace,
		Logger:        log,
	})
	if err != nil {
		return err
	}
	defer srv.Close()

	mux := http.NewServeMux()
	mux.Handle(cfg.path, srv)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "ok peers=%d\n", len(srv.Peers()))
	})

	httpServer := &http.Server{
		Addr:    cfg.listen,
		Handler: mux,
		// No WriteTimeout: it would cut every event stream. The handler sets a
		// per-write deadline itself.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	scheme := "http"
	if cfg.tlsCert != "" || cfg.tlsKey != "" {
		if cfg.tlsCert == "" || cfg.tlsKey == "" {
			return errors.New("-tls-cert and -tls-key must be set together")
		}
		scheme = "https"
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	errc := make(chan error, 1)
	go func() {
		var err error
		if scheme == "https" {
			err = httpServer.ListenAndServeTLS(cfg.tlsCert, cfg.tlsKey)
		} else {
			err = httpServer.ListenAndServe()
		}
		if !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	fmt.Printf("signaling: %s://%s%s\n", scheme, displayAddr(cfg.listen), cfg.path)
	fmt.Printf("health:    %s://%s/healthz\n", scheme, displayAddr(cfg.listen))

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	log.Info("signaling: shutting down", "peers", srv.Peers())
	_ = srv.Close()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return httpServer.Shutdown(shutdownCtx)
}

// parseTokens parses "peer=token,peer2=token2" into the token table the
// authenticator wants.
func parseTokens(spec string) (map[string]pipe.PeerID, error) {
	out := make(map[string]pipe.PeerID)
	for entry := range strings.SplitSeq(spec, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		peer, token, ok := strings.Cut(entry, "=")
		if !ok || peer == "" || token == "" {
			return nil, fmt.Errorf("%q is not a peer=token entry", entry)
		}
		if len(token) < 16 {
			return nil, fmt.Errorf("token for %q is too short; use at least 16 characters", peer)
		}
		if _, dup := out[token]; dup {
			return nil, fmt.Errorf("token for %q is also used by another peer", peer)
		}
		out[token] = pipe.PeerID(peer)
	}
	if len(out) == 0 {
		return nil, errors.New("no tokens were configured")
	}
	return out, nil
}

func displayAddr(listen string) string {
	if strings.HasPrefix(listen, ":") {
		return "localhost" + listen
	}
	return listen
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
