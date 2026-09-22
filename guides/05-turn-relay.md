# TURN relay

TURN relays traffic for peers that cannot reach each other directly. A peer
asks the relay for an **allocation** (a public `ip:port` on the relay), and ICE
uses it as a `relay` candidate when nothing better works. The relay sees only
DTLS ciphertext; it costs you bandwidth.

Each step gives a complete relay program (`relayd/main.go`) and the change to
the client's `iceServers`. The same relay is also a ready-made command:
`go run ella.to/pipe/examples/turnserver@latest -h`.

| Step | Adds |
| --- | --- |
| [1](#step-1-the-smallest-relay) | A local relay and a client forced through it |
| [2](#step-2-real-users) | One password per user |
| [3](#step-3-public-address) | Public address, relay port range, firewall |
| [4](#step-4-udp-and-tcp) | TURN over TCP for networks that block UDP |
| [5](#step-5-tiers-free-pro-unlimited) | Free, pro, and unlimited plans |
| [6](#step-6-ephemeral-credentials) | Expiring credentials from your API, carrying the tier |
| [7](#step-7-tiers-from-your-database) | Tier looked up on the relay instead |
| [8](#step-8-everything-together) | The final relay and client |

## Step 1: the smallest relay

```go
// relayd/main.go
package main

import (
	"context"
	"log"
	"os"
	"os/signal"

	"ella.to/pipe/relay"
)

func main() {
	srv, err := relay.Start(relay.Config{
		Listen: "127.0.0.1:3478",
		Users:  map[string]relay.User{"admin": {Password: "admin"}},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer srv.Close()
	log.Println(srv.STUNURL(), srv.TURNURL())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	<-ctx.Done()
}
```

The client is the `peer/main.go` from [02-quickstart.md](02-quickstart.md)
with two changes. Point `iceServers` at the relay:

```go
func iceServers() []pipe.ICEServer {
	return []pipe.ICEServer{
		{URLs: []string{"stun:127.0.0.1:3478"}},
		{
			URLs:       []string{"turn:127.0.0.1:3478?transport=udp"},
			Username:   "admin",
			Credential: "admin",
		},
	}
}
```

And force the relay while testing, by adding this to its `pipe.Config`:

```go
ICETransportPolicy: pipe.ICETransportPolicyRelay,
```

```sh
go run ./relayd
go run ./signal
go run ./peer listen bob
echo hi | go run ./peer dial alice bob
# connected via relay/relay
```

`relay/relay` proves the bytes went through TURN. Remove the relay-only policy
in production, so ICE uses direct paths when they exist.

## Step 2: real users

```go
// relayd/main.go
package main

import (
	"context"
	"log"
	"os"
	"os/signal"

	"ella.to/pipe/relay"
)

func main() {
	// PIPE_TURN_USERS="alice=<password>,bob=<password>"
	users, err := relay.ParseUsers(os.Getenv("PIPE_TURN_USERS"))
	if err != nil {
		log.Fatal(err)
	}

	srv, err := relay.Start(relay.Config{
		Listen: "127.0.0.1:3478",
		Realm:  "relay.example.net",
		Users:  users,
	})
	if err != nil {
		log.Fatal(err)
	}
	defer srv.Close()
	log.Println(srv.STUNURL(), srv.TURNURL())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	<-ctx.Done()
}
```

```go
func iceServers() []pipe.ICEServer {
	return []pipe.ICEServer{
		{URLs: []string{"stun:127.0.0.1:3478"}},
		{
			URLs:       []string{"turn:127.0.0.1:3478?transport=udp"},
			Username:   os.Getenv("PIPE_TURN_USERNAME"),
			Credential: os.Getenv("PIPE_TURN_PASSWORD"),
		},
	}
}
```

```sh
PIPE_TURN_USERS="alice=$(openssl rand -hex 16),bob=$(openssl rand -hex 16)" go run ./relayd
```

Clients do not configure the realm; the relay announces it.

## Step 3: public address

```go
// relayd/main.go
package main

import (
	"context"
	"log"
	"net"
	"os"
	"os/signal"

	"ella.to/pipe/relay"
)

func main() {
	users, err := relay.ParseUsers(os.Getenv("PIPE_TURN_USERS"))
	if err != nil {
		log.Fatal(err)
	}

	srv, err := relay.Start(relay.Config{
		Listen:  "0.0.0.0:3478",
		RelayIP: net.ParseIP("203.0.113.10"), // public IP; required with 0.0.0.0 or behind NAT
		MinPort: 49152,                       // relay sockets stay in a range you can open
		MaxPort: 49252,
		Realm:   "relay.example.net",
		Users:   users,
	})
	if err != nil {
		log.Fatal(err)
	}
	defer srv.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	<-ctx.Done()
}
```

```sh
ufw allow 3478/udp
ufw allow 49152:49252/udp
```

```go
func iceServers() []pipe.ICEServer {
	return []pipe.ICEServer{
		{URLs: []string{"stun:relay.example.net:3478"}},
		{
			URLs:       []string{"turn:relay.example.net:3478?transport=udp"},
			Username:   os.Getenv("PIPE_TURN_USERNAME"),
			Credential: os.Getenv("PIPE_TURN_PASSWORD"),
		},
	}
}
```

Test from a different network than the relay:

```sh
go run ella.to/pipe/examples/turnclient@latest -embedded=false \
    -turn 'turn:relay.example.net:3478?transport=udp' -user alice -pass "$ALICE_PW"
```

## Step 4: UDP and TCP

Add `ListenTCP`. Everything else stays.

```go
// relayd/main.go
package main

import (
	"context"
	"log"
	"net"
	"os"
	"os/signal"

	"ella.to/pipe/relay"
)

func main() {
	users, err := relay.ParseUsers(os.Getenv("PIPE_TURN_USERS"))
	if err != nil {
		log.Fatal(err)
	}

	srv, err := relay.Start(relay.Config{
		Listen:    "0.0.0.0:3478",
		ListenTCP: "0.0.0.0:3478",
		RelayIP:   net.ParseIP("203.0.113.10"),
		MinPort:   49152,
		MaxPort:   49252,
		Realm:     "relay.example.net",
		Users:     users,
	})
	if err != nil {
		log.Fatal(err)
	}
	defer srv.Close()
	log.Println(srv.TURNURL(), srv.TURNTCPURL())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	<-ctx.Done()
}
```

```sh
ufw allow 3478/tcp
```

Both transports go in one `ICEServer`. ICE prefers UDP when it works.

```go
func iceServers() []pipe.ICEServer {
	return []pipe.ICEServer{
		{URLs: []string{"stun:relay.example.net:3478"}},
		{
			URLs: []string{
				"turn:relay.example.net:3478?transport=udp",
				"turn:relay.example.net:3478?transport=tcp",
			},
			Username:   os.Getenv("PIPE_TURN_USERNAME"),
			Credential: os.Getenv("PIPE_TURN_PASSWORD"),
		},
	}
}
```

Only the client-to-relay leg is TCP. For `turns:` (TLS), see
[TLS](#tls-turns).

## Step 5: tiers: free, pro, unlimited

A `relay.Plan` caps each relay socket's rate (bytes per second per direction)
and how many sockets a user may hold. Zero means unlimited.

```go
// relayd/main.go
package main

import (
	"context"
	"log"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"time"

	"ella.to/pipe/relay"
)

func main() {
	srv, err := relay.Start(relay.Config{
		Listen:    "0.0.0.0:3478",
		ListenTCP: "0.0.0.0:3478",
		RelayIP:   net.ParseIP("203.0.113.10"),
		MinPort:   49152,
		MaxPort:   49252,
		Realm:     "relay.example.net",
		Plans: map[string]relay.Plan{
			"free":      {Rate: 512 << 10, MaxAllocations: 4}, // 512 KiB/s
			"pro":       {Rate: 8 << 20, MaxAllocations: 32},  // 8 MiB/s
			"unlimited": {},                                   // no caps
		},
		DefaultPlan: relay.Plan{Rate: 256 << 10, MaxAllocations: 2}, // users without a plan
		Users: map[string]relay.User{
			"alice": {Password: os.Getenv("ALICE_PW"), Plan: "free"},
			"bob":   {Password: os.Getenv("BOB_PW"), Plan: "pro"},
			"ops":   {Password: os.Getenv("OPS_PW"), Plan: "unlimited"},
			"guest": {Password: os.Getenv("GUEST_PW")},
		},
		Logger: slog.Default(),
	})
	if err != nil {
		log.Fatal(err)
	}
	defer srv.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			st := srv.Stats()
			for _, id := range relay.SortedUsers(st) {
				log.Printf("%s %s", id, st.Users[id])
			}
		case <-ctx.Done():
			return
		}
	}
}
```

The client does not change: the tier belongs to the credential.

```
alice plan=free allocations=2 active=2 sent=2.2MiB received=2.2MiB dropped=352.3KiB/301pkt rejected=0
bob plan=pro allocations=1 active=1 sent=10.0KiB received=313.8KiB dropped=0B/0pkt rejected=0
```

`dropped` growing means the user is at the rate cap; `rejected` growing means
they hit `MaxAllocations`.

Compare tiers on one machine (embedded relay, plan chosen by `name@plan`):

```sh
for p in free pro unlimited; do
  go run ella.to/pipe/examples/turnclient@latest -plans 'free=512KiB,pro=8MiB,unlimited=0' -user alice@$p -bytes 1MiB
done
# free       throughput   62.6KiB/s each way
# pro        throughput   3.8MiB/s each way
# unlimited  throughput   30.5MiB/s each way
```

Measured throughput is below the plan rate because that test echoes, so every
byte crosses the relay four times. See [the budget](#how-the-throughput-budget-works).

## Step 6: ephemeral credentials

Static passwords never expire. Instead, share a secret between the relay and
your API. The API mints `<expiry>:<user id>` plus an HMAC password; the relay
verifies it with no user list. The tier rides in the user ID as `name@plan`.

The relay: replace `Users` with `AuthSecret`.

```go
// relayd/main.go
package main

import (
	"context"
	"log"
	"log/slog"
	"net"
	"os"
	"os/signal"

	"ella.to/pipe/relay"
)

func main() {
	srv, err := relay.Start(relay.Config{
		Listen:     "0.0.0.0:3478",
		ListenTCP:  "0.0.0.0:3478",
		RelayIP:    net.ParseIP("203.0.113.10"),
		MinPort:    49152,
		MaxPort:    49252,
		Realm:      "relay.example.net",
		AuthSecret: os.Getenv("PIPE_TURN_SECRET"),
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	<-ctx.Done()
}
```

Your API: a signed-in user asks for relay credentials.

```go
// api/main.go
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"ella.to/pipe/relay"
)

type account struct {
	ID          string
	Paid, Staff bool
}

// Replace with your own session lookup.
var sessions = map[string]account{
	"alice-session": {ID: "alice"},
	"bob-session":   {ID: "bob", Paid: true},
}

type relayCreds struct {
	STUN       []string  `json:"stun"`
	TURN       []string  `json:"turn"`
	Username   string    `json:"username"`
	Credential string    `json:"credential"`
	ExpiresAt  time.Time `json:"expires_at"`
}

func main() {
	secret := os.Getenv("PIPE_TURN_SECRET")

	http.HandleFunc("GET /relay-credentials", func(w http.ResponseWriter, r *http.Request) {
		session, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		acct, ok := sessions[session]
		if !ok {
			http.Error(w, "sign in first", http.StatusUnauthorized)
			return
		}

		plan := "free"
		switch {
		case acct.Staff:
			plan = "unlimited"
		case acct.Paid:
			plan = "pro"
		}

		const ttl = 12 * time.Hour
		user, pass, err := relay.IssueCredentials(secret, acct.ID+"@"+plan, ttl)
		if err != nil {
			http.Error(w, "could not issue credentials", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(relayCreds{
			STUN: []string{"stun:relay.example.net:3478"},
			TURN: []string{
				"turn:relay.example.net:3478?transport=udp",
				"turn:relay.example.net:3478?transport=tcp",
			},
			Username:   user,
			Credential: pass,
			ExpiresAt:  time.Now().Add(ttl),
		})
	})

	log.Fatal(http.ListenAndServe("127.0.0.1:9000", nil))
}
```

The client fetches credentials before `pipe.New`. Add these to `peer/main.go`
and pass `iceServers(creds)` in the config:

```go
type relayCreds struct {
	STUN       []string  `json:"stun"`
	TURN       []string  `json:"turn"`
	Username   string    `json:"username"`
	Credential string    `json:"credential"`
	ExpiresAt  time.Time `json:"expires_at"`
}

func fetchRelayCreds(ctx context.Context, url, session string) (relayCreds, error) {
	var c relayCreds
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return c, err
	}
	req.Header.Set("Authorization", "Bearer "+session)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return c, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return c, fmt.Errorf("relay credentials: %s", resp.Status)
	}
	return c, json.NewDecoder(resp.Body).Decode(&c)
}

func iceServers(c relayCreds) []pipe.ICEServer {
	return []pipe.ICEServer{
		{URLs: c.STUN},
		{URLs: c.TURN, Username: c.Username, Credential: c.Credential},
	}
}
```

Mint one by hand for scripts and tests:

```sh
PIPE_TURN_SECRET=... go run ella.to/pipe/examples/turncred@latest -user alice@pro -ttl 1h
```

- **New credentials mean a new `Endpoint`**: `pipe.New` copies the config.
  Pick a TTL longer than an endpoint's lifetime, or rebuild before
  `ExpiresAt`.
- **Upgrade**: issue `alice@pro` instead of `alice@free` and rebuild. Live
  connections keep the plan they were allocated with.
- **Revoke everyone**: restart the relay with a new `PIPE_TURN_SECRET`.

## Step 7: tiers from your database

Keep user IDs plain (`alice`) and resolve the plan on the relay with
`PlanFor`. It runs per allocation, never per packet, but keep it cheap.

```go
// relayd/main.go
package main

import (
	"context"
	"log"
	"net"
	"os"
	"os/signal"
	"sync"

	"ella.to/pipe/relay"
)

// accounts is refreshed from your database in the background.
var (
	mu       sync.RWMutex
	accounts = map[string]string{"alice": "free", "bob": "pro", "ops": "unlimited"}
)

func planFor(userID string) string {
	mu.RLock()
	defer mu.RUnlock()
	return accounts[userID] // "" selects DefaultPlan
}

func main() {
	srv, err := relay.Start(relay.Config{
		Listen:     "0.0.0.0:3478",
		ListenTCP:  "0.0.0.0:3478",
		RelayIP:    net.ParseIP("203.0.113.10"),
		MinPort:    49152,
		MaxPort:    49252,
		Realm:      "relay.example.net",
		AuthSecret: os.Getenv("PIPE_TURN_SECRET"),
		Plans: map[string]relay.Plan{
			"free":      {Rate: 512 << 10, MaxAllocations: 4},
			"pro":       {Rate: 8 << 20, MaxAllocations: 32},
			"unlimited": {},
		},
		DefaultPlan: relay.Plan{Rate: 256 << 10, MaxAllocations: 2},
		PlanFor:     planFor,
	})
	if err != nil {
		log.Fatal(err)
	}
	defer srv.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	<-ctx.Done()
}
```

The API then mints `relay.IssueCredentials(secret, acct.ID, ttl)` without the
`@plan` suffix. The client does not change.

## Step 8: everything together

The relay: UDP and TCP, public address, ephemeral credentials, an ops user
with a static password, three tiers, a stats endpoint, and graceful shutdown.

```go
// relayd/main.go
//
//	PIPE_TURN_SECRET=$(openssl rand -hex 32) OPS_TURN_PW=$(openssl rand -hex 16) \
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

	"ella.to/pipe/relay"
)

func main() {
	publicIP := net.ParseIP(os.Getenv("RELAY_PUBLIC_IP"))
	if publicIP == nil {
		log.Fatal("set RELAY_PUBLIC_IP")
	}

	srv, err := relay.Start(relay.Config{
		Listen:     "0.0.0.0:3478",
		ListenTCP:  "0.0.0.0:3478",
		RelayIP:    publicIP,
		MinPort:    49152,
		MaxPort:    49252,
		Realm:      "relay.example.net",
		AuthSecret: os.Getenv("PIPE_TURN_SECRET"),
		Users: map[string]relay.User{
			"ops": {Password: os.Getenv("OPS_TURN_PW"), Plan: "unlimited"},
		},
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

	// Private stats endpoint; keep it off the public interface.
	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/stats", func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(srv.Stats())
		})
		log.Println(http.ListenAndServe("127.0.0.1:9100", mux))
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	<-ctx.Done()
}
```

```sh
ufw allow 3478/udp
ufw allow 3478/tcp
ufw allow 49152:49252/udp
```

The client: credentials from your API, UDP and TCP, direct paths preferred.

```go
// peer/main.go
//
//	PIPE_SESSION=alice-session go run ./peer listen alice
//	echo hi | PIPE_SESSION=bob-session go run ./peer dial bob alice
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"ella.to/pipe"
	"ella.to/pipe/signaling/sse"
)

type relayCreds struct {
	STUN       []string  `json:"stun"`
	TURN       []string  `json:"turn"`
	Username   string    `json:"username"`
	Credential string    `json:"credential"`
	ExpiresAt  time.Time `json:"expires_at"`
}

func fetchRelayCreds(ctx context.Context, url, session string) (relayCreds, error) {
	var c relayCreds
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return c, err
	}
	req.Header.Set("Authorization", "Bearer "+session)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return c, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return c, fmt.Errorf("relay credentials: %s", resp.Status)
	}
	return c, json.NewDecoder(resp.Body).Decode(&c)
}

func main() {
	if len(os.Args) < 3 {
		log.Fatal("usage: peer listen <id> | peer dial <id> <peer>")
	}
	ctx := context.Background()
	mode, id := os.Args[1], pipe.PeerID(os.Args[2])

	creds, err := fetchRelayCreds(ctx, envOr("PIPE_API_URL", "http://127.0.0.1:9000/relay-credentials"),
		os.Getenv("PIPE_SESSION"))
	if err != nil {
		log.Fatal(err)
	}

	ep, err := pipe.New(ctx, pipe.Config{
		ID: id,
		Signaler: &sse.Client{
			URL:   envOr("PIPE_SIGNAL_URL", "http://127.0.0.1:8080/pipe"),
			Token: os.Getenv("PIPE_SIGNAL_TOKEN"),
		},
		ICEServers: []pipe.ICEServer{
			{URLs: creds.STUN},
			{URLs: creds.TURN, Username: creds.Username, Credential: creds.Credential},
		},
		ICETransportPolicy: pipe.ICETransportPolicyAll,
	})
	if err != nil {
		log.Fatal(err)
	}
	defer ep.Close()

	switch mode {
	case "listen":
		ln, err := ep.Listen()
		if err != nil {
			log.Fatal(err)
		}
		for {
			conn, err := ln.AcceptConn()
			if err != nil {
				log.Fatal(err)
			}
			go func() {
				defer conn.Close()
				st := conn.Stats()
				log.Printf("%s via %s/%s", conn.PeerID(), st.LocalCandidate, st.RemoteCandidate)
				_, _ = io.Copy(os.Stdout, conn)
			}()
		}
	case "dial":
		conn, err := ep.Dial(ctx, pipe.PeerID(os.Args[3]))
		if err != nil {
			log.Fatal(err)
		}
		defer conn.Close()
		st := conn.Stats()
		log.Printf("via %s/%s", st.LocalCandidate, st.RemoteCandidate)
		_, _ = io.Copy(conn, os.Stdin)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
```

Plans in depth, quotas, and a combined relay-plus-API program:
[07-multi-user-relay.md](07-multi-user-relay.md).

---

## Reference

### `relay.Config`

| Field | Default | Meaning |
| --- | --- | --- |
| `Listen` | `127.0.0.1:0` | UDP address for STUN and TURN |
| `ListenTCP` | off | TCP address for TURN over TCP |
| `Realm` | `pipe.example` | Part of the credential digest; announced to clients |
| `Users` | none | Static users: `map[username]relay.User{Password, Plan}` |
| `AuthSecret` | off | Enables ephemeral credentials (`relay.IssueCredentials`) |
| `Plans` | none | `map[name]relay.Plan{Rate, Burst, MaxDelay, MaxAllocations}` |
| `DefaultPlan` | unlimited | Plan for users without one |
| `PlanFor` | `Users[id].Plan`, then the `name@plan` suffix | Custom plan lookup by user ID |
| `RelayIP` | the listen IP | Address advertised to clients; required with `0.0.0.0` or behind NAT |
| `MinPort`, `MaxPort` | kernel-chosen | Relay socket port range |
| `Logger` | discard | Logs usernames and relay addresses, never passwords |

`relay.Server`: `STUNURL()`, `TURNURL()`, `TURNTCPURL()`, `Stats()`,
`IssueCredentials(userID, ttl)`, `Close()`. Helpers: `ParseUsers`,
`ParsePlans`, `ParseSize`, `ParsePortRange`, `FormatSize`, `SortedUsers`.

### turnserver command

The same relay as flags:

```sh
go run ella.to/pipe/examples/turnserver@latest \
    -listen 0.0.0.0:3478 -listen-tcp 0.0.0.0:3478 \
    -relay-ip 203.0.113.10 -relay-ports 49152-49252 \
    -realm relay.example.net \
    -plans 'free=512KiB/4,pro=8MiB/32,unlimited=0' \
    -rate 256KiB -max-allocations 2 \
    -stats 60s
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `-listen` | `127.0.0.1:3478` | UDP address |
| `-listen-tcp` | off | TCP address |
| `-realm` | `pipe.example` | Realm |
| `-users` | `admin=admin` | `user=password[:plan],...`; `PIPE_TURN_USERS` overrides |
| `-auth-secret` | off | Shared secret; `PIPE_TURN_SECRET` overrides |
| `-plans` | none | `name=rate[/maxallocations],...`; rate `0` is unlimited |
| `-relay-ip` | listen address | Public relay address |
| `-relay-ports` | kernel-chosen | `min-max` |
| `-rate`, `-burst`, `-max-delay`, `-max-allocations` | unlimited | Default plan |
| `-stats` | off | Log per-user counters at this interval |

Sizes accept `512KiB`, `8MiB`, `1MB`, or plain bytes.

### Firewall

| Direction | Protocol | Ports |
| --- | --- | --- |
| Inbound | UDP | 3478 |
| Inbound | TCP | 3478 (with `ListenTCP`) |
| Inbound | UDP | the relay range |
| Outbound | UDP | any |

One relay socket per relaying side of each connection.

### systemd

```sh
CGO_ENABLED=0 go build -o /usr/local/bin/relayd ./relayd
```

```ini
# /etc/systemd/system/relayd.service
[Unit]
After=network-online.target
Wants=network-online.target

[Service]
User=turn
EnvironmentFile=/etc/relayd.env
ExecStart=/usr/local/bin/relayd
Restart=always
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true

[Install]
WantedBy=multi-user.target
```

```sh
# /etc/relayd.env (mode 0600)
PIPE_TURN_SECRET=...
OPS_TURN_PW=...
RELAY_PUBLIC_IP=203.0.113.10
```

### Behind NAT

Forward UDP 3478, TCP 3478, and the relay range, and set `RelayIP` to the
NAT's public address.

### TLS (`turns:`)

`relay` does not terminate TLS. Put a TCP TLS terminator (HAProxy `mode tcp`,
`stunnel`) on 5349 in front of `ListenTCP`, or use coturn. Then:

```go
URLs: []string{
	"turn:relay.example.net:3478?transport=udp",
	"turn:relay.example.net:3478?transport=tcp",
	"turns:relay.example.net:5349?transport=tcp",
},
```

### coturn

coturn accepts the same ephemeral credentials. It has no per-user plans:
`max-bps` and `user-quota` apply to everyone. Full hardened configuration:
`examples/docker/coturn/turnserver.conf`. The essentials:

```
listening-port=3478
realm=relay.example.net
external-ip=203.0.113.10
use-auth-secret
static-auth-secret=<same as PIPE_TURN_SECRET>
min-port=49152
max-port=49252
user-quota=8
max-bps=1048576
no-multicast-peers
denied-peer-ip=10.0.0.0-10.255.255.255
denied-peer-ip=172.16.0.0-172.31.255.255
denied-peer-ip=192.168.0.0-192.168.255.255
denied-peer-ip=127.0.0.0-127.255.255.255
tls-listening-port=5349
cert=/etc/letsencrypt/live/relay.example.net/fullchain.pem
pkey=/etc/letsencrypt/live/relay.example.net/privkey.pem
```

### How the throughput budget works

Each relay socket gets one token bucket per direction at the plan's rate.
Client-to-peer traffic over budget is dropped immediately; peer-to-client
traffic may wait up to `MaxDelay` (20 ms) and is then dropped. SCTP sees real
congestion and slows down. The rate is per socket per direction, so an echo
between two relayed peers crosses four buckets and measures well below the
rate; a one-way transfer through one relayed side comes close to it.

### Troubleshooting

| Symptom | Cause |
| --- | --- |
| Relay logs `turn: rejected an allocation` | Wrong password, expired credential, or a different secret |
| Allocation succeeds, no bytes flow | `RelayIP` is not the public address, or the relay range is closed |
| `host/host` although you expected `relay` | Policy is `All` and a direct path worked; that is correct |
| `ICETransportPolicy relay requires a TURN server` | Relay-only policy without a `turn:` URL |
| `listening on a wildcard address requires an explicit RelayIP` | Set `RelayIP` |
| `allocation quota reached` | User at `MaxAllocations` |
| One user never connects | UDP blocked on their network: add `?transport=tcp` |
| Works locally, fails on the internet | Test with `turnclient -embedded=false` from another network |
