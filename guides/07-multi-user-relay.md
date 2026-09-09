# One relay, many users: plans, quotas, and ephemeral credentials

This guide is for anyone running a TURN relay for more than themselves: a
personal project with a few friends, or a small service with a free tier and a
paid one. It shows how the example relay assigns every authenticated user a
**plan** (a throughput cap and an allocation quota), works through the
concrete case of free users capped at 512 KiB/s and paying users at 8 MiB/s,
explains how to hand out credentials that expire on their own instead of
passwords that live forever, and covers upgrades, revocation, per-user
statistics, the equivalent with coturn, and abuse controls. It assumes you
have the relay itself running; see [05-turn-relay.md](05-turn-relay.md).

## The problem

Signaling and STUN are nearly free. TURN is not: every relayed byte enters
your server and leaves it again, and you pay for both. Without limits, one
user syncing a disk image through the relay consumes the bandwidth of
everyone else. Any relay that serves more than one person therefore needs to
answer three questions per user:

1. Who are you? (authentication)
2. How fast may your relayed traffic go? (throughput budget)
3. How many relay sockets may you hold at once? (allocation quota)

The example server answers all three with one concept, the plan.

## The plan model

In `examples/internal/turnx`:

```go
// Plan is the relay budget granted to a class of users.
type Plan struct {
	// Rate limits each allocated relay socket, in bytes per second per
	// direction. Zero means unlimited.
	Rate int64

	// Burst is the token bucket depth in bytes. It defaults to a tenth of a
	// second of Rate, and is never smaller than 64 KiB.
	Burst int64

	// MaxDelay bounds how long an inbound relayed packet may be held waiting for
	// budget. Packets that would wait longer are dropped. It defaults to 20ms;
	// a zero-length delay is expressed as a negative value.
	MaxDelay time.Duration

	// MaxAllocations bounds the relay sockets one user may hold at a time. A
	// pipe connection uses one allocation per side. Zero means unlimited.
	MaxAllocations int
}

// User is one entry in the static credential table.
type User struct {
	Password string
	Plan     string // names an entry in Config.Plans; empty selects DefaultPlan
}
```

and in `Config`:

| Field | Meaning |
| --- | --- |
| `Users map[string]User` | Static credentials, each with an optional plan. |
| `AuthSecret string` | Enables ephemeral credentials (below). |
| `Plans map[string]Plan` | The named plans. |
| `DefaultPlan Plan` | Applies to users without a plan. Zero value means unlimited. |
| `PlanFor func(userID string) string` | Optional hook choosing the plan name for an authenticated user ID. |

Plan resolution for an authenticated user ID, in order:

1. If `PlanFor` is set, its result is used.
2. Otherwise, if the ID is a static user with a `Plan`, that plan.
3. Otherwise, if the ID has the form `name@plan` and `plan` is a known plan
   name, that plan. This is how ephemeral credentials carry their tier.
4. Otherwise `DefaultPlan`, reported in statistics under the name `default`.

The server validates at startup that every static user's plan exists and
that no plan name is empty.

Rates are enforced by a token bucket per relay socket per direction; the
mechanism, and why it produces real congestion rather than a reported number,
is described in [05-turn-relay.md](05-turn-relay.md). Quotas are enforced
before an allocation is created, through pion/turn's `QuotaHandler`, using a
live count of the user's open allocations.

## The worked example: free at 512 KiB/s, paid at 8 MiB/s

Two plans:

- **free**: 512 KiB/s per relay socket per direction, at most 4 concurrent
  allocations (two fully relayed pipe connections).
- **paid**: 8 MiB/s per relay socket per direction, at most 32 allocations.

### With static users

```sh
export PIPE_TURN_USERS="alice=$(openssl rand -hex 16):free,bob=$(openssl rand -hex 16):paid"
go run ./examples/turnserver \
    -listen 0.0.0.0:3478 -relay-ip 203.0.113.10 -relay-ports 49152-49252 \
    -plans 'free=512KiB/4,paid=8MiB/32' \
    -stats 60s
```

The startup banner lists the plans:

```
stun:  stun:0.0.0.0:3478
turn:  turn:0.0.0.0:3478?transport=udp
realm: pipe.example
users: alice, bob
plan free:    512.0KiB/s, 4 allocations
plan paid:    8.0MiB/s, 32 allocations
```

The `-plans` syntax is `name=rate[/maxallocations]`, comma-separated. A rate
of `0` is unlimited; omitting `/N` leaves allocations unlimited. Users
without a plan fall back to the default plan built from `-rate`, `-burst`,
`-max-delay`, and `-max-allocations`.

### Seeing the difference

`examples/turnclient` runs a relay in-process with the same plan machinery,
so you can compare tiers on one machine. The plan is selected by the user ID
suffix:

```sh
go run ./examples/turnclient -plans 'free=512KiB,paid=8MiB' -user alice@free -bytes 1MiB
go run ./examples/turnclient -plans 'free=512KiB,paid=8MiB' -user alice@paid -bytes 1MiB
```

Measured on one laptop, free plan:

```
user:        alice@free (realm pipe.example)
policy:      relay
plan paid:   8.0MiB/s
plan free:   512.0KiB/s
transfer:    1.0MiB (echoed, so every byte crosses the relay four times)

connected in 9ms
transferred  1.0MiB round trip in 12.752s
throughput   80.3KiB/s each way
state        connected, candidates relay/relay, read 1.0MiB, written 1.0MiB

relay        allocations=2 active=2 sent=2.2MiB/2729pkt received=2.2MiB/2729pkt dropped=330.1KiB/292pkt delayed=7pkt
user alice@free plan=free allocations=2 active=2 sent=2.2MiB received=2.2MiB dropped=330.1KiB/292pkt rejected=0
```

Paid plan, same transfer:

```
user:        alice@paid (realm pipe.example)
...
transferred  1.0MiB round trip in 73ms
throughput   13.7MiB/s each way
state        connected, candidates relay/relay, read 1.0MiB, written 1.0MiB

relay        allocations=2 active=2 sent=2.2MiB/2585pkt received=2.2MiB/2585pkt dropped=50.5KiB/43pkt delayed=0pkt
user alice@paid plan=paid allocations=2 active=2 sent=2.2MiB received=2.2MiB dropped=50.5KiB/43pkt rejected=0
```

Runs vary: the same free-plan command has measured anywhere from about 60 to
100 KiB/s each way on the same machine, because how many datagrams the token
bucket drops depends on timing, and SCTP's backoff amplifies each drop.

The per-user line is the one to watch in production: `dropped` says how hard
a user is pushing against the cap, `rejected` how often the quota refused an
allocation.

### Why 512 KiB/s shows up as 80 KiB/s

The cap is per relay socket per direction, and the number a client measures
depends on how many times its bytes cross the relay:

- In the echo test both peers relay through the same server, so a byte goes
  in through alice's allocation, out through bob's, and back the same way:
  **four** metered crossings, each with its own bucket.
- Dropped datagrams make SCTP inside the pipe connection back off, exactly as
  on a congested link, so the average rate settles below the cap even on a
  single crossing.
- The relay counters show the truth: 2.2 MiB metered for a 1 MiB echo, of
  which 330 KiB was dropped.

A one-way transfer where only one side relays crosses the server twice (in
and out of one allocation) and sees throughput much closer to the plan rate.
When you describe a tier to users, describe it as the rate at which the
relay forwards their traffic, and expect the application-level number to be
lower.

## Static users versus ephemeral credentials

Static users (`-users`) are fine for a handful of people you know. They have
two costs: the password never expires, and every change means editing the
list and restarting the relay.

The alternative is the **TURN REST API** credential scheme
(draft-uberti-behave-turn-rest), the same one coturn implements as
`use-auth-secret`:

- The server and your own service share a secret.
- A credential is a username of the form `<unix expiry>:<user id>` and a
  password that is the base64 HMAC-SHA1 of that username under the secret.
- The server verifies any username it has never seen by recomputing the HMAC
  and checking the clock. No database, no restart, no user list.

Enable it on the example server:

```sh
export PIPE_TURN_SECRET=$(openssl rand -hex 32)
go run ./examples/turnserver \
    -listen 0.0.0.0:3478 -relay-ip 203.0.113.10 -relay-ports 49152-49252 \
    -plans 'free=512KiB/4,paid=8MiB/32' -users '' -stats 60s
```

`-users ''` clears the default `admin=admin`; static users may also be kept
alongside the secret. The banner then says
`ephemeral credentials: enabled`.

The user ID part is any string without `:`. The plan rides in it by
convention as `name@plan`:

| User ID | Plan |
| --- | --- |
| `alice@free` | `free` |
| `alice@paid` | `paid` |
| `alice` | the default plan |
| `alice@gold` when no `gold` plan exists | the default plan |

### Issuing credentials with turncred

```sh
export PIPE_TURN_SECRET=...     # the same secret the relay has
go run ./examples/turncred -user alice@free -ttl 12h
```

```
username:   1788912235:alice@free
credential: bhtn4kqoJ+nL+/PChi6DjsY2nsE=
expires:    2026-09-09T00:03:55Z

PIPE_TURN_USERNAME="1788912235:alice@free" PIPE_TURN_PASSWORD="bhtn4kqoJ+nL+/PChi6DjsY2nsE="
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `-secret` | `PIPE_TURN_SECRET` | The shared secret. |
| `-user` | required | User ID, optionally `name@plan`. Must not contain `:`. |
| `-ttl` | `24h` | Validity period. |
| `-turn` | none | TURN URL to include in JSON output. |
| `-json` | off | Print `{"username","credential","expires_at","urls"}` for a client. |

The last line of the plain output is ready to paste into the environment that
`pipecat` and `turnclient` read.

### Issuing credentials from your own service

The same function the tool uses is exported from the examples' internal
package; copy it or call `turn.GenerateLongTermTURNRESTCredentials` from
`github.com/pion/turn/v5` directly, which is all it wraps:

```go
package main

import (
	"encoding/json"
	"net/http"
	"os"
	"time"

	"github.com/pion/turn/v5"
)

type relayCredentials struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username"`
	Credential string   `json:"credential"`
	ExpiresAt  string   `json:"expires_at"`
}

// handleRelayCredentials is called by a signed-in user. The service decides
// the plan from its own records and encodes it in the user ID.
func handleRelayCredentials(w http.ResponseWriter, r *http.Request) {
	account := accountFromSession(r) // your authentication
	if account == nil {
		http.Error(w, "sign in first", http.StatusUnauthorized)
		return
	}

	plan := "free"
	if account.Paid {
		plan = "paid"
	}
	userID := account.Name + "@" + plan // e.g. "alice@paid"; must not contain ':'

	const ttl = 12 * time.Hour
	username, password, err := turn.GenerateLongTermTURNRESTCredentials(
		os.Getenv("PIPE_TURN_SECRET"), userID, ttl)
	if err != nil {
		http.Error(w, "could not issue credentials", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(relayCredentials{
		URLs:       []string{"turn:relay.example.net:3478?transport=udp"},
		Username:   username,
		Credential: password,
		ExpiresAt:  time.Now().Add(ttl).UTC().Format(time.RFC3339),
	})
}
```

The client puts the result straight into `pipe.ICEServer`:

```go
servers := []pipe.ICEServer{{
	URLs:       creds.URLs,
	Username:   creds.Username,
	Credential: creds.Credential,
}}
```

and asks for a fresh set before `ExpiresAt`. Pipe copies `Config` in `New`,
so a new credential means a new `Endpoint`; keep the TTL long enough that an
endpoint's lifetime fits inside it, or rebuild endpoints on a schedule.

### Choosing the plan from a database instead

If you would rather not expose the tier in the user ID, keep IDs plain and
resolve plans on the server with `PlanFor`. This is a Go-level hook, so it
means embedding `turnx` in your own program rather than running the
`turnserver` binary:

```go
package main

import (
	"log/slog"
	"os"

	"ella.to/pipe/examples/internal/turnx"
)

func main() {
	db := openAccounts() // your storage

	srv, err := turnx.Start(turnx.Config{
		Listen:     "0.0.0.0:3478",
		RelayIP:    publicIP(),
		MinPort:    49152,
		MaxPort:    49252,
		AuthSecret: os.Getenv("PIPE_TURN_SECRET"),
		Plans: map[string]turnx.Plan{
			"free": {Rate: 512 << 10, MaxAllocations: 4},
			"paid": {Rate: 8 << 20, MaxAllocations: 32},
		},
		DefaultPlan: turnx.Plan{Rate: 512 << 10, MaxAllocations: 2},
		// userID is the part after the expiry in an ephemeral username, or
		// the static username. Return "" for the default plan.
		PlanFor: func(userID string) string {
			acct, ok := db.Lookup(userID)
			if !ok {
				return ""
			}
			if acct.Paid {
				return "paid"
			}
			return "free"
		},
		Logger: slog.Default(),
	})
	if err != nil {
		panic(err)
	}
	defer srv.Close()
	select {}
}
```

`PlanFor` is called on every allocation and quota check, so keep it cheap:
an in-memory map refreshed periodically, not a database round trip per
packet. It is never called per packet; the plan is fixed when the relay
socket is allocated.

`examples/internal/turnx` lives under `internal`, so a program outside this
module cannot import it directly. Copy the package into your own module; it
is a single file with no dependencies beyond `pion/turn` and
`golang.org/x/time/rate`.

## Quotas

`MaxAllocations` bounds how many relay sockets a user holds at once. The
count goes up when an allocation is created and down when it is deleted
(pion/turn's `EventHandler`), and the quota check runs before each new
allocation.

What a pipe connection consumes:

- One allocation per side that relays. A fully relayed connection between two
  users of your relay costs one allocation for each of them.
- A client with a TURN URL configured allocates a relay socket while
  gathering even if ICE ends up on a direct path, and that allocation is not
  necessarily released before the PeerConnection closes. Budget one
  allocation per connection per user of your relay, whether or not the relay
  ends up carrying the traffic, and confirm against `-stats` for your own
  clients.
- An ICE restart (recovery after connectivity loss) may allocate again
  briefly.

So `free=512KiB/4` lets a free user hold four concurrent connections that
gathered through your relay. A refused allocation appears in the
server log as `turn: allocation quota reached` and in the user's statistics
as `rejected`. The client sees ICE fail to gather a relay candidate; if a
direct path exists the connection still comes up, otherwise the dial times
out.

## Per-user statistics

With `-stats 60s` the server logs every minute:

```
turn: traffic stats="allocations=2 active=2 sent=323.9KiB/436pkt received=323.8KiB/433pkt dropped=129.2KiB/108pkt delayed=0pkt"
turn: user user=alice stats="plan=free allocations=1 active=1 sent=314.0KiB received=10.0KiB dropped=129.2KiB/108pkt rejected=0"
turn: user user=bob stats="plan=paid allocations=1 active=1 sent=10.0KiB received=313.8KiB dropped=0B/0pkt rejected=0"
```

Field meanings, from `turnx.UserStats`:

| Field | Meaning |
| --- | --- |
| `plan` | The plan the user resolved to (`default` for the default plan). |
| `allocations` | Relay sockets created for the user since startup. |
| `active` | Relay sockets open right now. |
| `sent` | Bytes forwarded from the user toward peers. |
| `received` | Bytes forwarded from peers toward the user. |
| `dropped` | Bytes and packets discarded because they exceeded the plan's budget. |
| `rejected` | Allocate requests refused by `MaxAllocations`. |

In a program embedding `turnx`, `srv.Stats()` returns the same numbers as a
`turnx.Stats` value with a `Users map[string]UserStats`, ready to export to
whatever metrics system you use. The example server has no metrics endpoint;
the log lines are the interface.

A user with steadily growing `dropped` is hitting the cap: either they need
a bigger plan or the plan is doing its job. A user with `rejected` growing
is opening more connections than the quota allows.

## Changing a user's plan

**Upgrade or downgrade.** Issue a new credential with the new suffix
(`alice@paid`) and have the client rebuild its endpoint with it. Nothing
happens on the relay: the plan is decided when a credential authenticates.

**Existing connections keep their plan.** The rate limiter is attached to
the relay socket when it is allocated, so a connection established as `free`
stays at 512 KiB/s until it ends. That is usually what you want; a downgrade
that throttled live connections would surprise users. If you need it to take
effect sooner, close and redial from the client.

**Allocation lifetime.** pion/turn allocations live 10 minutes by default
and clients refresh them. Refreshes are authenticated with the same
credential, so an expired credential stops working at the next refresh, and
an abandoned allocation disappears within its lifetime.

**Static users.** Change the `:plan` suffix in `-users` and restart. Only
new allocations see the change.

## Revocation

Ephemeral credentials cannot be revoked individually; they expire. So:

- **Use short TTLs.** Twelve to twenty-four hours for a personal project; an
  hour or less for anything with real users. The cost is that clients fetch
  a fresh credential from your service more often, which is one HTTP request.
- **Rotate the secret to revoke everything.** Restart the relay with a new
  `PIPE_TURN_SECRET`. Every outstanding credential fails at its next
  allocation or refresh. Clients that ask your service for a new credential
  recover on their own.
- **Cut a user off at the source.** Stop issuing credentials to a banned
  account; their last credential dies at its expiry.
- **Static users** are revoked by removing them from `-users` and
  restarting.

Never log credentials. The example server logs usernames (which for
ephemeral credentials contain the expiry and the user ID) and never the
password or the secret.

## Doing this with coturn

coturn accepts the same ephemeral credentials (`use-auth-secret` with
`static-auth-secret`), so `turncred` output and `IssueCredentials` work
unchanged against it. Its limits are global, not per plan:

| Option | Scope |
| --- | --- |
| `user-quota=N` | Concurrent allocations per username. Applies to every user. |
| `total-quota=N` | Concurrent allocations for the whole server. |
| `max-bps=N` | Bytes per second per allocation. Applies to every allocation. |
| `bps-capacity=N` | Bytes per second for the whole server. |

With `use-auth-secret`, check how your coturn version keys `user-quota`: if
it uses the full `expiry:userid` username, a user who fetches a new credential
gets a fresh quota, so the effective limit is looser than the number suggests.
The Go server keys quotas by the user ID after the colon.

To offer two tiers with coturn you have two options:

1. **Two instances.** Run one coturn on port 3478 with
   `max-bps=524288` and `user-quota=4`, another on port 3479 with
   `max-bps=8388608` and `user-quota=32`, each with its own secret. Your
   service hands paying users the second URL and secret-derived credential.
   Use disjoint `min-port`/`max-port` ranges.
2. **Use the Go server.** `examples/turnserver` was written because coturn
   cannot express per-user plans. It lacks coturn's TLS and IPv6 support,
   which is the tradeoff.

The Docker setup in [08-docker.md](08-docker.md) includes coturn behind a
profile with the configuration from [05-turn-relay.md](05-turn-relay.md).

## Abuse controls

A relay forwards UDP to any address a client asks for, which makes an open
relay a proxy into your network and a vector for reflection. Beyond
credentials and plans:

- **Deny relaying to private networks.** coturn's `denied-peer-ip` rules in
  the shipped configuration refuse RFC 1918 ranges, loopback, link-local, and
  IPv6 private ranges. The example Go server does not filter peer addresses;
  run it on a host with nothing else reachable from it, or add a
  `PermissionHandler` in pion/turn's `PacketConnConfig` when embedding
  `turnx`. See [06-security.md](06-security.md).
- **Bound the signaling server.** `examples/signaling -max-peers N` (the
  `sse.Config.MaxPeers` field) caps how many peers can register at once, and
  each peer needs a bearer token, so an attacker cannot register thousands
  of names to hunt for relay users. See
  [03-signaling-server.md](03-signaling-server.md).
- **Let listeners choose their callers.** `pipe.Config.AllowPeer` refuses an
  inbound offer before any PeerConnection or relay allocation is spent on it,
  with `RejectUnauthorized` reported to the dialer. With the authenticated
  peer IDs the HTTP signaler provides, it is a real access-control list.
- **Cap allocations.** A plan's `MaxAllocations` stops one credential from
  opening hundreds of relay sockets.
- **Keep TTLs short and rotate the secret** if a credential leaks.
- **Watch `rejected` and `dropped`.** Sudden growth for one user is the
  earliest signal of abuse or of a bug in a client.

## Summary

- Give every user a plan: a per-socket rate, a burst, and an allocation
  quota. `free=512KiB/4,paid=8MiB/32` is a complete two-tier relay.
- Prefer ephemeral credentials from a shared secret to static passwords; the
  plan travels in the user ID as `name@plan`, or comes from your own lookup
  through `PlanFor`.
- Measure with `turnclient -plans ... -user name@plan` and read the per-user
  `dropped` and `rejected` counters, remembering that an echo crosses the
  relay four times.
- coturn can enforce one global cap; true tiers need two instances or the Go
  server.
