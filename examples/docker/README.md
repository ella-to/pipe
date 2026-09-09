# Docker

Everything needed to run pipe's infrastructure on one host:

| File | Purpose |
| --- | --- |
| `Dockerfile` | Multi-stage build of every example command into one distroless image. |
| `docker-compose.yml` | `signaling` and `turn` services, a `turncred` tool, and an optional `coturn` relay. |
| `.env.example` | The variables compose needs; copy to `.env` and edit. |
| `coturn/turnserver.conf` | A hardened coturn configuration that accepts the same ephemeral credentials. |

Quick start, from the repository root:

```sh
cp examples/docker/.env.example examples/docker/.env
$EDITOR examples/docker/.env          # tokens, TURN_RELAY_IP, PIPE_TURN_SECRET
docker compose -f examples/docker/docker-compose.yml up --build -d
docker compose -f examples/docker/docker-compose.yml run --rm turncred -user alice@free -ttl 24h
```

The walkthrough, including TLS, firewall rules, and what to check when a
connection does not come up, is in [`guides/08-docker.md`](../../guides/08-docker.md).
