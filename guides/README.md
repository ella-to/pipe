# pipe guides

Everything needed to run pipe for a personal project or a small service: the
concepts, a quickstart, each server you might run, how to secure it, how to
give different users different relay budgets, and how to operate it.

| Guide | Read it when |
| --- | --- |
| [01 Concepts](01-concepts.md) | You want to know how two peers find each other, what signaling, STUN, and TURN each do, and what crosses the network. |
| [02 Quickstart](02-quickstart.md) | You want two machines talking in ten minutes, with the CLI and with Go. |
| [03 Signaling server](03-signaling-server.md) | You are running or embedding the HTTP signaling server, authenticating peers, or writing your own signaler. |
| [04 STUN](04-stun.md) | Your peers are behind home routers and you want direct connections. |
| [05 TURN relay](05-turn-relay.md) | Some peers cannot connect directly and you want your own relay, with the example server or coturn. |
| [06 Security](06-security.md) | You are exposing any of this to the internet. |
| [07 Multi-user relay](07-multi-user-relay.md) | You have free and paying users and want the relay to cap them differently, for example 512 KiB/s and 8 MiB/s. |
| [08 Docker](08-docker.md) | You want the signaling server and the relay as containers. |
| [09 Client API](09-client-api.md) | You are writing Go against `pipe.Endpoint`, `pipe.Conn`, and friends. |
| [10 Operations](10-operations.md) | You are running it: metrics, tuning, timeouts, troubleshooting. |
| [11 Protocol](11-protocol.md) | You are implementing a signaler, another client, or debugging on the wire. |

Runnable code that the guides refer to lives in [`../examples`](../examples):

| Command | Purpose |
| --- | --- |
| `examples/echo` | Two peers in one process; the whole API in one file. |
| `examples/signaling` | The HTTP signaling server. |
| `examples/pipecat` | netcat over pipe, for two machines. |
| `examples/turnserver` | STUN + TURN with per-user plans. |
| `examples/turnclient` | Proves and measures a connection through a relay. |
| `examples/turncred` | Mints ephemeral TURN credentials. |
| `examples/docker` | Dockerfile, compose stack, coturn configuration. |
