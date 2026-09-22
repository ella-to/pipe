# Quickstart

From one process, to two processes, to two machines. Every file is a complete
program inside the module from the [README](README.md):

```
hello-pipe/
  go.mod
  inprocess/main.go   step 1
  signal/main.go      steps 2 and 3
  peer/main.go        steps 2 to 5
```

## Step 1: two peers in one process

No server, no network. `memory.New()` is an in-process signaler.

```go
// inprocess/main.go
package main

import (
	"context"
	"fmt"
	"io"
	"log"

	"ella.to/pipe"
	"ella.to/pipe/signaling/memory"
)

func main() {
	ctx := context.Background()
	hub := memory.New()

	bob, err := pipe.New(ctx, pipe.Config{ID: "bob", Signaler: hub})
	if err != nil {
		log.Fatal(err)
	}
	defer bob.Close()

	alice, err := pipe.New(ctx, pipe.Config{ID: "alice", Signaler: hub})
	if err != nil {
		log.Fatal(err)
	}
	defer alice.Close()

	ln, err := bob.Listen()
	if err != nil {
		log.Fatal(err)
	}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.Copy(conn, conn) // echo
	}()

	conn, err := alice.Dial(ctx, "bob")
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte("hello")); err != nil {
		log.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(conn, buf); err != nil {
		log.Fatal(err)
	}
	st := conn.Stats()
	fmt.Printf("%s via %s/%s in %v\n", buf, st.LocalCandidate, st.RemoteCandidate, st.ConnectDuration)
}
```

```sh
go run ./inprocess
# hello via host/host in 7ms
```

## Step 2: two processes, one signaling server

The signaling server. `TrustPeerHeader` believes whatever ID a client claims,
so bind it to loopback only.

```go
// signal/main.go
package main

import (
	"log"
	"net/http"

	"ella.to/pipe/signaling/sse"
)

func main() {
	srv, err := sse.NewServer(sse.Config{Authenticator: sse.TrustPeerHeader()})
	if err != nil {
		log.Fatal(err)
	}
	defer srv.Close()

	http.Handle("/pipe", srv)
	log.Println("signaling on http://127.0.0.1:8080/pipe")
	log.Fatal(http.ListenAndServe("127.0.0.1:8080", nil))
}
```

A peer that either listens and prints what it receives, or dials and sends
stdin. Later steps only change `iceServers` and the environment.

```go
// peer/main.go
//
//	go run ./peer listen bob
//	echo hi | go run ./peer dial alice bob
package main

import (
	"context"
	"io"
	"log"
	"os"

	"ella.to/pipe"
	"ella.to/pipe/signaling/sse"
)

func main() {
	if len(os.Args) < 3 {
		log.Fatal("usage: peer listen <id> | peer dial <id> <peer>")
	}
	ctx := context.Background()
	mode, id := os.Args[1], pipe.PeerID(os.Args[2])

	ep, err := pipe.New(ctx, pipe.Config{
		ID: id,
		Signaler: &sse.Client{
			URL:   envOr("PIPE_SIGNAL_URL", "http://127.0.0.1:8080/pipe"),
			Token: os.Getenv("PIPE_SIGNAL_TOKEN"),
		},
		ICEServers: iceServers(),
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
		log.Printf("listening as %s", id)
		for {
			conn, err := ln.AcceptConn()
			if err != nil {
				log.Fatal(err)
			}
			go func() {
				defer conn.Close()
				st := conn.Stats()
				log.Printf("%s connected via %s/%s", conn.PeerID(), st.LocalCandidate, st.RemoteCandidate)
				_, _ = io.Copy(os.Stdout, conn)
			}()
		}

	case "dial":
		if len(os.Args) < 4 {
			log.Fatal("usage: peer dial <id> <peer>")
		}
		conn, err := ep.Dial(ctx, pipe.PeerID(os.Args[3]))
		if err != nil {
			log.Fatal(err)
		}
		defer conn.Close()
		st := conn.Stats()
		log.Printf("connected via %s/%s", st.LocalCandidate, st.RemoteCandidate)
		if _, err := io.Copy(conn, os.Stdin); err != nil {
			log.Fatal(err)
		}
	}
}

// iceServers is empty on one machine: host candidates are enough.
func iceServers() []pipe.ICEServer { return nil }

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
```

```sh
go run ./signal                         # terminal 1
go run ./peer listen bob                # terminal 2
echo hi | go run ./peer dial alice bob  # terminal 3
```

## Step 3: tokens

Replace `signal/main.go`. Each peer ID gets its own bearer token; the server
refuses a peer that claims another ID.

```go
// signal/main.go
package main

import (
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"ella.to/pipe"
	"ella.to/pipe/signaling/sse"
)

func main() {
	// PIPE_SIGNAL_TOKENS="alice=<token>,bob=<token>"
	tokens := map[string]pipe.PeerID{}
	for pair := range strings.SplitSeq(os.Getenv("PIPE_SIGNAL_TOKENS"), ",") {
		peer, token, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if ok && peer != "" && token != "" {
			tokens[token] = pipe.PeerID(peer)
		}
	}
	if len(tokens) == 0 {
		log.Fatal("set PIPE_SIGNAL_TOKENS=alice=<token>,bob=<token>")
	}

	srv, err := sse.NewServer(sse.Config{Authenticator: sse.StaticTokens(tokens)})
	if err != nil {
		log.Fatal(err)
	}
	defer srv.Close()

	mux := http.NewServeMux()
	mux.Handle("/pipe", srv)

	httpServer := &http.Server{
		Addr:              envOr("PIPE_SIGNAL_ADDR", "127.0.0.1:8080"),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: it would cut the event streams.
	}
	log.Printf("signaling on http://%s/pipe", httpServer.Addr)
	log.Fatal(httpServer.ListenAndServe())
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
```

```sh
ALICE=$(openssl rand -hex 24) BOB=$(openssl rand -hex 24)
PIPE_SIGNAL_TOKENS="alice=$ALICE,bob=$BOB" go run ./signal
PIPE_SIGNAL_TOKEN=$BOB go run ./peer listen bob
echo hi | PIPE_SIGNAL_TOKEN=$ALICE go run ./peer dial alice bob
```

The peer code does not change; it already sends `PIPE_SIGNAL_TOKEN`. A wrong
token fails in `pipe.New` with `sse: permanent failure: 401 Unauthorized`.

## Step 4: two machines

Run the signaling server on a host both peers can reach, behind TLS:

```sh
PIPE_SIGNAL_ADDR=:8080 PIPE_SIGNAL_TOKENS="alice=$ALICE,bob=$BOB" go run ./signal
# put Caddy or nginx in front for https; see 03-signaling-server.md
```

On a LAN that is all. Across the internet, peers need STUN to find their
public addresses. Replace `iceServers` in `peer/main.go`:

```go
func iceServers() []pipe.ICEServer {
	return []pipe.ICEServer{
		{URLs: []string{"stun:stun.l.google.com:19302"}},
	}
}
```

```sh
# machine A
PIPE_SIGNAL_URL=https://signal.example.net/pipe PIPE_SIGNAL_TOKEN=$BOB go run ./peer listen bob
# machine B
echo hi | PIPE_SIGNAL_URL=https://signal.example.net/pipe PIPE_SIGNAL_TOKEN=$ALICE go run ./peer dial alice bob
# connected via srflx/srflx
```

If the dial times out, one of the NATs does not allow a direct path. Add a
TURN relay:

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

Running that relay is [05-turn-relay.md](05-turn-relay.md).

## Step 5: accept only known peers

In `peer/main.go`, add `AllowPeer` to the `pipe.Config`:

```go
AllowPeer: func(peer pipe.PeerID) bool { return peer == "alice" },
```

Anyone else who dials gets `pipe.ErrPeerRejected` with code `unauthorized`,
before any WebRTC resources are spent.

## When it does not connect

| Symptom | Fix |
| --- | --- |
| `pipe.New`: `sse: permanent failure: 401` | Wrong token, or the token belongs to another peer ID |
| `Dial`: `peer unavailable` | The other peer is not connected to signaling |
| `Dial`: rejected, `not_listening` | The other peer has not called `Listen` |
| `Dial`: rejected, `unauthorized` | The other peer's `AllowPeer` refused you |
| `Dial` times out on the internet | Add STUN; if it still times out, add TURN |
| Works with STUN only on some networks | Symmetric NAT or blocked UDP: add TURN, and `?transport=tcp` |

Next: [03-signaling-server.md](03-signaling-server.md) for a production
signaling server, or [05-turn-relay.md](05-turn-relay.md) for the relay.
