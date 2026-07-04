// A TURN server listening on UDP, TCP and TLS (TURNS) at the same time.
//
// UDP is the fastest path; TCP helps clients behind UDP-blocking firewalls;
// TLS (TURNS) additionally gets through TLS-inspecting middleboxes and is the
// recommended production transport.
//
//	go run ./turn/examples/02-tcp-tls \
//	    -public-ip 203.0.113.10 \
//	    -users alice=secret \
//	    -tls-cert /etc/turn/cert.pem -tls-key /etc/turn/key.pem
//
// Omit -tls-cert/-tls-key to run UDP+TCP only.
package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"ella.to/pipe/turn"
)

func main() {
	publicIP := flag.String("public-ip", "", "IP address that TURN clients use to reach this server (required)")
	udpListen := flag.String("udp", ":3478", "UDP listen address")
	tcpListen := flag.String("tcp", ":3478", "TCP listen address (UDP and TCP may share a port)")
	tlsListen := flag.String("tls", ":5349", "TLS (TURNS) listen address")
	tlsCert := flag.String("tls-cert", "", "TLS certificate file (PEM)")
	tlsKey := flag.String("tls-key", "", "TLS private key file (PEM)")
	users := flag.String("users", "", `static users as "user=pass,user=pass" (required)`)
	realm := flag.String("realm", "example.com", "authentication realm")
	flag.Parse()

	if *publicIP == "" || *users == "" {
		flag.Usage()
		os.Exit(1)
	}

	var staticUsers []turn.User
	for pair := range strings.SplitSeq(*users, ",") {
		username, password, ok := strings.Cut(pair, "=")
		if !ok {
			log.Fatalf("invalid user %q: expected user=pass", pair)
		}
		staticUsers = append(staticUsers, turn.User{Username: username, Password: password})
	}

	cfg := turn.Config{
		ListenAddr:    *udpListen,
		TCPListenAddr: *tcpListen,
		PublicIP:      *publicIP,
		Realm:         *realm,
		Users:         staticUsers,
	}

	// TLS is optional: only enabled when a certificate is provided.
	if *tlsCert != "" && *tlsKey != "" {
		cfg.TLSListenAddr = *tlsListen
		cfg.TLSCertFile = *tlsCert
		cfg.TLSKeyFile = *tlsKey
	}

	srv := &turn.Server{}
	if err := srv.Start(cfg); err != nil {
		log.Fatalf("start turn server: %v", err)
	}
	defer srv.Close()

	// URLs() reports every enabled transport:
	//   turn:203.0.113.10:3478?transport=udp
	//   turn:203.0.113.10:3478?transport=tcp
	//   turns:203.0.113.10:5349?transport=tcp
	log.Printf("TURN server running, URLs: %v", srv.URLs())

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	<-sigs
}
