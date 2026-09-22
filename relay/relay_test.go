package relay_test

import (
	"context"
	"io"
	"testing"
	"time"

	"ella.to/pipe"
	"ella.to/pipe/relay"
	"ella.to/pipe/signaling/memory"
)

func TestParseSize(t *testing.T) {
	cases := map[string]int64{
		"0":      0,
		"512":    512,
		"512KiB": 512 << 10,
		"4MiB":   4 << 20,
		"1MB":    1000 * 1000,
		"2K":     2 << 10,
	}
	for in, want := range cases {
		got, err := relay.ParseSize(in)
		if err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "-1", "fast"} {
		if _, err := relay.ParseSize(bad); err == nil {
			t.Errorf("ParseSize(%q) succeeded", bad)
		}
	}
}

func TestParsePlans(t *testing.T) {
	plans, err := relay.ParsePlans("free=512KiB/4, pro=8MiB/32, unlimited=0")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]relay.Plan{
		"free":      {Rate: 512 << 10, MaxAllocations: 4},
		"pro":       {Rate: 8 << 20, MaxAllocations: 32},
		"unlimited": {},
	}
	if len(plans) != len(want) {
		t.Fatalf("got %d plans, want %d", len(plans), len(want))
	}
	for name, p := range want {
		if plans[name] != p {
			t.Errorf("plan %s = %+v, want %+v", name, plans[name], p)
		}
	}
	if _, err := relay.ParsePlans("free"); err == nil {
		t.Error("ParsePlans accepted an entry without a rate")
	}
}

func TestParseUsers(t *testing.T) {
	users, err := relay.ParseUsers("alice=a:free,bob=b")
	if err != nil {
		t.Fatal(err)
	}
	if users["alice"] != (relay.User{Password: "a", Plan: "free"}) || users["bob"] != (relay.User{Password: "b"}) {
		t.Fatalf("unexpected users %+v", users)
	}
	if _, err := relay.ParseUsers("alice="); err == nil {
		t.Error("ParseUsers accepted an empty password")
	}
}

func TestParsePortRange(t *testing.T) {
	lo, hi, err := relay.ParsePortRange("49152-49252")
	if err != nil || lo != 49152 || hi != 49252 {
		t.Fatalf("ParsePortRange = %d, %d, %v", lo, hi, err)
	}
	if _, _, err := relay.ParsePortRange("49252-49152"); err == nil {
		t.Error("ParsePortRange accepted a reversed range")
	}
}

func TestStartRejectsUnknownPlan(t *testing.T) {
	_, err := relay.Start(relay.Config{
		Users: map[string]relay.User{"alice": {Password: "a", Plan: "gold"}},
	})
	if err == nil {
		t.Fatal("Start accepted a user with an unknown plan")
	}
}

func TestIssueCredentialsRequiresSecret(t *testing.T) {
	if _, _, err := relay.IssueCredentials("", "alice", time.Hour); err == nil {
		t.Error("IssueCredentials succeeded without a secret")
	}
	if _, _, err := relay.IssueCredentials("s", "a:b", time.Hour); err == nil {
		t.Error("IssueCredentials accepted a user ID containing ':'")
	}
}

// TestRelayedConnection forces a pipe connection through the relay with
// ephemeral credentials and checks that the plan in the user ID was applied.
func TestRelayedConnection(t *testing.T) {
	const secret = "test-secret"

	srv, err := relay.Start(relay.Config{
		Listen:     "127.0.0.1:0",
		AuthSecret: secret,
		Plans:      map[string]relay.Plan{"pro": {Rate: 8 << 20, MaxAllocations: 4}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	user, pass, err := relay.IssueCredentials(secret, "alice@pro", time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	hub := memory.New()
	newEndpoint := func(id pipe.PeerID) *pipe.Endpoint {
		ep, err := pipe.New(ctx, pipe.Config{
			ID:       id,
			Signaler: hub,
			ICEServers: []pipe.ICEServer{{
				URLs:       []string{srv.TURNURL()},
				Username:   user,
				Credential: pass,
			}},
			ICETransportPolicy: pipe.ICETransportPolicyRelay,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ep.Close() })
		return ep
	}

	server := newEndpoint("server")
	client := newEndpoint("client")

	ln, err := server.Listen()
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.Copy(conn, conn)
	}()

	conn, err := client.Dial(ctx, "server")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if st := conn.Stats(); st.LocalCandidate != pipe.CandidateRelay || st.RemoteCandidate != pipe.CandidateRelay {
		t.Fatalf("candidates %s/%s, want relay/relay", st.LocalCandidate, st.RemoteCandidate)
	}

	msg := []byte("through the relay")
	if _, err := conn.Write(msg); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != string(msg) {
		t.Fatalf("echo = %q, want %q", got, msg)
	}

	us, ok := srv.Stats().Users["alice@pro"]
	if !ok {
		t.Fatalf("no statistics for alice@pro: %+v", srv.Stats().Users)
	}
	if us.Plan != "pro" || us.Allocations == 0 {
		t.Fatalf("alice@pro stats = %+v, want plan pro with allocations", us)
	}
}
