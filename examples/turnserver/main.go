// Command turnserver runs a STUN and TURN server with a configurable
// throughput budget, for developing and testing against pipe.
//
// The default configuration is a local development server with one user,
// admin/admin, on 127.0.0.1:3478:
//
//	go run ./examples/turnserver
//
// It prints the STUN and TURN URLs to hand to a client. Limit the relayed
// throughput to see how a pipe connection behaves on a slow relay:
//
//	go run ./examples/turnserver -rate 512KiB -stats 2s
//
// Credentials come from flags or the environment, never from a checked-in file:
//
//	TUNNEL_TURN_USERS='alice=$(openssl rand -hex 16)' go run ./examples/turnserver
//
// admin/admin is a convenience for a server bound to loopback. Anything
// reachable from a network needs real credentials, a real realm, and a public
// relay address.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"slices"
	"strings"
	"time"

	"ella.to/pipe/examples/internal/turnx"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:3478", "UDP address to serve STUN and TURN on")
	realm := flag.String("realm", turnx.DefaultRealm, "TURN realm")
	users := flag.String("users", "admin=admin",
		"comma-separated user=password list (or set TUNNEL_TURN_USERS)")
	relayIP := flag.String("relay-ip", "",
		"address advertised to clients as their relay address (defaults to the listen address)")
	rateSpec := flag.String("rate", "0",
		"throughput budget per relay socket per direction, e.g. 512KiB or 4MiB; 0 is unlimited")
	burstSpec := flag.String("burst", "0", "token bucket depth; 0 derives it from -rate")
	maxDelay := flag.Duration("max-delay", turnx.DefaultMaxDelay,
		"how long a relayed packet may be held for budget before it is dropped")
	statsEvery := flag.Duration("stats", 0, "how often to log traffic counters; 0 disables")
	verbose := flag.Bool("v", false, "log debug detail")
	flag.Parse()

	if err := run(config{
		listen:     *listen,
		realm:      *realm,
		users:      *users,
		relayIP:    *relayIP,
		rateSpec:   *rateSpec,
		burstSpec:  *burstSpec,
		maxDelay:   *maxDelay,
		statsEvery: *statsEvery,
		verbose:    *verbose,
	}); err != nil {
		fmt.Fprintln(os.Stderr, "turnserver:", err)
		os.Exit(1)
	}
}

type config struct {
	listen     string
	realm      string
	users      string
	relayIP    string
	rateSpec   string
	burstSpec  string
	maxDelay   time.Duration
	statsEvery time.Duration
	verbose    bool
}

func run(cfg config) error {
	level := slog.LevelInfo
	if cfg.verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	spec := cfg.users
	if env := strings.TrimSpace(os.Getenv("TUNNEL_TURN_USERS")); env != "" {
		spec = env
	}
	users, err := turnx.ParseUsers(spec)
	if err != nil {
		return err
	}

	rateBytes, err := turnx.ParseSize(cfg.rateSpec)
	if err != nil {
		return err
	}
	burst, err := turnx.ParseSize(cfg.burstSpec)
	if err != nil {
		return err
	}

	var relayIP net.IP
	if cfg.relayIP != "" {
		if relayIP = net.ParseIP(cfg.relayIP); relayIP == nil {
			return fmt.Errorf("%q is not an IP address", cfg.relayIP)
		}
	}

	srv, err := turnx.Start(turnx.Config{
		Listen:   cfg.listen,
		Realm:    cfg.realm,
		Users:    users,
		RelayIP:  relayIP,
		Rate:     rateBytes,
		Burst:    burst,
		MaxDelay: cfg.maxDelay,
		Logger:   log,
	})
	if err != nil {
		return err
	}
	defer srv.Close()

	// The URLs go to stdout so they can be captured; logs go to stderr.
	fmt.Println("stun:", srv.STUNURL())
	fmt.Println("turn:", srv.TURNURL())
	fmt.Println("realm:", cfg.realm)
	fmt.Printf("users: %s\n", strings.Join(names(users), ", "))
	fmt.Println("try:  go run ./examples/turnclient -turn", srv.TURNURL(),
		"-user", firstName(users), "-pass '<password>'")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if cfg.statsEvery > 0 {
		ticker := time.NewTicker(cfg.statsEvery)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				log.Info("turn: traffic", "stats", srv.Stats().String())
			case <-ctx.Done():
				log.Info("turn: shutting down", "stats", srv.Stats().String())
				return nil
			}
		}
	}

	<-ctx.Done()
	log.Info("turn: shutting down", "stats", srv.Stats().String())
	return nil
}

// names returns the configured usernames in a stable order. Passwords are never
// printed.
func names(users map[string]string) []string {
	out := make([]string, 0, len(users))
	for user := range users {
		out = append(out, user)
	}
	slices.Sort(out)
	return out
}

func firstName(users map[string]string) string {
	if all := names(users); len(all) > 0 {
		return all[0]
	}
	return ""
}
