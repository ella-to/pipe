package sse

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"

	"ella.to/pipe"
)

// PeerHeader is the header in which a client states the peer ID it intends to
// act as. The server compares it with the authenticated identity and refuses a
// mismatch, so a misconfigured client fails at Open rather than silently
// signaling under the wrong name.
const PeerHeader = "X-Pipe-Peer"

// ErrUnauthorized reports a request whose identity could not be established or
// does not match the peer it claims to be.
var ErrUnauthorized = errors.New("sse: unauthorized")

// Authenticator establishes the peer identity behind a request. It is the
// only trust decision the server makes; everything else follows from it.
type Authenticator interface {
	// Authenticate returns the peer ID the request is allowed to act as, or an
	// error. The error is never sent to the client.
	Authenticate(r *http.Request) (pipe.PeerID, error)
}

// AuthenticatorFunc adapts a function to [Authenticator].
type AuthenticatorFunc func(r *http.Request) (pipe.PeerID, error)

// Authenticate implements [Authenticator].
func (f AuthenticatorFunc) Authenticate(r *http.Request) (pipe.PeerID, error) { return f(r) }

// StaticTokens authenticates requests by bearer token. The map is copied, and
// each token is bound to exactly one peer ID. Tokens should be long random
// strings; compare them with the ones issued to your users, not with peer IDs.
//
//	auth := sse.StaticTokens(map[string]pipe.PeerID{
//		os.Getenv("ALICE_TOKEN"): "alice",
//		os.Getenv("BOB_TOKEN"):   "bob",
//	})
func StaticTokens(tokens map[string]pipe.PeerID) Authenticator {
	table := make(map[string]pipe.PeerID, len(tokens))
	for token, peer := range tokens {
		if token == "" || peer == "" {
			continue
		}
		table[token] = peer
	}
	return AuthenticatorFunc(func(r *http.Request) (pipe.PeerID, error) {
		token, ok := BearerToken(r)
		if !ok {
			return "", ErrUnauthorized
		}
		// Look up by iterating so that the comparison is constant-time per
		// candidate; token tables are small.
		for candidate, peer := range table {
			if len(candidate) == len(token) &&
				subtle.ConstantTimeCompare([]byte(candidate), []byte(token)) == 1 {
				return peer, nil
			}
		}
		return "", ErrUnauthorized
	})
}

// TrustPeerHeader believes the peer ID in [PeerHeader] without any proof. It
// exists so that two processes on one machine can be wired together in a
// minute. Never expose a server using it to a network you do not control.
func TrustPeerHeader() Authenticator {
	return AuthenticatorFunc(func(r *http.Request) (pipe.PeerID, error) {
		peer := pipe.PeerID(strings.TrimSpace(r.Header.Get(PeerHeader)))
		if peer == "" {
			return "", ErrUnauthorized
		}
		return peer, nil
	})
}

// BearerToken extracts the token from an Authorization: Bearer header.
func BearerToken(r *http.Request) (string, bool) {
	const prefix = "bearer "
	h := r.Header.Get("Authorization")
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(h[len(prefix):])
	return token, token != ""
}
