# Running your own TURN relay

This guide is for anyone who wants pipe connections to work between any two
networks, including the ones where hole punching fails, and who would rather
run a small relay than depend on a commercial one. It covers the
`examples/turnserver` command flag by flag, a real deployment on a VPS with
firewall and systemd, TCP transport for UDP-hostile networks, coturn as the
production alternative, how the throughput budget works, how to test with
`examples/turnclient`, and what to check when it does not work. Per-user
plans and ephemeral credentials get their own guide,
[07-multi-user-relay.md](07-multi-user-relay.md).

## Why you need TURN

STUN ([04-stun.md](04-stun.md)) lets peers find each other's public
addresses, and for most home networks that is enough. It is not enough when
both peers sit behind symmetric NATs, when a firewall drops UDP to unknown
hosts, or when a network only allows outbound TCP. Those cases are common
enough (corporate networks, mobile carriers, hotels, some countries) that a
service which must always connect needs a fallback.

TURN (Traversal Using Relays around NAT, RFC 8656) is that fallback. A peer
opens a connection to the TURN server and asks for an **allocation**: a
public `ip:port` on the server. Packets sent to that address are forwarded to
the peer over the connection it opened, which works from behind any NAT
because the peer initiated it. ICE advertises the allocation as a `relay`
candidate, tries it last, and uses it only when nothing better works.

Everything that crosses the relay is DTLS ciphertext. The relay operator sees
addresses and byte counts, not content. What the operator pays for is
bandwidth, which is why every TURN server requires credentials and why a
budget per user matters.

## The example server

`examples/turnserver` is a complete STUN and TURN server built on `pion/turn`.
One UDP listener answers both protocols. It authenticates users from a static
table or from a shared secret, applies a throughput budget per relay socket,
enforces per-user allocation quotas, and reports traffic counters. It serves
IPv4 over UDP, with an optional TCP listener, and does not terminate TLS.

### Local development

```sh
go run ./examples/turnserver
```

```
stun:  stun:127.0.0.1:3478
turn:  turn:127.0.0.1:3478?transport=udp
realm: pipe.example
users: admin
try:  go run ./examples/turnclient -embedded=false -turn turn:127.0.0.1:3478?transport=udp -user admin -pass '<password>'
```

The defaults are `admin`/`admin` on loopback, unlimited throughput, and
ephemeral relay ports chosen by the kernel. Log lines go to stderr; the URLs
above go to stdout so a script can capture them.

### Flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `-listen` | `127.0.0.1:3478` | UDP address for STUN and TURN. Use `0.0.0.0:3478` to serve every interface. |
| `-listen-tcp` | off | TCP address on which TURN is also served, for clients that cannot use UDP. |
| `-realm` | `pipe.example` | TURN realm. It is part of the credential digest, so clients using long-term credentials must be given the same realm. |
| `-users` | `admin=admin` | Comma-separated `user=password[:plan]` list. `PIPE_TURN_USERS` overrides the flag. |
| `-auth-secret` | off | Shared secret for ephemeral credentials. `PIPE_TURN_SECRET` overrides the flag. Static users keep working alongside. |
| `-plans` | none | Comma-separated `name=rate[/maxallocations]` list, for example `free=512KiB/4,paid=8MiB/32`. |
| `-relay-ip` | the listen address | Address advertised to clients as their relay address. Required when listening on `0.0.0.0` and whenever the server is behind NAT. |
| `-relay-ports` | kernel-chosen | Inclusive UDP port range for relay sockets, for example `49152-49252`. Needed behind a firewall or a Docker port mapping. |
| `-rate` | `0` (unlimited) | Default plan: bytes per second per relay socket per direction. Accepts `512KiB`, `4MiB`, `1MB`, plain numbers. |
| `-burst` | derived | Default plan: token bucket depth in bytes. Zero derives a tenth of a second of `-rate`, never below 64 KiB. |
| `-max-delay` | `20ms` | How long a relayed packet may be held waiting for budget before it is dropped. |
| `-max-allocations` | `0` (unlimited) | Default plan: concurrent relay sockets per user. |
| `-stats` | off | Interval for logging traffic counters, overall and per user. |
| `-v` | off | Debug logging. |

The command refuses to start with an empty user list and no secret, because
such a relay would refuse everyone. Passwords are converted to the RFC 5389
key digest at startup and never logged; a rejected allocation is logged with
the username, realm, and source address only.

### Size and rate syntax

Sizes accept binary suffixes (`KiB`, `MiB`, `GiB`, and the short forms `K`,
`M`, `G`, which are also binary), decimal suffixes (`KB`, `MB`, `GB`), a bare
`B`, or a bare number of bytes. `512KiB` is 524288 bytes per second; `1MB`
is 1000000. A rate of `0` means unlimited.

## A real deployment

### What the server needs

- A public IPv4 address, or a NAT with the ports below forwarded and the
  public address known to you.
- UDP 3478 inbound for STUN and TURN, TCP 3478 if you enable `-listen-tcp`.
- A UDP port range inbound for relay sockets. Peers send their traffic to
  those ports.
- Bandwidth. Every relayed byte enters the server once and leaves it once.

### Build and install

```sh
CGO_ENABLED=0 go build -o /usr/local/bin/pipe-turnserver ./examples/turnserver
```

The binary is static and has no runtime dependencies.

### Run it

```sh
export PIPE_TURN_USERS="alice=$(openssl rand -hex 16),bob=$(openssl rand -hex 16)"
pipe-turnserver \
    -listen 0.0.0.0:3478 \
    -listen-tcp 0.0.0.0:3478 \
    -relay-ip 203.0.113.10 \
    -relay-ports 49152-49252 \
    -realm relay.example.net \
    -stats 60s
```

Points that matter here:

- `-relay-ip` is the address clients will send relayed packets to. It must be
  the server's **public** address. When the server listens on `0.0.0.0` the
  flag is mandatory; when the server is behind a NAT it must be the NAT's
  external address, not the interface address the server itself sees.
- `-relay-ports` confines relay sockets to a range you can open on the
  firewall. Each active allocation uses one port. A hundred ports serve fifty
  concurrent pipe connections (one allocation per side when both sides relay
  through you).
- `-realm` is any string, conventionally your domain. Clients that use
  long-term credentials do not configure the realm explicitly; the server
  announces it in the 401 challenge and the client derives the key from
  `username:realm:password`. Changing the realm invalidates nothing for
  clients, but the digest table on the server is recomputed at startup from
  the passwords, so it is simply a restart.
- Write the credentials to the environment, not to the command line, so that
  they do not appear in `ps` output.

### Firewall

With `nftables` or `iptables`, allow:

| Direction | Protocol | Port(s) | Purpose |
| --- | --- | --- | --- |
| Inbound | UDP | 3478 | STUN and TURN control and data |
| Inbound | TCP | 3478 | TURN over TCP (`-listen-tcp`) |
| Inbound | UDP | 49152-49252 | Relay sockets (`-relay-ports`) |
| Outbound | UDP | any | Relayed traffic toward peers |

For `ufw`:

```sh
ufw allow 3478/udp
ufw allow 3478/tcp
ufw allow 49152:49252/udp
```

Relayed packets leave from the relay ports toward whatever address the peer
is at, so outbound UDP must be unrestricted.

### systemd unit

```ini
# /etc/systemd/system/pipe-turnserver.service
[Unit]
Description=pipe STUN/TURN relay
After=network-online.target
Wants=network-online.target

[Service]
User=turn
Group=turn
EnvironmentFile=/etc/pipe-turnserver.env
ExecStart=/usr/local/bin/pipe-turnserver \
    -listen 0.0.0.0:3478 \
    -listen-tcp 0.0.0.0:3478 \
    -relay-ip 203.0.113.10 \
    -relay-ports 49152-49252 \
    -realm relay.example.net \
    -plans free=512KiB/4,paid=8MiB/32 \
    -stats 300s
Restart=always
RestartSec=2
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
AmbientCapabilities=

[Install]
WantedBy=multi-user.target
```

```sh
# /etc/pipe-turnserver.env  (mode 0600, owned by root)
PIPE_TURN_USERS=alice=...:free,bob=...:paid
PIPE_TURN_SECRET=...
```

Port 3478 is above 1024, so the service needs no capabilities. Enable with
`systemctl enable --now pipe-turnserver` and watch
`journalctl -u pipe-turnserver -f` for the periodic `turn: traffic` lines.

### Behind NAT

A relay behind a NAT works if the NAT forwards UDP 3478, TCP 3478, and the
relay port range to it and `-relay-ip` is set to the NAT's public address.
The server binds its relay sockets to its own interface address and tells
clients the public one; the NAT translates in between. Nothing else is
needed, but every one of those ports must be forwarded or allocations will
succeed and carry no traffic.

## TCP transport

Some networks block UDP entirely. Peers on such networks can still reach a
TURN server over TCP, and the server relays to the other peer over UDP as
usual. Enable the listener:

```sh
pipe-turnserver -listen 0.0.0.0:3478 -listen-tcp 0.0.0.0:3478 -relay-ip 203.0.113.10 ...
```

and give clients a second URL:

```go
{
	URLs: []string{
		"turn:relay.example.net:3478?transport=udp",
		"turn:relay.example.net:3478?transport=tcp",
	},
	Username:   user,
	Credential: pass,
}
```

ICE gathers a relay candidate through each and prefers UDP when it works.
Only the leg between the peer and the relay is TCP; SCTP's own congestion
control still runs end to end, so throughput on the TCP leg is somewhat lower
than on UDP, and latency spikes under loss are larger. It is still far better
than not connecting.

## TLS (`turns:`)

`turns:` wraps TURN in TLS on TCP, usually on port 5349. It exists for
networks that only let HTTPS-looking traffic out. The payload is already
DTLS-encrypted, so `turns:` adds no confidentiality for your data; it adds
reachability and hides the TURN protocol from middleboxes.

The example server does **not** terminate TLS. Two ways to get `turns:`:

1. Run coturn (below) with `tls-listening-port=5349`, `cert=`, and `pkey=`.
   coturn accepts the same credentials and the same relay configuration.
2. Put a TCP-level TLS terminator in front of the example server's TCP
   listener, for example HAProxy in `mode tcp` or `stunnel`, decrypting on
   5349 and forwarding to `127.0.0.1:3478`. The relay then sees the
   terminator's address as the client's source address, which is fine: TURN
   authenticates by credential, not by address.

Pipe accepts `turns:` URLs and passes them to Pion, which handles the TLS
handshake. The certificate must be valid for the host name in the URL.

## coturn as the production alternative

coturn is the reference production TURN server. Use it when you need TLS,
IPv6, a database of users, or years of operational track record. The
configuration below is the one shipped in
`examples/docker/coturn/turnserver.conf`; the values that depend on your
deployment are passed on the command line in the Docker setup so that they
can live in `.env`, but you can also write them into the file.

```
# coturn configuration for pipe.

# STUN and TURN on the standard port, UDP and TCP.
listening-port=3478

# Ephemeral credentials in the TURN REST API format: username "expiry:user",
# password HMAC-SHA1(secret, username), which is what turncred and
# turnx.IssueCredentials produce. The secret comes from --static-auth-secret.
use-auth-secret

# Each relay allocation and each peer permission expire on their own; nothing
# needs a database.
no-cli
no-tlsv1
no-tlsv1_1
fingerprint

# Refuse to relay to private ranges and to the server itself, so that an
# authenticated client cannot use the relay to reach the internal network.
no-multicast-peers
denied-peer-ip=10.0.0.0-10.255.255.255
denied-peer-ip=172.16.0.0-172.31.255.255
denied-peer-ip=192.168.0.0-192.168.255.255
denied-peer-ip=127.0.0.0-127.255.255.255
denied-peer-ip=169.254.0.0-169.254.255.255
denied-peer-ip=::1
denied-peer-ip=fc00::-fdff:ffff:ffff:ffff:ffff:ffff:ffff:ffff
denied-peer-ip=fe80::-febf:ffff:ffff:ffff:ffff:ffff:ffff:ffff

# Quotas. user-quota is concurrent allocations per username, total-quota is
# the server-wide bound. max-bps is bytes per second per allocation and applies
# to everyone; coturn has no per-user tiers, which is why the Go turnserver
# exists. bps-capacity caps the whole server.
user-quota=8
total-quota=1000
max-bps=1048576
bps-capacity=0

# Log to stdout for `docker compose logs`.
log-file=stdout
simple-log
```

Add for a bare-metal deployment:

```
realm=relay.example.net
external-ip=203.0.113.10
static-auth-secret=<the same secret you give turncred>
min-port=49152
max-port=49252
# TLS
tls-listening-port=5349
cert=/etc/letsencrypt/live/relay.example.net/fullchain.pem
pkey=/etc/letsencrypt/live/relay.example.net/privkey.pem
```

How the options map to the example server:

| coturn | Example server | Notes |
| --- | --- | --- |
| `listening-port` | `-listen` / `-listen-tcp` | coturn listens on UDP and TCP with one option. |
| `external-ip` | `-relay-ip` | The public relay address. |
| `min-port` / `max-port` | `-relay-ports` | Relay socket range. |
| `realm` | `-realm` | Must match what clients with static passwords were given. |
| `user=name:password` (with `lt-cred-mech`) | `-users` | Static long-term credentials. |
| `use-auth-secret` + `static-auth-secret` | `-auth-secret` | Same HMAC-SHA1 scheme; `turncred` output works with both. |
| `user-quota` | `-max-allocations` or a plan's `/N` | coturn's is per username, global. |
| `max-bps` | `-rate` or a plan's rate | coturn's is per allocation for every user; no plans. |
| `bps-capacity` | none | Server-wide cap; the example server has no global cap. |
| `denied-peer-ip` | none | The example server relays to any address; see [06-security.md](06-security.md). |
| `tls-listening-port`, `cert`, `pkey` | none | The example server does not terminate TLS. |

If you need per-user tiers with coturn, see the coturn section of
[07-multi-user-relay.md](07-multi-user-relay.md).

## How the throughput budget works

When a plan has a rate, every relay socket the server allocates for that
plan's users gets two token buckets, one per direction, each filling at the
plan's rate with a depth of the plan's burst (a tenth of a second of rate by
default, never below 64 KiB so that a single datagram can always pass).

The two directions are treated differently on purpose:

- **Client to peer** (packets the client sends through its allocation) is
  **policed**. This path runs on the goroutine that serves every client of
  the listener, so it must never block. A datagram over budget is dropped and
  the client is told it was sent, which is exactly what a congested link
  does.
- **Peer to client** (packets arriving at the relay address) is **shaped**.
  This path has its own goroutine per allocation, so a datagram may be held
  up to `-max-delay` (20 ms by default) to stay inside the budget, and is
  dropped only if it would need longer.

The point is that the congestion is real. SCTP inside the pipe connection
sees delay and loss and reduces its sending rate the way it would on a
genuinely slow link. A knob that only reported a number would tell you
nothing about how your application behaves when a free user hits the cap.

### Why measured throughput is lower than the rate

The rate is per relay socket per direction. A pipe connection where both
sides relay through the same server uses two allocations, and a byte from
alice to bob crosses the server twice: in through alice's allocation and out
through bob's. If bob echoes it back, it crosses twice more. `turnclient`
measures an echo, so every byte is metered four times, each crossing has its
own bucket, and the drops make SCTP back off between crossings. With a
512 KiB/s plan an echoed 1 MiB transfer measures roughly 80 KiB/s each way on
loopback; a one-way transfer between two peers where only one side relays
sees something much closer to the cap. Read the relay counters in the same
output: they show what was actually metered and what was dropped, and the
dropped bytes explain the gap.

## Testing with turnclient

`examples/turnclient` moves data through a pipe connection that is forced
onto a relay, and reports what it measured.

### Self-contained

```sh
go run ./examples/turnclient
```

It starts a relay in-process, connects two endpoints through it with
`ICETransportPolicyRelay`, transfers 4 MiB as an echo, and prints a report.
Output from a `-bytes 512KiB` run on one machine:

```
turn:        turn:127.0.0.1:55711?transport=udp
stun:        stun:127.0.0.1:55711
user:        admin (realm pipe.example)
policy:      relay
relay rate:  unlimited
transfer:    512.0KiB (echoed, so every byte crosses the relay four times)

connected in 3ms
state        connected, candidates relay/relay, read 0B, written 0B

transferred  512.0KiB round trip in 17ms
throughput   28.6MiB/s each way
state        connected, candidates relay/relay, read 512.0KiB, written 512.0KiB

relay        allocations=2 active=2 sent=1.1MiB/1294pkt received=1.1MiB/1294pkt dropped=0B/0pkt delayed=0pkt
user admin    plan=default allocations=2 active=2 sent=1.1MiB received=1.1MiB dropped=0B/0pkt rejected=0
```

`candidates relay/relay` is the line that matters. With `-relay-only` (the
default) ICE gathers relay candidates only, so it cannot quietly pick the
direct host path and report a success that never touched the relay. Numbers
from a loopback run describe the mechanism, not capacity.

Squeeze the relay:

```sh
go run ./examples/turnclient -rate 512KiB -bytes 1MiB
```

### Against a server you run

```sh
go run ./examples/turnclient -embedded=false \
    -turn 'turn:relay.example.net:3478?transport=udp' \
    -stun 'stun:relay.example.net:3478' \
    -user alice -pass "$ALICE_PASSWORD" -bytes 1MiB
```

`PIPE_TURN_USERNAME` and `PIPE_TURN_PASSWORD` are read when the flags are
empty. The `-realm` flag only affects the embedded server; against an
external server the realm comes from the server's challenge.

Flags:

| Flag | Default | Meaning |
| --- | --- | --- |
| `-embedded` | `true` | Run a relay in-process. |
| `-turn` | none | TURN URL; required with `-embedded=false`. |
| `-stun` | none | Optional STUN URL. |
| `-user` | `admin` | TURN username, or user ID when `-auth-secret` is set. |
| `-pass` | `admin` | TURN password. |
| `-auth-secret` | none | Mint ephemeral credentials for `-user` instead of using `-pass`. |
| `-realm` | `pipe.example` | Realm of the embedded server. |
| `-relay-only` | `true` | Gather relay candidates only. |
| `-bytes` | `4MiB` | Transfer size. |
| `-rate`, `-burst`, `-max-delay` | unlimited | Default plan of the embedded server. |
| `-plans` | none | Named plans for the embedded server; select with `-user name@plan`. |
| `-v` | off | Debug logging from pipe and the relay. |

### Reading the numbers honestly

`connected in` is the time from `Dial` to an open DataChannel, including the
TURN allocation and ICE checks. `throughput` is one direction of the echo:
transfer size divided by round-trip time. Compare it with the relay
counters, not with the plan rate, and remember that the transfer crosses the
relay four times.

## Client configuration in Go

```go
package main

import (
	"context"
	"log"
	"os"

	"ella.to/pipe"
	"ella.to/pipe/signaling/sse"
)

func newEndpoint(ctx context.Context, id pipe.PeerID) (*pipe.Endpoint, error) {
	return pipe.New(ctx, pipe.Config{
		ID:       id,
		Signaler: &sse.Client{URL: os.Getenv("PIPE_SIGNAL_URL"), Token: os.Getenv("PIPE_SIGNAL_TOKEN")},
		ICEServers: []pipe.ICEServer{
			// STUN first: cheap, and it lets ICE find a direct path when one exists.
			{URLs: []string{"stun:relay.example.net:3478"}},
			// TURN as the fallback. UDP and TCP transports of the same server.
			{
				URLs: []string{
					"turn:relay.example.net:3478?transport=udp",
					"turn:relay.example.net:3478?transport=tcp",
				},
				Username:       os.Getenv("PIPE_TURN_USERNAME"),
				Credential:     os.Getenv("PIPE_TURN_PASSWORD"),
				CredentialType: pipe.ICECredentialPassword, // the default; shown for clarity
			},
		},
		// Leave this at the default (all) in production so that direct paths
		// are used when possible. Set relay to force the relay, which is how
		// you test it and how you keep peers from learning each other's
		// addresses.
		ICETransportPolicy: pipe.ICETransportPolicyAll,
	})
}

func main() {
	ep, err := newEndpoint(context.Background(), "alice")
	if err != nil {
		log.Fatal(err)
	}
	defer ep.Close()
}
```

Rules enforced by `pipe.New`: a TURN URL requires both `Username` and
`Credential`; `ICETransportPolicyRelay` requires at least one TURN URL;
`ICECredentialOAuth` is not supported. Credentials are never logged, never
put in metrics labels, and never included in signaling messages.

When only one peer needs the relay (the other has a public address or a
friendly NAT), ICE will pair the relay candidate on one side with a direct
candidate on the other, and only one allocation is used. `Conn.Stats()`
reports the pair, for example `relay/srflx`.

## Capacity planning

Rules of thumb for the example server or coturn:

- **Bandwidth is metered twice.** Every relayed byte enters and leaves the
  server. A connection pushing 1 MiB/s through the relay costs 2 MiB/s of
  server bandwidth, or 4 MiB/s when both sides relay through you.
- **One allocation per relaying side.** A pipe connection uses one relay
  socket per side that relays. Size `-relay-ports` accordingly: 200 ports
  serve 100 fully relayed connections.
- **Each allocation has its own budget.** With `-rate 512KiB`, ten users
  each get 512 KiB/s per direction per allocation; the server has no global
  cap. Use the plan's `/maxallocations` to bound how many sockets one user
  can hold. See [07-multi-user-relay.md](07-multi-user-relay.md).
- **Allocations expire.** Pion clients refresh them; an abandoned allocation
  disappears after its lifetime (10 minutes by default in pion/turn).
- **CPU is rarely the limit.** Relaying is a copy per packet. A small VM
  saturates its network link long before its CPU.
- **Watch the counters.** `-stats 300s` logs overall and per-user traffic,
  drops, and rejected allocations. Drops on a plan mean users are hitting
  their cap; rejected allocations mean they hit the quota.

## Troubleshooting

**The server logs `turn: rejected an allocation` and the client never connects.**
The username is unknown, the password is wrong, or with ephemeral credentials
the credential has expired or was minted with a different secret. If you use
`turnclient -embedded=false`, note that it authenticates against the realm the
server announces; a `-realm` mismatch only matters for the embedded server.
With static credentials the realm is part of the key, so a server restarted
with a new `-realm` still accepts the same passwords (it recomputes the
digests), but a client that cached an old challenge must retry.

**Allocations succeed, `connected in` is fine, but no bytes flow or the transfer stalls.**
Peers cannot reach the relay address. Either `-relay-ip` is wrong (an
internal address, or `0.0.0.0` was refused at startup), or the relay port
range is not open on the firewall or not forwarded through the NAT. Check the
`turn: allocated a relay` log line: the `relay=` address is what peers are
told.

**`candidates host/host` although you expected the relay.**
The client is not using `ICETransportPolicyRelay`, so ICE preferred the
direct path. That is correct behavior in production. For a relay test, force
the policy (`turnclient -relay-only`, `pipecat -relay-only`).

**`pipe.New` fails with "ICETransportPolicy relay requires a TURN server".**
You set the relay policy without a `turn:` URL, or the TURN entry was
rejected for missing credentials.

**Startup fails with "listening on a wildcard address requires an explicit RelayIP".**
Add `-relay-ip` with the public address.

**Startup fails with "MinPort and MaxPort must both be set".**
`-relay-ports` needs the form `min-max` with both ends.

**Connections work over UDP but a particular user never connects.**
Their network blocks UDP. Enable `-listen-tcp` and give clients the
`?transport=tcp` URL too, or run coturn for `turns:`.

**Throughput through a capped plan is far below the cap.**
Expected; see the budget section above. Check the relay's `dropped` counter
for that user and remember the echo test crosses the relay four times.

**Everything works on one machine and fails across the internet.**
The one-machine test never leaves loopback, so `-relay-ip`, port forwarding,
and firewall rules are untested. Run `turnclient -embedded=false` from a
different network against the deployed server; if allocations succeed but
data does not flow, it is the relay address or port range.

## Where to go next

- Plans, quotas, and ephemeral credentials: [07-multi-user-relay.md](07-multi-user-relay.md)
- Threat model and hardening: [06-security.md](06-security.md)
- Docker Compose for signaling plus TURN: [08-docker.md](08-docker.md)
- Metrics, logging, and tuning: [10-operations.md](10-operations.md)
