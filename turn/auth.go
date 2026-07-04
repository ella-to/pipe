package turn

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"
)

// ErrDynamicAuthNotConfigured is returned by credential generation methods
// when Config.Dynamic is not set.
var ErrDynamicAuthNotConfigured = errors.New("turn: dynamic auth not configured")

// DynamicAuth enables time-limited credentials as described by the TURN REST
// API (draft-uberti-behave-turn-rest-00).
//
// Two username formats are accepted:
//
//	"<expiry>"          — plain expiry epoch seconds (GenerateRESTCredentials)
//	"<expiry>:<userID>" — REST format carrying a user id (GenerateCredentials)
//
// The credential is always base64(HMAC-SHA1(Secret, username)). The userID
// part, when present, is what quota accounting and events see, so per-user
// quotas keep working across credential rotations.
type DynamicAuth struct {
	// Secret is the shared secret used to derive credentials. Required.
	Secret string

	// MaxTTL caps how far in the future a credential may expire.
	// Defaults to 24h.
	MaxTTL time.Duration
}

func (d *DynamicAuth) maxTTL() time.Duration {
	if d.MaxTTL > 0 {
		return d.MaxTTL
	}
	return 24 * time.Hour
}

// credential derives base64(HMAC-SHA1(secret, username)).
func (d *DynamicAuth) credential(username string) string {
	mac := hmac.New(sha1.New, []byte(d.Secret))
	_, _ = mac.Write([]byte(username))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// authenticate validates a time-limited username and returns the user id and
// long-term credential key on success.
func (d *DynamicAuth) authenticate(ra *RequestAttributes) (userID string, key []byte, ok bool) {
	ts, user, hasUser := strings.Cut(ra.Username, ":")
	exp, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return "", nil, false
	}
	now := time.Now().Unix()
	if exp < now || exp-now > int64(d.maxTTL().Seconds()) {
		return "", nil, false
	}
	userID = ra.Username
	if hasUser && user != "" {
		userID = user
	}
	return userID, GenerateAuthKey(ra.Username, ra.Realm, d.credential(ra.Username)), true
}

// buildAuthHandler wires the configured authentication chain:
// Config.AuthHandler override, else dynamic credentials, then static users.
func (s *Server) buildAuthHandler() AuthHandler {
	if s.cfg.AuthHandler != nil {
		return s.cfg.AuthHandler
	}

	static := make(map[string][]byte, len(s.cfg.Users))
	for _, u := range s.cfg.Users {
		static[u.Username] = GenerateAuthKey(u.Username, s.cfg.Realm, u.Password)
	}
	dynamic := s.cfg.Dynamic

	return func(ra *RequestAttributes) (string, []byte, bool) {
		if dynamic != nil && dynamic.Secret != "" {
			if userID, key, ok := dynamic.authenticate(ra); ok {
				return userID, key, true
			}
		}
		if key, ok := static[ra.Username]; ok {
			return ra.Username, key, true
		}
		return "", nil, false
	}
}

// GenerateCredentials returns time-limited credentials bound to a user id
// (username is "<expiry>:<userID>"). The user id is what per-user quotas and
// events are keyed on. If userID is empty, the plain "<expiry>" format is
// used. ttl is clamped to Dynamic.MaxTTL.
func (s *Server) GenerateCredentials(userID string, ttl time.Duration) (username, credential string, err error) {
	d := s.cfg.Dynamic
	if d == nil || d.Secret == "" {
		return "", "", ErrDynamicAuthNotConfigured
	}
	if ttl <= 0 || ttl > d.maxTTL() {
		ttl = d.maxTTL()
	}
	username = strconv.FormatInt(time.Now().Add(ttl).Unix(), 10)
	if userID != "" {
		username += ":" + userID
	}
	return username, d.credential(username), nil
}

// GenerateRESTCredentials returns (username, credential) for time-limited
// TURN auth using the plain "<expiry>" username format. It returns empty
// strings when Dynamic is not configured.
//
// Prefer GenerateCredentials, which carries a user id for quota accounting.
func (s *Server) GenerateRESTCredentials(ttl time.Duration) (string, string) {
	username, credential, err := s.GenerateCredentials("", ttl)
	if err != nil {
		return "", ""
	}
	return username, credential
}
