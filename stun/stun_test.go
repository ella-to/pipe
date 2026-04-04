package stun

import (
	"net"
	"testing"
)

func TestServer_Start(t *testing.T) {
	s := &Server{}
	err := s.Start(Config{
		ListenAddr: ":0",
		PublicAddr: "stun.example.com:3478",
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer s.Close()

	if s.pc == nil {
		t.Fatal("PacketConn is nil after Start")
	}

	addr := s.pc.LocalAddr().(*net.UDPAddr)
	if addr.Port == 0 {
		t.Fatal("Expected non-zero port")
	}
}

func TestServer_StartDefaultAddr(t *testing.T) {
	s := &Server{}
	err := s.Start(Config{
		ListenAddr: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer s.Close()

	if s.cfg.PublicAddr == "" {
		t.Fatal("PublicAddr should not be empty")
	}
}

func TestServer_ICEServer(t *testing.T) {
	s := &Server{}
	err := s.Start(Config{
		ListenAddr: "127.0.0.1:0",
		PublicAddr: "stun.example.com:3478",
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer s.Close()

	ice := s.ICEServer()
	if len(ice.URLs) != 1 {
		t.Fatalf("Expected 1 URL, got %d", len(ice.URLs))
	}

	expected := "stun:stun.example.com:3478"
	if ice.URLs[0] != expected {
		t.Fatalf("Expected URL %q, got %q", expected, ice.URLs[0])
	}
}

func TestServer_Close(t *testing.T) {
	s := &Server{}
	err := s.Start(Config{
		ListenAddr: "127.0.0.1:0",
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

func TestServer_StartInvalidAddr(t *testing.T) {
	s := &Server{}
	err := s.Start(Config{
		ListenAddr: "invalid:addr:format",
	})
	if err == nil {
		s.Close()
		t.Fatal("Expected error for invalid address")
	}
}
