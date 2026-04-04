package turn

import (
	"testing"
	"time"
)

func TestServer_Start(t *testing.T) {
	s := &Server{}
	err := s.Start(Config{
		ListenAddr: "127.0.0.1:0",
		PublicIP:   "127.0.0.1",
		Realm:      "test.local",
		Users:      []User{{Username: "test", Password: "pass"}},
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer s.Close()

	if s.pc == nil {
		t.Fatal("PacketConn is nil after Start")
	}

	if s.srv == nil {
		t.Fatal("TURN server is nil after Start")
	}
}

func TestServer_StartDefaults(t *testing.T) {
	s := &Server{}
	err := s.Start(Config{
		ListenAddr: "127.0.0.1:0",
		PublicIP:   "127.0.0.1",
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer s.Close()

	if s.cfg.Realm != "ella.to" {
		t.Fatalf("Expected default realm 'ella.to', got %q", s.cfg.Realm)
	}
}

func TestServer_ICEServerFor(t *testing.T) {
	s := &Server{}
	err := s.Start(Config{
		ListenAddr: "127.0.0.1:0",
		PublicIP:   "192.0.2.1",
		Users:      []User{{Username: "alice", Password: "secret"}},
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer s.Close()

	ice := s.ICEServerFor("alice")

	if ice.Username != "alice" {
		t.Fatalf("Expected username 'alice', got %q", ice.Username)
	}

	if ice.Credential != "secret" {
		t.Fatalf("Expected credential 'secret', got %q", ice.Credential)
	}

	if len(ice.URLs) == 0 {
		t.Fatal("Expected at least one URL")
	}
}

func TestServer_Close(t *testing.T) {
	s := &Server{}
	err := s.Start(Config{
		ListenAddr: "127.0.0.1:0",
		PublicIP:   "127.0.0.1",
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	err = s.Close()
	if err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	// Double close may return error from already-closed resources, which is fine
	_ = s.Close()
}

func TestServer_TCPListener(t *testing.T) {
	s := &Server{}
	err := s.Start(Config{
		ListenAddr:    "127.0.0.1:0",
		TCPListenAddr: "127.0.0.1:0",
		PublicIP:      "127.0.0.1",
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer s.Close()

	if s.tcpL == nil {
		t.Fatal("TCP listener is nil")
	}
}

func TestServer_DynamicAuth(t *testing.T) {
	s := &Server{}
	err := s.Start(Config{
		ListenAddr: "127.0.0.1:0",
		PublicIP:   "127.0.0.1",
		Dynamic: &DynamicAuth{
			Secret: "test-secret",
			MaxTTL: 24 * time.Hour,
		},
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer s.Close()

	username, credential := s.GenerateRESTCredentials(1 * time.Hour)

	if username == "" {
		t.Fatal("Expected non-empty username")
	}

	if credential == "" {
		t.Fatal("Expected non-empty credential")
	}

	for _, ch := range username {
		if ch < '0' || ch > '9' {
			t.Fatalf("Username should be numeric, got %q", username)
		}
	}
}

func TestServer_GenerateRESTCredentials_NoDynamic(t *testing.T) {
	s := &Server{}
	err := s.Start(Config{
		ListenAddr: "127.0.0.1:0",
		PublicIP:   "127.0.0.1",
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer s.Close()

	username, credential := s.GenerateRESTCredentials(1 * time.Hour)

	if username != "" || credential != "" {
		t.Fatal("Expected empty credentials when Dynamic is not configured")
	}
}

func TestServer_PasswordFor(t *testing.T) {
	s := &Server{}
	err := s.Start(Config{
		ListenAddr: "127.0.0.1:0",
		PublicIP:   "127.0.0.1",
		Users: []User{
			{Username: "alice", Password: "secret1"},
			{Username: "bob", Password: "secret2"},
		},
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer s.Close()

	if p := s.passwordFor("alice"); p != "secret1" {
		t.Fatalf("Expected 'secret1', got %q", p)
	}

	if p := s.passwordFor("bob"); p != "secret2" {
		t.Fatalf("Expected 'secret2', got %q", p)
	}

	if p := s.passwordFor("unknown"); p != "" {
		t.Fatalf("Expected empty for unknown user, got %q", p)
	}
}

func TestServer_ListenAddrToPublic(t *testing.T) {
	s := &Server{}
	s.cfg.PublicIP = "203.0.113.10"

	tests := []struct {
		listenAddr string
		expected   string
	}{
		{"127.0.0.1:3478", "203.0.113.10:3478"},
		{":3478", "203.0.113.10:3478"},
		{"", ""},
	}

	for _, tt := range tests {
		result := s.listenAddrToPublic(tt.listenAddr)
		if result != tt.expected {
			t.Errorf("listenAddrToPublic(%q) = %q, want %q", tt.listenAddr, result, tt.expected)
		}
	}
}

func TestServer_ListenAddrToPublic_NoPublicIP(t *testing.T) {
	s := &Server{}
	s.cfg.PublicIP = ""

	result := s.listenAddrToPublic("127.0.0.1:3478")
	if result != "127.0.0.1:3478" {
		t.Errorf("Expected fallback to listen addr, got %q", result)
	}
}
