// A production-style TURN server combining every feature of the package:
//
//   - Dynamic time-limited credentials (HTTP issuer) plus a static admin user
//   - Per-user quotas: allocations + bandwidth, with tiered overrides
//   - Permission filtering: block relays to private/loopback peer ranges so
//     the relay can't be used to reach your internal network
//   - Lifecycle events feeding live metrics (allocations, per-user counts)
//   - Restricted relay port range (easy firewalling: open 50000-55000/udp)
//   - TLS (optional) and graceful shutdown
//
//	go run ./turn/examples/05-advanced -public-ip 127.0.0.1 -secret my-shared-secret
//
// Then:
//
//	curl 'http://localhost:8080/credentials?user=alice&tier=premium'
//	curl 'http://localhost:8080/stats'
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"ella.to/pipe/turn"
)

// tiers maps a subscription level to its quota.
var tiers = map[string]turn.UserQuota{
	"free":    {MaxAllocations: 1, MaxBytesPerSecond: 128_000 / 8},
	"basic":   {MaxAllocations: 2, MaxBytesPerSecond: 1_000_000 / 8},
	"premium": {MaxAllocations: 10, MaxBytesPerSecond: 10_000_000 / 8},
}

// metrics collects allocation lifecycle data from the event callbacks.
type metrics struct {
	mu          sync.Mutex
	created     int
	deleted     int
	authFailed  int
	permsDenied int
}

func main() {
	publicIP := flag.String("public-ip", "", "IP address that TURN clients use to reach this server (required)")
	listen := flag.String("listen", ":3478", "UDP listen address")
	tcpListen := flag.String("tcp", ":3478", "TCP listen address")
	tlsListen := flag.String("tls", "", "TLS (TURNS) listen address, e.g. :5349 (optional)")
	tlsCert := flag.String("tls-cert", "", "TLS certificate file (PEM)")
	tlsKey := flag.String("tls-key", "", "TLS private key file (PEM)")
	httpListen := flag.String("http", ":8080", "HTTP listen address for credentials/stats")
	secret := flag.String("secret", "", "shared secret for dynamic credentials (required)")
	adminPass := flag.String("admin-pass", "", "password for the static 'admin' user (optional)")
	realm := flag.String("realm", "example.com", "authentication realm")
	ttl := flag.Duration("ttl", time.Hour, "issued credential lifetime")
	flag.Parse()

	if *publicIP == "" || *secret == "" {
		flag.Usage()
		os.Exit(1)
	}

	m := &metrics{}

	// Per-user quota table, updated at runtime as the HTTP endpoint issues
	// credentials for a tier. Quota.Lookup is called concurrently from the
	// request path, so access is guarded by a mutex.
	perUser := map[string]turn.UserQuota{}
	var perUserMu sync.Mutex

	quota := &turn.Quota{
		// Unknown users get the free tier.
		Default: tiers["free"],
		Lookup: func(userID string) (turn.UserQuota, bool) {
			perUserMu.Lock()
			defer perUserMu.Unlock()
			uq, ok := perUser[userID]
			return uq, ok
		},
	}

	cfg := turn.Config{
		ListenAddr:    *listen,
		TCPListenAddr: *tcpListen,
		PublicIP:      *publicIP,
		Realm:         *realm,

		// Relay allocations use a fixed port range: open exactly
		// 50000-55000/udp in your firewall.
		RelayMinPort: 50000,
		RelayMaxPort: 55000,

		// Auth: dynamic credentials for end users, a static account for
		// operators/monitoring.
		Dynamic: &turn.DynamicAuth{Secret: *secret, MaxTTL: 24 * time.Hour},

		Quota: quota,

		// Never relay to loopback/private/link-local peers: a public TURN
		// server must not become a proxy into its own network.
		PermissionHandler: func(clientAddr net.Addr, peerIP net.IP) bool {
			if peerIP.IsLoopback() || peerIP.IsPrivate() || peerIP.IsLinkLocalUnicast() {
				log.Printf("DENY permission: client=%s peer=%s (private range)", clientAddr, peerIP)
				m.mu.Lock()
				m.permsDenied++
				m.mu.Unlock()
				return false
			}
			return true
		},

		// Lifecycle events feed metrics and structured logs.
		Events: turn.EventHandler{
			OnAuth: func(src, _ net.Addr, _, username, _ string, method string, verdict bool) {
				if !verdict {
					m.mu.Lock()
					m.authFailed++
					m.mu.Unlock()
					log.Printf("AUTH FAIL: user=%q method=%s from=%s", username, method, src)
				}
			},
			OnAllocationCreated: func(src, _ net.Addr, proto, userID, _ string, relayAddr net.Addr, _ int) {
				m.mu.Lock()
				m.created++
				m.mu.Unlock()
				log.Printf("ALLOC CREATE: user=%s proto=%s client=%s relay=%s", userID, proto, src, relayAddr)
			},
			OnAllocationDeleted: func(src, _ net.Addr, proto, userID, _ string) {
				m.mu.Lock()
				m.deleted++
				m.mu.Unlock()
				log.Printf("ALLOC DELETE: user=%s proto=%s client=%s", userID, proto, src)
			},
			OnAllocationError: func(src, _ net.Addr, proto, message string) {
				log.Printf("ALLOC ERROR: proto=%s client=%s err=%s", proto, src, message)
			},
		},

		// Tighter lifetimes than the 10-minute defaults.
		AllocationLifetime: 5 * time.Minute,
		PermissionTimeout:  5 * time.Minute,
	}

	if *adminPass != "" {
		cfg.Users = []turn.User{{Username: "admin", Password: *adminPass}}
		perUser["admin"] = turn.UserQuota{} // unlimited
	}
	if *tlsListen != "" && *tlsCert != "" && *tlsKey != "" {
		cfg.TLSListenAddr = *tlsListen
		cfg.TLSCertFile = *tlsCert
		cfg.TLSKeyFile = *tlsKey
	}

	srv := &turn.Server{}
	if err := srv.Start(cfg); err != nil {
		log.Fatalf("start turn server: %v", err)
	}

	log.Printf("TURN server running, URLs: %v", srv.URLs())
	log.Printf("relay port range: 50000-55000/udp")

	mux := http.NewServeMux()

	// GET /credentials?user=<id>&tier=<free|basic|premium>
	// In production, authenticate the request and derive user/tier from the
	// session instead of query parameters.
	mux.HandleFunc("/credentials", func(w http.ResponseWriter, r *http.Request) {
		user := r.URL.Query().Get("user")
		if user == "" {
			http.Error(w, "missing ?user=", http.StatusBadRequest)
			return
		}
		tier := r.URL.Query().Get("tier")
		userQuota, ok := tiers[tier]
		if !ok {
			userQuota = tiers["free"]
			tier = "free"
		}

		// Register the user's tier before handing out credentials.
		perUserMu.Lock()
		perUser[user] = userQuota
		perUserMu.Unlock()

		username, credential, err := srv.GenerateCredentials(user, *ttl)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"urls":       srv.URLs(),
			"username":   username,
			"credential": credential,
			"ttl":        ttl.String(),
			"tier":       tier,
		})
	})

	// GET /stats — live server metrics.
	mux.HandleFunc("/stats", func(w http.ResponseWriter, _ *http.Request) {
		m.mu.Lock()
		stats := map[string]any{
			"allocations_active":  srv.TotalAllocations(),
			"allocations_created": m.created,
			"allocations_deleted": m.deleted,
			"auth_failures":       m.authFailed,
			"permissions_denied":  m.permsDenied,
		}
		m.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(stats)
	})

	httpSrv := &http.Server{Addr: *httpListen, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		log.Printf("HTTP API on http://%s (/credentials, /stats)", *httpListen)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http server: %v", err)
		}
	}()

	// Graceful shutdown on SIGINT/SIGTERM.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	<-sigs
	log.Println("shutting down...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	if err := srv.Close(); err != nil {
		log.Printf("turn server close: %v", err)
	}
}
