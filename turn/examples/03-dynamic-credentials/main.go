// A TURN server with dynamic time-limited credentials (TURN REST API,
// draft-uberti-behave-turn-rest-00) and an HTTP endpoint that issues them.
//
// No passwords are stored or distributed: the TURN server and the credential
// issuer share a secret, and credentials expire automatically. This is how
// you hand TURN access to web/mobile clients after they authenticate with
// your application server.
//
//	go run ./turn/examples/03-dynamic-credentials -public-ip 127.0.0.1 -secret my-shared-secret
//
// Fetch credentials for a user:
//
//	curl 'http://localhost:8080/credentials?user=alice'
//	{
//	  "urls": ["turn:127.0.0.1:3478?transport=udp"],
//	  "username": "1751659200:alice",
//	  "credential": "Fs3...="
//	}
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"flag"

	"ella.to/pipe/turn"
)

func main() {
	publicIP := flag.String("public-ip", "", "IP address that TURN clients use to reach this server (required)")
	listen := flag.String("listen", ":3478", "UDP listen address")
	httpListen := flag.String("http", ":8080", "HTTP listen address for the credential endpoint")
	secret := flag.String("secret", "", "shared secret for credential generation (required)")
	realm := flag.String("realm", "example.com", "authentication realm")
	ttl := flag.Duration("ttl", time.Hour, "credential lifetime")
	flag.Parse()

	if *publicIP == "" || *secret == "" {
		flag.Usage()
		os.Exit(1)
	}

	srv := &turn.Server{}
	err := srv.Start(turn.Config{
		ListenAddr: *listen,
		PublicIP:   *publicIP,
		Realm:      *realm,
		Dynamic: &turn.DynamicAuth{
			Secret: *secret,
			MaxTTL: 24 * time.Hour, // reject credentials claiming to live longer
		},
	})
	if err != nil {
		log.Fatalf("start turn server: %v", err)
	}
	defer srv.Close()

	log.Printf("TURN server running, URLs: %v", srv.URLs())

	// In a real deployment this handler sits behind your app's
	// authentication: verify the session first, then mint credentials for
	// the authenticated user id.
	http.HandleFunc("/credentials", func(w http.ResponseWriter, r *http.Request) {
		user := r.URL.Query().Get("user")
		if user == "" {
			http.Error(w, "missing ?user=", http.StatusBadRequest)
			return
		}

		// Username comes back as "<expiry>:<user>". The user id part is
		// what per-user quotas and events are keyed on.
		username, credential, err := srv.GenerateCredentials(user, *ttl)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"urls":       srv.URLs(),
			"username":   username,
			"credential": credential,
			"ttl":        ttl.String(),
		})
	})

	go func() {
		log.Printf("credential endpoint on http://%s/credentials?user=<id>", *httpListen)
		if err := http.ListenAndServe(*httpListen, nil); err != nil {
			log.Fatalf("http server: %v", err)
		}
	}()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	<-sigs
}
