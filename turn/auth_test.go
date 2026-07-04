package turn

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestDynamicAuth_Authenticate_RESTFormat(t *testing.T) {
	s := &Server{cfg: Config{Dynamic: &DynamicAuth{Secret: "test-secret"}}}

	username, credential, err := s.GenerateCredentials("alice", time.Hour)
	if err != nil {
		t.Fatalf("GenerateCredentials: %v", err)
	}
	if !strings.HasSuffix(username, ":alice") {
		t.Fatalf("expected username to end with ':alice', got %q", username)
	}
	if credential == "" {
		t.Fatal("expected non-empty credential")
	}

	userID, key, ok := s.cfg.Dynamic.authenticate(&RequestAttributes{
		Username: username,
		Realm:    "test.local",
	})
	if !ok {
		t.Fatal("expected authentication to succeed")
	}
	if userID != "alice" {
		t.Fatalf("expected userID 'alice', got %q", userID)
	}
	expected := GenerateAuthKey(username, "test.local", credential)
	if !bytes.Equal(key, expected) {
		t.Fatal("auth key mismatch")
	}
}

func TestDynamicAuth_Authenticate_Expired(t *testing.T) {
	d := &DynamicAuth{Secret: "test-secret"}
	past := time.Now().Add(-time.Minute).Unix()

	_, _, ok := d.authenticate(&RequestAttributes{
		Username: fmt.Sprintf("%d:alice", past),
		Realm:    "test.local",
	})
	if ok {
		t.Fatal("expected expired credential to be rejected")
	}
}

func TestDynamicAuth_Authenticate_BeyondMaxTTL(t *testing.T) {
	d := &DynamicAuth{Secret: "test-secret", MaxTTL: time.Hour}
	far := time.Now().Add(48 * time.Hour).Unix()

	_, _, ok := d.authenticate(&RequestAttributes{
		Username: fmt.Sprintf("%d:alice", far),
		Realm:    "test.local",
	})
	if ok {
		t.Fatal("expected credential beyond MaxTTL to be rejected")
	}
}

func TestDynamicAuth_Authenticate_MalformedUsername(t *testing.T) {
	d := &DynamicAuth{Secret: "test-secret"}

	for _, username := range []string{"", "alice", "notanumber:alice", "12x34"} {
		if _, _, ok := d.authenticate(&RequestAttributes{Username: username}); ok {
			t.Fatalf("expected malformed username %q to be rejected", username)
		}
	}
}

func TestServer_GenerateCredentials_ClampsTTL(t *testing.T) {
	s := &Server{cfg: Config{Dynamic: &DynamicAuth{Secret: "test-secret", MaxTTL: time.Hour}}}

	username, _, err := s.GenerateCredentials("bob", 48*time.Hour)
	if err != nil {
		t.Fatalf("GenerateCredentials: %v", err)
	}
	ts, _, _ := strings.Cut(username, ":")
	var exp int64
	if _, err := fmt.Sscanf(ts, "%d", &exp); err != nil {
		t.Fatalf("parse expiry: %v", err)
	}
	maxExp := time.Now().Add(time.Hour + time.Minute).Unix()
	if exp > maxExp {
		t.Fatalf("expected expiry clamped to MaxTTL, got %d (max %d)", exp, maxExp)
	}
}

func TestServer_GenerateCredentials_NotConfigured(t *testing.T) {
	s := &Server{}
	if _, _, err := s.GenerateCredentials("alice", time.Hour); err != ErrDynamicAuthNotConfigured {
		t.Fatalf("expected ErrDynamicAuthNotConfigured, got %v", err)
	}
}

func TestServer_CustomAuthHandler(t *testing.T) {
	called := false
	s := &Server{}
	err := s.Start(Config{
		ListenAddr: "127.0.0.1:0",
		PublicIP:   "127.0.0.1",
		Realm:      "test.local",
		AuthHandler: func(ra *RequestAttributes) (string, []byte, bool) {
			called = true
			return ra.Username, GenerateAuthKey(ra.Username, ra.Realm, "pass"), true
		},
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer s.Close()

	handler := s.buildAuthHandler()
	if _, _, ok := handler(&RequestAttributes{Username: "anyone", Realm: "test.local"}); !ok {
		t.Fatal("expected custom auth handler to accept")
	}
	if !called {
		t.Fatal("expected custom auth handler to be called")
	}
}
