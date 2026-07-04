// A TURN server with per-user quotas.
//
// Two kinds of limits, combinable and overridable per user:
//
//   - MaxAllocations: how many concurrent allocations (sessions) a user may
//     hold. Exceeding it returns a 486 (Allocation Quota Reached) to the
//     client.
//   - MaxBytesPerSecond: total relay bandwidth (upload + download combined)
//     across all of the user's allocations. Packets over budget are dropped,
//     like a congested link.
//
// This example gives everyone 2 allocations and 1 Mbps, while the user
// "premium" gets 10 allocations and 10 Mbps and "free" is capped to a single
// allocation at 128 Kbps.
//
//	go run ./turn/examples/04-quota -public-ip 127.0.0.1 -users alice=secret,premium=gold,free=basic
package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"ella.to/pipe/turn"
)

func main() {
	publicIP := flag.String("public-ip", "", "IP address that TURN clients use to reach this server (required)")
	listen := flag.String("listen", ":3478", "UDP listen address")
	users := flag.String("users", "", `static users as "user=pass,user=pass" (required)`)
	realm := flag.String("realm", "example.com", "authentication realm")
	flag.Parse()

	if *publicIP == "" || *users == "" {
		flag.Usage()
		os.Exit(1)
	}

	var staticUsers []turn.User
	for pair := range strings.SplitSeq(*users, ",") {
		username, password, ok := strings.Cut(pair, "=")
		if !ok {
			log.Fatalf("invalid user %q: expected user=pass", pair)
		}
		staticUsers = append(staticUsers, turn.User{Username: username, Password: password})
	}

	srv := &turn.Server{}
	err := srv.Start(turn.Config{
		ListenAddr: *listen,
		PublicIP:   *publicIP,
		Realm:      *realm,
		Users:      staticUsers,
		Quota: &turn.Quota{
			// Applies to every user without a PerUser entry.
			Default: turn.UserQuota{
				MaxAllocations:    2,
				MaxBytesPerSecond: 1_000_000 / 8, // 1 Mbps
			},
			// Per-user overrides. An entry fully replaces Default for
			// that user; zero fields mean unlimited.
			PerUser: map[string]turn.UserQuota{
				"premium": {
					MaxAllocations:    10,
					MaxBytesPerSecond: 10_000_000 / 8, // 10 Mbps
				},
				"free": {
					MaxAllocations:    1,
					MaxBytesPerSecond: 128_000 / 8, // 128 Kbps
				},
			},
		},
	})
	if err != nil {
		log.Fatalf("start turn server: %v", err)
	}
	defer srv.Close()

	log.Printf("TURN server running, URLs: %v", srv.URLs())
	log.Printf("quotas: default 2 allocations @ 1 Mbps, premium 10 @ 10 Mbps, free 1 @ 128 Kbps")

	// Live usage is queryable at any time:
	//   srv.ActiveAllocations("premium")
	//   srv.TotalAllocations()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	<-sigs
}
