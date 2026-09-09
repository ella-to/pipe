// Command turnserver runs a STUN and TURN server with per-user throughput
// plans, for developing against pipe and as a starting point for a small
// production relay.
//
// The default configuration is a local development server with one user,
// admin/admin, on 127.0.0.1:3478:
//
//	go run ./examples/turnserver
//
// It prints the STUN and TURN URLs to hand to a client. Give different users
// different budgets with plans; this is how a service relays free users at
// 512 KiB/s and paying users faster from one server:
//
//	go run ./examples/turnserver \
//	    -plans 'free=512KiB/4,paid=8MiB/32' \
//	    -users 'alice=alice-secret:free,bob=bob-secret:paid' \
//	    -stats 5s
//
// Instead of a static user list, hand out short-lived credentials signed with
// a shared secret (the TURN REST API scheme, coturn's use-auth-secret). The
// plan rides along in the user ID as "name@plan":
//
//	export PIPE_TURN_SECRET=$(openssl rand -hex 32)
//	go run ./examples/turnserver -plans 'free=512KiB/4,paid=8MiB/32'
//	go run ./examples/turncred -user alice@free -ttl 12h     # prints credentials
//
// admin/admin is a convenience for a server bound to loopback. Anything
// reachable from a network needs real credentials, a real realm, a public
// relay address, and a relay port range that the firewall allows:
//
//	go run ./examples/turnserver -listen 0.0.0.0:3478 -relay-ip 203.0.113.10 \
//	    -relay-ports 49152-49352 -realm relay.example.net -users "$PIPE_TURN_USERS"
package main

import (
	"context"
	"errors"
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
	listenTCP := flag.String("listen-tcp", "", "optional TCP address to serve TURN on as well")
	realm := flag.String("realm", turnx.DefaultRealm, "TURN realm")
	users := flag.String("users", "admin=admin",
		"comma-separated user=password[:plan] list (or set PIPE_TURN_USERS)")
	secret := flag.String("auth-secret", "",
		"shared secret for ephemeral credentials (or set PIPE_TURN_SECRET); empty disables them")
	plans := flag.String("plans", "",
		"comma-separated name=rate[/maxallocations] list, e.g. free=512KiB/4,paid=8MiB/32")
	relayIP := flag.String("relay-ip", "",
		"address advertised to clients as their relay address (defaults to the listen address)")
	relayPorts := flag.String("relay-ports", "",
		"inclusive UDP port range for relay sockets, e.g. 49152-49352; empty lets the kernel choose")
	rateSpec := flag.String("rate", "0",
		"default plan: throughput budget per relay socket per direction, e.g. 512KiB; 0 is unlimited")
	burstSpec := flag.String("burst", "0", "default plan: token bucket depth; 0 derives it from -rate")
	maxDelay := flag.Duration("max-delay", turnx.DefaultMaxDelay,
		"how long a relayed packet may be held for budget before it is dropped")
	maxAllocs := flag.Int("max-allocations", 0,
		"default plan: concurrent relay sockets per user; 0 is unlimited")
	statsEvery := flag.Duration("stats", 0, "how often to log traffic counters; 0 disables")
	verbose := flag.Bool("v", false, "log debug detail")
	flag.Parse()

	if err := run(config{
		listen:     *listen,
		listenTCP:  *listenTCP,
		realm:      *realm,
		users:      envOr("PIPE_TURN_USERS", *users),
		secret:     envOr("PIPE_TURN_SECRET", *secret),
		plans:      *plans,
		relayIP:    *relayIP,
		relayPorts: *relayPorts,
		rateSpec:   *rateSpec,
		burstSpec:  *burstSpec,
		maxDelay:   *maxDelay,
		maxAllocs:  *maxAllocs,
		statsEvery: *statsEvery,
		verbose:    *verbose,
	}); err != nil {
		fmt.Fprintln(os.Stderr, "turnserver:", err)
		os.Exit(1)
	}
}

type config struct {
	listen     string
	listenTCP  string
	realm      string
	users      string
	secret     string
	plans      string
	relayIP    string
	relayPorts string
	rateSpec   string
	burstSpec  string
	maxDelay   time.Duration
	maxAllocs  int
	statsEvery time.Duration
	verbose    bool
}

func run(cfg config) error {
	level := slog.LevelInfo
	if cfg.verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	users, err := turnx.ParseUsers(cfg.users)
	if err != nil {
		return err
	}
	if len(users) == 0 && cfg.secret == "" {
		return errors.New("configure -users or -auth-secret; a relay with neither refuses everyone")
	}
	plans, err := turnx.ParsePlans(cfg.plans)
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
	minPort, maxPort, err := turnx.ParsePortRange(cfg.relayPorts)
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
		Listen:     cfg.listen,
		ListenTCP:  cfg.listenTCP,
		Realm:      cfg.realm,
		Users:      users,
		AuthSecret: cfg.secret,
		Plans:      plans,
		DefaultPlan: turnx.Plan{
			Rate:           rateBytes,
			Burst:          burst,
			MaxDelay:       cfg.maxDelay,
			MaxAllocations: cfg.maxAllocs,
		},
		RelayIP: relayIP,
		MinPort: minPort,
		MaxPort: maxPort,
		Logger:  log,
	})
	if err != nil {
		return err
	}
	defer srv.Close()

	// The URLs go to stdout so they can be captured; logs go to stderr.
	fmt.Println("stun: ", srv.STUNURL())
	fmt.Println("turn: ", srv.TURNURL())
	if tcp := srv.TURNTCPURL(); tcp != "" {
		fmt.Println("turn: ", tcp)
	}
	fmt.Println("realm:", cfg.realm)
	if len(users) > 0 {
		fmt.Printf("users: %s\n", strings.Join(names(users), ", "))
	}
	if cfg.secret != "" {
		fmt.Println("ephemeral credentials: enabled (go run ./examples/turncred -user <name@plan>)")
	}
	for _, name := range planNames(plans) {
		fmt.Printf("plan %-8s %s\n", name+":", plans[name])
	}
	if len(users) > 0 {
		fmt.Println("try:  go run ./examples/turnclient -embedded=false -turn", srv.TURNURL(),
			"-user", firstName(users), "-pass '<password>'")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if cfg.statsEvery > 0 {
		ticker := time.NewTicker(cfg.statsEvery)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				logStats(log, srv.Stats())
			case <-ctx.Done():
				log.Info("turn: shutting down")
				logStats(log, srv.Stats())
				return nil
			}
		}
	}

	<-ctx.Done()
	log.Info("turn: shutting down")
	logStats(log, srv.Stats())
	return nil
}

func logStats(log *slog.Logger, s turnx.Stats) {
	log.Info("turn: traffic", "stats", s.String())
	for _, id := range turnx.SortedUsers(s) {
		log.Info("turn: user", "user", id, "stats", s.Users[id].String())
	}
}

// names returns the configured usernames in a stable order. Passwords are never
// printed.
func names(users map[string]turnx.User) []string {
	out := make([]string, 0, len(users))
	for user := range users {
		out = append(out, user)
	}
	slices.Sort(out)
	return out
}

func planNames(plans map[string]turnx.Plan) []string {
	out := make([]string, 0, len(plans))
	for name := range plans {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

func firstName(users map[string]turnx.User) string {
	if all := names(users); len(all) > 0 {
		return all[0]
	}
	return ""
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
