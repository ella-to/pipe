# Docker

Two ways: the ready-made stack in `examples/docker`, or your own programs from
the earlier guides in your own image.

## Option A: the ready-made stack

```sh
git clone https://github.com/ella-to/pipe && cd pipe
cp examples/docker/.env.example examples/docker/.env
$EDITOR examples/docker/.env     # tokens, TURN_RELAY_IP, PIPE_TURN_SECRET
docker compose -f examples/docker/docker-compose.yml up --build -d
```

| Service | Runs | Ports |
| --- | --- | --- |
| `signaling` | `signaling -listen=:8080` | `8080/tcp` |
| `turn` | `turnserver` with plans and `-auth-secret` | `3478/udp`, `3478/tcp`, relay range `/udp` |
| `turncred` (profile `tools`) | Mints credentials | none |
| `coturn` (profile `coturn`) | coturn with the same secret | same as `turn` |

```sh
# mint credentials
docker compose -f examples/docker/docker-compose.yml run --rm turncred -user alice@free -ttl 24h
# swap the Go relay for coturn
docker compose -f examples/docker/docker-compose.yml stop turn
docker compose -f examples/docker/docker-compose.yml --profile coturn up -d coturn
```

`.env`:

| Variable | Meaning |
| --- | --- |
| `PIPE_SIGNAL_TOKENS` | `peer=token,...`, at least 16 characters each |
| `SIGNAL_PORT`, `SIGNAL_MAX_PEERS` | Published port, peer limit |
| `TURN_RELAY_IP` | Public IP of the host |
| `TURN_REALM` | Realm |
| `TURN_RELAY_PORTS` | `min-max`, used for both the flag and the port publish |
| `TURN_RELAY_MIN`, `TURN_RELAY_MAX` | Same range for coturn |
| `PIPE_TURN_SECRET` | Shared secret for ephemeral credentials |
| `TURN_PLANS` | `free=512KiB/4,paid=8MiB/32` |
| `PIPE_TURN_USERS` | Optional static users |

## Option B: your own programs

Take `signal/main.go` from [02 step 3](02-quickstart.md#step-3-tokens) and
`relayd/main.go` from [05 step 8](05-turn-relay.md#step-8-everything-together).

```dockerfile
# Dockerfile
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/ ./signal ./relayd

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ /usr/local/bin/
```

```yaml
# compose.yml
services:
  signal:
    build: .
    image: hello-pipe
    command: ["signal"]
    environment:
      PIPE_SIGNAL_ADDR: ":8080"
      PIPE_SIGNAL_TOKENS: ${PIPE_SIGNAL_TOKENS:?}
    ports:
      - "8080:8080"
    restart: unless-stopped

  relayd:
    image: hello-pipe
    command: ["relayd"]
    environment:
      PIPE_TURN_SECRET: ${PIPE_TURN_SECRET:?}
      OPS_TURN_PW: ${OPS_TURN_PW:?}
      RELAY_PUBLIC_IP: ${RELAY_PUBLIC_IP:?}
    ports:
      - "3478:3478/udp"
      - "3478:3478/tcp"
      - "49152-49252:49152-49252/udp"
    restart: unless-stopped
```

```sh
docker compose up --build -d
docker compose logs -f relayd
```

## The two TURN traps

**Relay address.** Inside a container the relay sees `172.18.0.x`. Clients
must be told the host's public IP: `RelayIP` / `-relay-ip` /
`TURN_RELAY_IP`.

**Relay ports.** Docker publishes only listed ports. Pin relay sockets to a
range (`MinPort`/`MaxPort`, `-relay-ports`) and publish exactly that range.
Keep it to a few hundred ports; Docker's userland proxy starts one process per
port.

On Linux, `network_mode: host` avoids port publishing entirely:

```yaml
  relayd:
    image: hello-pipe
    command: ["relayd"]
    network_mode: host
    environment:
      PIPE_TURN_SECRET: ${PIPE_TURN_SECRET:?}
      OPS_TURN_PW: ${OPS_TURN_PW:?}
      RELAY_PUBLIC_IP: ${RELAY_PUBLIC_IP:?}
```

This does not work the same on Docker Desktop (macOS, Windows).

## TLS in front of signaling

```yaml
  caddy:
    image: caddy:2
    ports: ["80:80", "443:443"]
    volumes:
      - ./Caddyfile:/etc/caddy/Caddyfile:ro
      - caddy_data:/data

volumes:
  caddy_data:
```

```
# Caddyfile
signal.example.net {
	reverse_proxy signal:8080 {
		flush_interval -1
		transport http {
			read_timeout 0
		}
	}
}
```

Clients then use `https://signal.example.net/pipe`.

## Health and logs

- `GET /healthz` on the ready-made signaling server, or add one to yours as in
  [03 step 3](03-signaling-server.md#step-3-production-http-server).
  Distroless has no `curl`; probe from outside.
- The relay logs every allocation with user, plan, and relay address.
- Restarting either service drops in-memory state. Clients reconnect to
  signaling with backoff; relayed connections try an ICE restart, then close
  with `ErrDisconnected`.

## Troubleshooting

| Symptom | Check |
| --- | --- |
| Allocation logged, then the dial times out | `RelayIP` is public, and the published range equals `MinPort`-`MaxPort` (`docker port <container>`) |
| `turn: rejected an allocation` | Secret or realm differs, or the credential expired |
| `401` from signaling | Token missing or wrong |
| `403` from signaling | Token belongs to another peer ID |
| Containers on one host connect `host/host` | Expected: they share a bridge network. Force `ICETransportPolicyRelay` to test the relay |
| Streams drop every minute behind a proxy | Proxy buffering or read timeout |
| Works on LAN, not internet | Cloud firewall or security group must open UDP 3478, TCP 3478, and the relay range |
| `coturn` and `turn` both fail to start | Both bind 3478; run one |
