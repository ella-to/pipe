// A minimal TURN server: static username/password auth over UDP.
//
//	go run ./turn/examples/01-simple -public-ip 127.0.0.1 -users alice=secret,bob=hunter2
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
	listen := flag.String("listen", ":3478", "UDP listen address")
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

	srv := &turn.Server{}
	err := srv.Start(turn.Config{
		ListenAddr: *listen,
		PublicIP:   *publicIP,
		Realm:      *realm,
		Users:      staticUsers,
	})
	if err != nil {
		log.Fatalf("start turn server: %v", err)
	}
	defer srv.Close()

	log.Printf("TURN server running, URLs: %v", srv.URLs())
	log.Printf("example ICE server for %q: %+v", staticUsers[0].Username, srv.ICEServerFor(staticUsers[0].Username))

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	<-sigs
}
