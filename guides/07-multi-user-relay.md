# Multi-user relay: plans, credentials, stats

Builds on [05-turn-relay.md](05-turn-relay.md). One relay serves many users,
each with a **plan**:

```go
type Plan struct {
	Rate           int64         // bytes/s per relay socket per direction; 0 = unlimited
	Burst          int64         // token bucket depth; default Rate/10, at least 64 KiB
	MaxDelay       time.Duration // how long a packet may wait for budget; default 20ms
	MaxAllocations int           // concurrent relay sockets per user; 0 = unlimited
}
```

Plan resolution for an authenticated user ID:

1. `Config.PlanFor(userID)` if set.
2. `Config.Users[userID].Plan` for static users.
3. The `@plan` suffix, when it names a known plan: `alice@pro` gets `pro`.
4. `Config.DefaultPlan`, reported as `default`.

## Step 1: see the tiers on one machine

A relay on loopback, one credential per tier, 1 MiB sent one way through the
relay for each.

```go
// tiers/main.go
package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log"
	"time"

	"ella.to/pipe"
	"ella.to/pipe/relay"
	"ella.to/pipe/signaling/memory"
)

const secret = "demo-secret"

func main() {
	srv, err := relay.Start(relay.Config{
		Listen:     "127.0.0.1:0",
		AuthSecret: secret,
		Plans: map[string]relay.Plan{
			"free":      {Rate: 512 << 10, MaxAllocations: 4},
			"pro":       {Rate: 8 << 20, MaxAllocations: 32},
			"unlimited": {},
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer srv.Close()

	for _, plan := range []string{"free", "pro", "unlimited"} {
		elapsed, err := send(srv, "alice@"+plan, 1<<20)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("%-9s 1 MiB in %v\n", plan, elapsed.Round(time.Millisecond))
	}

	st := srv.Stats()
	for _, id := range relay.SortedUsers(st) {
		fmt.Println(id, st.Users[id])
	}
}

// send moves size bytes between two endpoints forced through the relay with
// credentials for userID, and returns how long delivery took.
func send(srv *relay.Server, userID string, size int64) (time.Duration, error) {
	user, pass, err := relay.IssueCredentials(secret, userID, time.Hour)
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	hub := memory.New()
	config := func(id pipe.PeerID) pipe.Config {
		return pipe.Config{
			ID:                 id,
			Signaler:           hub,
			ICEServers:         []pipe.ICEServer{{URLs: []string{srv.TURNURL()}, Username: user, Credential: pass}},
			ICETransportPolicy: pipe.ICETransportPolicyRelay,
		}
	}

	sink, err := pipe.New(ctx, config("sink"))
	if err != nil {
		return 0, err
	}
	defer sink.Close()
	source, err := pipe.New(ctx, config("source"))
	if err != nil {
		return 0, err
	}
	defer source.Close()

	ln, err := sink.Listen()
	if err != nil {
		return 0, err
	}
	received := make(chan time.Time, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		if _, err := io.CopyN(io.Discard, conn, size); err == nil {
			received <- time.Now()
		}
	}()

	conn, err := source.Dial(ctx, "sink")
	if err != nil {
		return 0, err
	}
	defer conn.Close()

	start := time.Now()
	if _, err := io.CopyN(conn, rand.Reader, size); err != nil {
		return 0, err
	}
	select {
	case end := <-received:
		return end.Sub(start), nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}
```

```sh
go run ./tiers
# free      1 MiB in 15.772s
# pro       1 MiB in 218ms
# unlimited 1 MiB in 29ms
# alice@free plan=free allocations=2 active=0 sent=1.1MiB received=1.1MiB dropped=330.6KiB/281pkt rejected=0
# alice@pro plan=pro allocations=2 active=0 sent=1.1MiB received=1.1MiB dropped=84.9KiB/71pkt rejected=0
# alice@unlimited plan=unlimited allocations=2 active=0 sent=1.1MiB received=1.1MiB dropped=0B/0pkt rejected=0
```

The rate is a ceiling, not a promise: over-budget packets are dropped, SCTP
backs off, and a capped plan delivers well under its `Rate`. Tune `Burst` and
`MaxDelay` if you need delivered throughput closer to it. The counter fields
are explained under [Step 4](#step-4-per-user-statistics).

## Step 2: relay, credentials API, and stats in one binary

```go
// relayd/main.go
//
//	PIPE_TURN_SECRET=$(openssl rand -hex 32) ADMIN_TOKEN=$(openssl rand -hex 24) \
//	RELAY_PUBLIC_IP=203.0.113.10 go run ./relayd
package main

import (
	"context"
	"encoding/json"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"ella.to/pipe/relay"
)

type account struct {
	ID          string
	Paid, Staff bool
}

// accountFor authenticates the caller. Replace with your session lookup.
func accountFor(r *http.Request) (account, bool) {
	sessions := map[string]account{
		"alice-session": {ID: "alice"},
		"bob-session":   {ID: "bob", Paid: true},
		"ops-session":   {ID: "ops", Staff: true},
	}
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	acct, ok := sessions[token]
	return acct, ok
}

func planOf(a account) string {
	switch {
	case a.Staff:
		return "unlimited"
	case a.Paid:
		return "pro"
	default:
		return "free"
	}
}

func main() {
	secret := os.Getenv("PIPE_TURN_SECRET")
	publicIP := net.ParseIP(os.Getenv("RELAY_PUBLIC_IP"))
	if secret == "" || publicIP == nil {
		log.Fatal("set PIPE_TURN_SECRET and RELAY_PUBLIC_IP")
	}

	srv, err := relay.Start(relay.Config{
		Listen:     "0.0.0.0:3478",
		ListenTCP:  "0.0.0.0:3478",
		RelayIP:    publicIP,
		MinPort:    49152,
		MaxPort:    49252,
		Realm:      "relay.example.net",
		AuthSecret: secret,
		Plans: map[string]relay.Plan{
			"free":      {Rate: 512 << 10, MaxAllocations: 4},
			"pro":       {Rate: 8 << 20, MaxAllocations: 32},
			"unlimited": {},
		},
		DefaultPlan: relay.Plan{Rate: 256 << 10, MaxAllocations: 2},
		Logger:      slog.Default(),
	})
	if err != nil {
		log.Fatal(err)
	}
	defer srv.Close()

	mux := http.NewServeMux()

	mux.HandleFunc("GET /relay-credentials", func(w http.ResponseWriter, r *http.Request) {
		acct, ok := accountFor(r)
		if !ok {
			http.Error(w, "sign in first", http.StatusUnauthorized)
			return
		}
		const ttl = time.Hour
		user, pass, err := srv.IssueCredentials(acct.ID+"@"+planOf(acct), ttl)
		if err != nil {
			http.Error(w, "could not issue credentials", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"stun": []string{"stun:relay.example.net:3478"},
			"turn": []string{
				"turn:relay.example.net:3478?transport=udp",
				"turn:relay.example.net:3478?transport=tcp",
			},
			"username":   user,
			"credential": pass,
			"expires_at": time.Now().Add(ttl),
		})
	})

	mux.HandleFunc("GET /admin/stats", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+os.Getenv("ADMIN_TOKEN") {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(srv.Stats())
	})

	httpServer := &http.Server{Addr: ":9000", Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	<-ctx.Done()
	_ = httpServer.Shutdown(context.Background())
}
```

```sh
curl -H 'Authorization: Bearer bob-session' http://127.0.0.1:9000/relay-credentials
curl -H "Authorization: Bearer $ADMIN_TOKEN" http://127.0.0.1:9000/admin/stats
```

The client is the final `peer/main.go` from
[05-turn-relay.md](05-turn-relay.md#step-8-everything-together), with
`PIPE_API_URL=http://relay.example.net:9000/relay-credentials`.

## Step 3: quotas

`MaxAllocations` counts relay sockets a user holds right now.

- One allocation per relaying side of each connection.
- A client with a TURN URL allocates while gathering, even if ICE ends up
  direct. Budget one per connection per user.
- An ICE restart may allocate again briefly.

So `free` with `MaxAllocations: 4` is about four concurrent connections. A
refused allocation logs `turn: allocation quota reached`, counts in
`rejected`, and the client's dial falls back to a direct path or times out.

## Step 4: per-user statistics

```go
st := srv.Stats()
for _, id := range relay.SortedUsers(st) {
	u := st.Users[id]
	fmt.Println(id, u.Plan, u.Active, u.SentBytes, u.ReceivedBytes, u.DroppedBytes, u.RejectedAllocations)
}
```

| Field | Meaning |
| --- | --- |
| `Plan` | Plan the user resolved to |
| `Allocations`, `Active` | Relay sockets created since start, and open now |
| `SentBytes`, `ReceivedBytes` | Relayed toward peers, and toward the user |
| `DroppedBytes`, `DroppedPackets` | Over the plan's rate |
| `RejectedAllocations` | Refused by `MaxAllocations` |

`Stats` also has server-wide totals (`Allocations`, `Active`, `SentBytes`,
`DroppedBytes`, `DelayedPackets`, and so on) for your metrics exporter.

## Step 5: changing plans and revoking

| Action | How |
| --- | --- |
| Upgrade or downgrade | Issue `alice@pro` instead of `alice@free`; the client rebuilds its endpoint. Live connections keep their old plan |
| Change plan with `PlanFor` | Update your lookup; applies to new allocations |
| Revoke one user | Stop issuing to them; their last credential dies at its TTL |
| Revoke everyone | Restart with a new `AuthSecret` |
| Static users | Edit `Users` and restart |

Keep TTLs short (an hour for real users). An expired credential fails at the
next allocation refresh, within 10 minutes.

## Tiers with coturn

coturn accepts the same credentials but applies `max-bps` and `user-quota` to
everyone. For tiers, run one instance per tier on different ports and port
ranges, each with its own secret, and hand paying users the second URL:

```
# free: turnserver-free.conf
listening-port=3478
min-port=49152
max-port=49252
max-bps=524288
user-quota=4
use-auth-secret
static-auth-secret=<free secret>

# pro: turnserver-pro.conf
listening-port=3479
min-port=49300
max-port=49400
max-bps=8388608
user-quota=32
use-auth-secret
static-auth-secret=<pro secret>
```

## Abuse controls

- Every plan sets `MaxAllocations`; untrusted plans set a `Rate`.
- Block relaying into private networks at the firewall
  ([06-security.md](06-security.md#5-harden-the-relay)).
- Cap signaling registrations with `sse.Config.MaxPeers`, and refuse unknown
  callers with `pipe.Config.AllowPeer` before any relay socket is spent.
- Alert on `DroppedBytes` and `RejectedAllocations` growing for one user.
