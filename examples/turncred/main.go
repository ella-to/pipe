// Command turncred mints ephemeral TURN credentials for a relay that runs with
// a shared secret (turnserver -auth-secret, or coturn with use-auth-secret).
//
// This is the piece a service runs when a signed-in user asks for relay
// access: it needs the secret and the user's ID, and it produces a username and
// password that stop working at the expiry. Nothing is stored anywhere.
//
//	export PIPE_TURN_SECRET=$(openssl rand -hex 32)
//	go run ./examples/turncred -user alice@free -ttl 12h
//
// The plan a user gets is carried in the user ID as "name@plan"; see the
// turnserver command and guides/07-multi-user-relay.md.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"ella.to/pipe/examples/internal/turnx"
)

func main() {
	secret := flag.String("secret", "", "shared secret (or set PIPE_TURN_SECRET)")
	user := flag.String("user", "", "user ID, optionally with a plan suffix such as alice@free")
	ttl := flag.Duration("ttl", turnx.DefaultCredentialTTL, "how long the credentials stay valid")
	turnURL := flag.String("turn", "", "TURN URL to include in the JSON output, e.g. turn:relay.example.net:3478?transport=udp")
	asJSON := flag.Bool("json", false, "print a JSON object suitable for handing to a client")
	flag.Parse()

	if *secret == "" {
		*secret = strings.TrimSpace(os.Getenv("PIPE_TURN_SECRET"))
	}
	if *secret == "" || *user == "" {
		fmt.Fprintln(os.Stderr, "turncred: -secret (or PIPE_TURN_SECRET) and -user are required")
		os.Exit(2)
	}

	username, password, err := turnx.IssueCredentials(*secret, *user, *ttl)
	if err != nil {
		fmt.Fprintln(os.Stderr, "turncred:", err)
		os.Exit(1)
	}

	if *asJSON {
		out := map[string]any{
			"username":   username,
			"credential": password,
			"expires_at": time.Now().Add(*ttl).UTC().Format(time.RFC3339),
		}
		if *turnURL != "" {
			out["urls"] = []string{*turnURL}
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
		return
	}

	fmt.Printf("username:   %s\n", username)
	fmt.Printf("credential: %s\n", password)
	fmt.Printf("expires:    %s\n", time.Now().Add(*ttl).UTC().Format(time.RFC3339))
	fmt.Printf("\nPIPE_TURN_USERNAME=%q PIPE_TURN_PASSWORD=%q\n", username, password)
}
