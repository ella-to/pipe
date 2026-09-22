# STUN

STUN tells a peer its public `ip:port`, so ICE can try a direct path across
NATs (`srflx` candidates). The STUN server is not in the data path.

| Situation | STUN alone |
| --- | --- |
| Same LAN | Not needed |
| Home routers, most mobile hotspots | Usually works |
| One side behind symmetric NAT | Often works |
| Both sides behind symmetric NAT, strict corporate NAT, UDP blocked | Fails: add TURN ([05](05-turn-relay.md)) |

## Step 1: use a public STUN server

In the `peer/main.go` from [02-quickstart.md](02-quickstart.md):

```go
func iceServers() []pipe.ICEServer {
	return []pipe.ICEServer{
		{URLs: []string{"stun:stun.l.google.com:19302"}},
	}
}
```

A STUN entry has no `Username` or `Credential`; setting one fails `pipe.New`
with `ErrConfig`.

## Step 2: run your own

`relay.Start` answers STUN Binding requests on its UDP port. Without users or a
secret it refuses every TURN allocation, which makes it a STUN-only server.

```go
// stund/main.go
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
	srv, err := relay.Start(relay.Config{
		Listen:  "0.0.0.0:3478",
		RelayIP: net.ParseIP("203.0.113.10"), // this host's public address
	})
	if err != nil {
		log.Fatal(err)
	}
	defer srv.Close()
	log.Println("STUN on udp :3478")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	<-ctx.Done()
}
```

```sh
go run ./stund
ufw allow 3478/udp
```

```go
func iceServers() []pipe.ICEServer {
	return []pipe.ICEServer{
		{URLs: []string{"stun:stun.example.net:3478"}},
		{URLs: []string{"stun:stun.l.google.com:19302"}}, // fallback if yours is down
	}
}
```

Or without writing code: `go run ella.to/pipe/examples/turnserver@latest`.
coturn with `stun-only` works too.

## Step 3: verify

```go
st := conn.Stats()
log.Printf("via %s/%s", st.LocalCandidate, st.RemoteCandidate)
```

| Pair | Meaning |
| --- | --- |
| `srflx/srflx`, `srflx/host`, `prflx/...` | STUN worked, direct path |
| `host/host` | Same LAN, or one side has a public address |
| `relay/...` | Went through TURN |
| Dial timeout | NATs did not cooperate: add TURN |

On one machine you will always see `host/host`; test from two networks.

## Step 4: STUN and TURN from the same host

A TURN server is also a STUN server on the same port. Use two entries: STUN
without credentials, TURN with them.

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

See [05-turn-relay.md](05-turn-relay.md).

## Troubleshooting

| Symptom | Cause |
| --- | --- |
| Only `host` candidates, works on LAN only | STUN unreachable: UDP 3478 closed, or a typo in the URL. `pipe.New` does not fail for this |
| `STUN URLs must not carry credentials` | Credentials on a `stun:` entry. Split into two entries |
| `srflx` gathered but the dial still times out | NAT filtering; add TURN |
| Reflexive address is private | STUN server is on the same private network, or a VPN intercepts |
| Direct connections take 500 ms longer | Pion waits before nominating a reflexive pair. Lower it with `se.SetSrflxAcceptanceMinWait` in `Config.Pion` ([09](09-client-api.md#the-pion-escape-hatch)) |

`stuns:` (STUN over TLS) is accepted but rarely useful: it reports a TCP
mapping, not the UDP one the data will use.
