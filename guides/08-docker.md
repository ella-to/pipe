# Running pipe's infrastructure with Docker

This guide is for someone who wants the signaling server and a TURN relay up
on one host with as little ceremony as possible, and who wants to understand
the two or three Docker-specific traps that make TURN fail silently. It is
built on the files in `examples/docker/`; read them alongside this text.

| File | Purpose |
| --- | --- |
| `examples/docker/Dockerfile` | Multi-stage build of every example command into one distroless image. |
| `examples/docker/docker-compose.yml` | `signaling` and `turn` services, a `turncred` tool profile, an optional `coturn` relay. |
| `examples/docker/.env.example` | The variables compose needs; copy to `.env` and edit. |
| `examples/docker/coturn/turnserver.conf` | A hardened coturn configuration that accepts the same ephemeral credentials. |
| `examples/docker/README.md` | The five-line quick start. |

## The image

The Dockerfile builds from the repository root (the examples import the module
they live in) in two stages:

1. `golang:1.27-alpine` downloads modules (cached separately from source
   changes) and builds `signaling`, `turnserver`, `turncred`, `pipecat`, and
   `turnclient` with `CGO_ENABLED=0 -trimpath -ldflags='-s -w'`.
2. `gcr.io/distroless/static-debian12:nonroot` receives the binaries in
   `/usr/local/bin/`. There is no shell and no package manager; the image
   contains the five binaries and CA certificates (needed by `turncred` and
   `pipecat` to talk HTTPS). It runs as a non-root user.

The image measured about 38 MB. Build it from the repository root:

```sh
docker build -t pipe-examples -f examples/docker/Dockerfile .
```

The default command is `signaling -listen :8080`. Any other binary is run by
naming it:

```sh
# Signaling with two peers' tokens.
docker run --rm -p 8080:8080 \
  -e PIPE_SIGNAL_TOKENS="alice=$(openssl rand -hex 24),bob=$(openssl rand -hex 24)" \
  pipe-examples signaling -listen :8080

# TURN with plans, ephemeral credentials, and a published relay range.
docker run --rm -p 3478:3478/udp -p 3478:3478/tcp -p 49152-49252:49152-49252/udp \
  -e PIPE_TURN_SECRET="$(openssl rand -hex 32)" \
  pipe-examples turnserver -listen 0.0.0.0:3478 -listen-tcp 0.0.0.0:3478 \
    -relay-ip 203.0.113.10 -relay-ports 49152-49252 \
    -plans 'free=512KiB/4,paid=8MiB/32' -stats 60s

# Mint credentials for a user (prints username, credential, expiry).
docker run --rm -e PIPE_TURN_SECRET=... pipe-examples turncred -user alice@free -ttl 24h

# Measure a relay from a container.
docker run --rm pipe-examples turnclient -embedded=false \
  -turn 'turn:203.0.113.10:3478?transport=udp' -user "$U" -pass "$P" -bytes 4MiB

# pipecat, connecting stdin/stdout of two containers on two hosts.
echo hello | docker run --rm -i pipe-examples pipecat \
  -signal https://signal.example.net/pipe -token "$ALICE_TOKEN" -id alice dial bob
```

Because the image has no shell, `docker run ... sh` does not work; use
`docker logs` and the `/healthz` endpoint instead.

## The compose stack

```sh
cp examples/docker/.env.example examples/docker/.env
$EDITOR examples/docker/.env
docker compose -f examples/docker/docker-compose.yml up --build -d
docker compose -f examples/docker/docker-compose.yml logs -f
```

Services:

| Service | Image | What it runs | Ports |
| --- | --- | --- | --- |
| `signaling` | built from the Dockerfile as `pipe-examples` | `signaling -listen=:8080 -path=/pipe -max-peers=${SIGNAL_MAX_PEERS}` | `${SIGNAL_PORT}:8080` |
| `turn` | `pipe-examples` | `turnserver` on UDP and TCP 3478, `-realm`, `-relay-ip`, `-relay-ports`, `-plans`, `-stats=60s` | `3478/udp`, `3478/tcp`, the relay range over UDP |
| `turncred` (profile `tools`) | `pipe-examples` | `turncred` with the secret from `.env`; one-shot | none |
| `coturn` (profile `coturn`) | `coturn/coturn:4.6` | coturn with `coturn/turnserver.conf` plus realm, external IP, secret, and port range from `.env` | same as `turn` |

Profiles keep the optional pieces from starting by default:

```sh
# Mint credentials with the same secret the relay uses.
docker compose -f examples/docker/docker-compose.yml run --rm turncred -user alice@free -ttl 24h

# Run coturn instead of the Go relay (stop `turn` first; they share ports).
docker compose -f examples/docker/docker-compose.yml stop turn
docker compose -f examples/docker/docker-compose.yml --profile coturn up -d coturn
```

Both relays accept credentials minted by `turncred`, because both implement
the TURN REST API scheme with the same secret and realm. The difference is
that the Go relay applies per-user plans while coturn's bandwidth limits are
per session; see [07-multi-user-relay.md](07-multi-user-relay.md).

### `.env`

| Variable | Used by | Meaning |
| --- | --- | --- |
| `PIPE_SIGNAL_TOKENS` | `signaling` | `peer=token,...`. One token per peer ID; at least 16 characters each. Required. |
| `SIGNAL_PORT` | `signaling` | Host port published for HTTP. Default 8080. |
| `SIGNAL_MAX_PEERS` | `signaling` | Bound on concurrently known peers. Default 1000. |
| `TURN_RELAY_IP` | `turn`, `coturn` | Public IP of the host. Required. |
| `TURN_REALM` | `turn`, `coturn` | TURN realm; clients must use the same. Default `pipe.example`. |
| `TURN_RELAY_PORTS` | `turn`, `coturn` | Relay range, `min-max`, also used for the port publish. Default `49152-49252`. |
| `TURN_RELAY_MIN`, `TURN_RELAY_MAX` | `coturn` | The same range as two numbers, because coturn takes them separately. |
| `PIPE_TURN_SECRET` | `turn`, `coturn`, `turncred` | Shared secret for ephemeral credentials. Required. |
| `TURN_PLANS` | `turn` | `name=rate[/maxallocations],...`. Default `free=512KiB/4,paid=8MiB/32`. |
| `PIPE_TURN_USERS` | `turn` | Optional static users, `user=password[:plan],...`. |

Generate the secrets:

```sh
openssl rand -hex 24   # one per token
openssl rand -hex 32   # PIPE_TURN_SECRET
```

Compose refuses to start with the `:?` variables unset, so a missing
`TURN_RELAY_IP` fails loudly instead of producing a relay nobody can reach.

## Why TURN under Docker needs `-relay-ip` and `-relay-ports`

A TURN allocation hands the client an address on the relay: "send to
`ip:port` and I will forward it to your peer". Two things go wrong in a
container if you do nothing:

**The address.** The relay learns its own address from the socket it bound,
which inside a container is something like `172.18.0.3`. A client on the
internet sends to that, and the packets go nowhere. `-relay-ip` (compose:
`TURN_RELAY_IP`) is the address advertised to clients and must be the public
IP that reaches the host. The example server also refuses to start on a
wildcard listen address without an explicit relay IP, for the same reason.

**The ports.** By default the kernel picks an ephemeral port for every relay
socket. Docker publishes only the ports you list at start time, so a relay
socket on port 53127 is unreachable unless you published 53127. The fix is to
confine relay sockets to a range (`-relay-ports 49152-49252`) and publish
exactly that range (`-p 49152-49252:49152-49252/udp`). The two must match.
The example server uses Pion's `RelayAddressGeneratorPortRange` for this;
coturn uses `--min-port` and `--max-port`.

Publishing a large UDP range is slow with the default userland proxy: Docker
starts one proxy process per port. A hundred ports is fine; ten thousand is
not. Keep the range proportional to the concurrent connections you expect
(one relay socket per side of each relayed connection), and prefer a modest
range with quotas over a huge one.

**The simpler alternative on Linux** is `network_mode: host` for the `turn`
service: the container shares the host's network stack, binds 3478 and its
relay sockets directly on the host, and needs no port publishing at all.
`-relay-ip` is still needed if the host is behind NAT (a cloud VM with a
private address and a public one mapped to it). This does not behave the same
on Docker Desktop for macOS or Windows, where "host" is the network of a
hidden Linux VM, not your machine; on those platforms the published-range
approach is the one that works, and even then only for clients that can reach
the VM's forwarded ports.

## TLS in front of signaling

The `signaling` service speaks plain HTTP on 8080. Bearer tokens travel in
headers, so anything beyond a single host needs TLS
([06-security.md](06-security.md)). The easiest way is a reverse proxy in the
same compose file. Two things matter for Server-Sent Events: the proxy must not
buffer responses, and it must not close idle upstream connections after a
short read timeout (the server sends a keepalive comment every 15 seconds by
default, so anything above a minute is comfortable).

Caddy, which obtains certificates itself:

```yaml
  caddy:
    image: caddy:2
    ports:
      - "80:80"
      - "443:443"
    volumes:
      - ./Caddyfile:/etc/caddy/Caddyfile:ro
      - caddy_data:/data
    depends_on:
      - signaling
```

```caddyfile
signal.example.net {
	reverse_proxy signaling:8080 {
		flush_interval -1
		transport http {
			read_timeout 0
		}
	}
}
```

nginx:

```nginx
server {
    listen 443 ssl;
    server_name signal.example.net;
    ssl_certificate     /etc/nginx/certs/fullchain.pem;
    ssl_certificate_key /etc/nginx/certs/privkey.pem;

    location /pipe {
        proxy_pass http://signaling:8080/pipe;
        proxy_http_version 1.1;
        proxy_set_header Connection "";
        proxy_buffering off;
        proxy_cache off;
        proxy_read_timeout 1h;
        proxy_send_timeout 1h;
    }
    location /healthz {
        proxy_pass http://signaling:8080/healthz;
    }
}
```

Alternatively give the signaling container a certificate and run it with
`-tls-cert` and `-tls-key`; then publish 443 and skip the proxy. Do not add a
`WriteTimeout` anywhere in the chain.

Clients then use `https://signal.example.net/pipe` as `sse.Client.URL`.

## Health, logs, statistics

- `GET /healthz` on the signaling service returns `ok peers=N`. Use it for
  the compose `healthcheck` or your load balancer. It is served by the example
  command, not by the `sse.Server` handler, so it stays unauthenticated.
- `docker compose logs -f signaling` shows peer registrations and
  forgettings at info level; add `-v` to the command for debug lines about
  rejected requests and stream attaches (never tokens or SDP).
- The relay logs every allocation with user, plan, relay address, and rate,
  and with `-stats=60s` prints a traffic line plus one line per user every
  minute, including drops caused by the plan's budget and allocations refused
  by its quota. [10-operations.md](10-operations.md) explains the fields.

A compose health check for signaling:

```yaml
    healthcheck:
      test: ["CMD", "/usr/local/bin/pipecat", "-h"]
      interval: 30s
```

Distroless has no `curl` or `wget`, so a health check that fetches `/healthz`
must come from outside the container or from a sidecar. The line above only
proves the binary runs; for a real probe, poll `/healthz` from your monitoring.

## Upgrading

```sh
git pull
docker compose -f examples/docker/docker-compose.yml build
docker compose -f examples/docker/docker-compose.yml up -d
```

The signaling server keeps all state in memory. A restart drops every peer's
stream and queue; clients reconnect with backoff (500 ms doubling to 10 s by
default) and resume. Signals in flight during the restart are lost, which for
a negotiation in progress means that dial times out and should be retried.

The relay likewise loses allocations on restart. Established pipe connections
through it lose connectivity, enter recovery, and either restart ICE through
the new relay instance within the `Reconnect` budget or close with
`ErrDisconnected`.

Nothing on disk needs migrating.

## Troubleshooting

| Symptom | Likely cause | Check |
| --- | --- | --- |
| `turnclient` connects with `-embedded` but not against the container | `-relay-ip` wrong or unset, or relay range not published | The relay log line `allocated a relay relay=IP:PORT`: is that IP public and that port inside the published range? |
| Allocation succeeds (log shows `allocated a relay`), then the dial times out | Relay sockets unreachable: range not published, firewall blocks it, or `TURN_RELAY_PORTS` differs between the command and the `ports:` mapping | `docker port <container>`; compare with `-relay-ports` |
| `401 Unauthorized` from signaling | Token missing, wrong, or shorter than 16 characters (the command refuses those at startup) | `docker compose logs signaling` at startup; the client's `sse: permanent failure` error |
| `403` from signaling | Token belongs to a different peer ID than `-id` | Match `PIPE_SIGNAL_TOKENS` entries to client `-id` values |
| Two containers on the same compose network connect `host/host` even with a TURN server configured | ICE prefers direct paths; they can reach each other on the bridge network | Expected. Force the relay with `-relay-only` to test it |
| `turn: rejected an allocation` in the relay log | Wrong realm, expired ephemeral credential, or a static user not in `PIPE_TURN_USERS` | Realm must match on both sides; mint a fresh credential |
| Event stream drops every minute or so behind a proxy | Proxy read timeout or buffering | `proxy_buffering off`, long `proxy_read_timeout`; Caddy `flush_interval -1` |
| Relay works from the LAN but not the internet | Host firewall or cloud security group does not open 3478 and the relay range for UDP | Open both; TCP 3478 too if you serve `transport=tcp` |
| coturn and `turn` both fail to start | Both publish 3478 and the same range | Run one at a time; the `coturn` profile exists so they do not start together |

For the wire-level view of what should be crossing each port, see
[11-protocol.md](11-protocol.md).
