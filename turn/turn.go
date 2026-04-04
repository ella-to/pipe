package turn

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"time"

	"github.com/pion/turn/v4"
	"github.com/pion/webrtc/v4"
)

// Server is a TURN server using pion/turn.
// It also handles STUN Binding requests on the same port.
type Server struct {
	cfg  Config
	pc   net.PacketConn
	tcpL net.Listener
	tlsL net.Listener
	srv  *turn.Server
}

// User represents static auth credentials.
type User struct {
	Username string
	Password string
}

// Config controls the TURN server.
//
// ListenAddr is the local UDP address to bind, e.g. ":3478".
// PublicIP is the external/public IP address that peers will use to reach relays.
// RelayMinPort/RelayMaxPort control the relay port range.
// Realm is the authentication realm.
// Users is a static list of username/password pairs (long-term credential).
type Config struct {
	// UDP listener (TURN over UDP)
	ListenAddr string

	// TCP/TLS listeners (optional)
	TCPListenAddr string
	TLSListenAddr string

	// TLS configuration (either provide Cert/Key files or TLSConfig)
	TLSCertFile string
	TLSKeyFile  string
	TLSConfig   *tls.Config

	// Public relay settings
	PublicIP     string
	RelayMinPort uint16 // optional; if zero, default system range is used
	RelayMaxPort uint16 // optional; if zero, default system range is used
	Realm        string
	Users        []User // static users

	// Dynamic (REST) credentials
	Dynamic *DynamicAuth
}

// DynamicAuth enables time-limited credentials.
// Username is an expiry epoch (seconds), credential is base64(HMAC-SHA1(secret, username)).
// Requests are accepted if not expired and within MaxTTL.
type DynamicAuth struct {
	Secret string
	MaxTTL time.Duration
}

// Start launches the TURN server.
func (s *Server) Start(cfg Config) error {
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = ":3478"
	}
	if cfg.Realm == "" {
		cfg.Realm = "ella.to"
	}
	// Optional port range; if left zero, the generator will use system defaults

	s.cfg = cfg
	var (
		packetConns []turn.PacketConnConfig
		listeners   []turn.ListenerConfig
	)

	// UDP
	if cfg.ListenAddr != "" {
		pc, err := net.ListenPacket("udp4", cfg.ListenAddr)
		if err != nil {
			return err
		}
		s.pc = pc
		// Persist actual addr (handles :0)
		s.cfg.ListenAddr = pc.LocalAddr().String()
		// Configure relay address generator for UDP as well
		// (relayGen is defined below)
		// We'll append after relayGen is constructed.
		packetConns = append(packetConns, turn.PacketConnConfig{PacketConn: pc})
	}

	// Build a map for quick lookup in AuthHandler
	userPass := map[string]string{}
	for _, u := range cfg.Users {
		userPass[u.Username] = u.Password
	}

	relayGen := &turn.RelayAddressGeneratorStatic{
		RelayAddress: net.ParseIP(cfg.PublicIP),
		Address:      "0.0.0.0",
		// Port range omitted; defaults in the library will be used
	}

	// TCP
	if cfg.TCPListenAddr != "" {
		tl, err := net.Listen("tcp", cfg.TCPListenAddr)
		if err != nil {
			if s.pc != nil {
				_ = s.pc.Close()
			}
			return err
		}
		s.tcpL = tl
		s.cfg.TCPListenAddr = tl.Addr().String()
		listeners = append(listeners, turn.ListenerConfig{Listener: tl, RelayAddressGenerator: relayGen})
	}

	// TLS (TURNS)
	if cfg.TLSListenAddr != "" {
		var tconf *tls.Config
		if cfg.TLSConfig != nil {
			tconf = cfg.TLSConfig
		} else if cfg.TLSCertFile != "" && cfg.TLSKeyFile != "" {
			cert, err := tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile)
			if err != nil {
				if s.pc != nil {
					_ = s.pc.Close()
				}
				if s.tcpL != nil {
					_ = s.tcpL.Close()
				}
				return err
			}
			tconf = &tls.Config{Certificates: []tls.Certificate{cert}}
		}
		if tconf != nil {
			tl, err := tls.Listen("tcp", cfg.TLSListenAddr, tconf)
			if err != nil {
				if s.pc != nil {
					_ = s.pc.Close()
				}
				if s.tcpL != nil {
					_ = s.tcpL.Close()
				}
				return err
			}
			s.tlsL = tl
			s.cfg.TLSListenAddr = tl.Addr().String()
			listeners = append(listeners, turn.ListenerConfig{Listener: tl, RelayAddressGenerator: relayGen})
		}
	}

	// attach relay generator to UDP PacketConn entries
	if len(packetConns) > 0 {
		for i := range packetConns {
			packetConns[i].RelayAddressGenerator = relayGen
		}
	}

	srv, err := turn.NewServer(turn.ServerConfig{
		Realm: cfg.Realm,
		AuthHandler: func(username, realm string, srcAddr net.Addr) ([]byte, bool) {
			// Dynamic credentials first
			if cfg.Dynamic != nil && cfg.Dynamic.Secret != "" && cfg.Dynamic.MaxTTL > 0 {
				if valid, pass := s.validateDynamic(username, cfg.Dynamic); valid {
					key := turn.GenerateAuthKey(username, realm, pass)
					return key, true
				}
			}
			// Static users
			pass, ok := userPass[username]
			if ok {
				key := turn.GenerateAuthKey(username, realm, pass)
				return key, true
			}
			return nil, false
		},
		PacketConnConfigs: packetConns,
		ListenerConfigs:   listeners,
	})
	if err != nil {
		if s.pc != nil {
			_ = s.pc.Close()
		}
		if s.tcpL != nil {
			_ = s.tcpL.Close()
		}
		if s.tlsL != nil {
			_ = s.tlsL.Close()
		}
		return err
	}

	s.srv = srv
	return nil
}

// Close stops the TURN server.
func (s *Server) Close() error {
	var srvErr error
	if s.srv != nil {
		srvErr = s.srv.Close()
		s.srv = nil
		// pion/turn closes the PacketConn and listeners
		s.pc = nil
		s.tcpL = nil
		s.tlsL = nil
		return srvErr
	}
	// Fallback cleanup if srv was never created
	var err error
	if s.pc != nil {
		err = s.pc.Close()
		s.pc = nil
	}
	if s.tcpL != nil {
		_ = s.tcpL.Close()
		s.tcpL = nil
	}
	if s.tlsL != nil {
		_ = s.tlsL.Close()
		s.tlsL = nil
	}
	return err
}

// ICEServer returns a webrtc.ICEServer for a given static user.
// The PublicAddr should be the externally reachable host:port, i.e., PublicIP:port.
func (s *Server) ICEServerFor(username string) webrtc.ICEServer {
	return webrtc.ICEServer{
		URLs:       s.turnURLs(),
		Username:   username,
		Credential: s.passwordFor(username),
	}
}

func (s *Server) passwordFor(username string) string {
	for _, u := range s.cfg.Users {
		if u.Username == username {
			return u.Password
		}
	}
	return ""
}

// ListenAddrToPublic returns the public host:port to use in ICE URLs.
// If ListenAddr is host:port and PublicIP is set, this returns PublicIP:port.
func (s *Server) listenAddrToPublic(listenAddr string) string {
	if listenAddr == "" {
		return ""
	}
	_, port, err := net.SplitHostPort(listenAddr)
	if err != nil || port == "" {
		return s.cfg.PublicIP
	}
	if s.cfg.PublicIP == "" {
		return listenAddr
	}
	return net.JoinHostPort(s.cfg.PublicIP, port)
}

func (s *Server) turnURLs() []string {
	urls := []string{}
	if s.pc != nil || s.cfg.ListenAddr != "" {
		if addr := s.listenAddrToPublic(s.cfg.ListenAddr); addr != "" {
			urls = append(urls, fmt.Sprintf("turn:%s?transport=udp", addr))
		}
	}
	if s.tcpL != nil || s.cfg.TCPListenAddr != "" {
		if addr := s.listenAddrToPublic(s.cfg.TCPListenAddr); addr != "" {
			urls = append(urls, fmt.Sprintf("turn:%s?transport=tcp", addr))
		}
	}
	if s.tlsL != nil || s.cfg.TLSListenAddr != "" {
		if addr := s.listenAddrToPublic(s.cfg.TLSListenAddr); addr != "" {
			urls = append(urls, fmt.Sprintf("turns:%s?transport=tcp", addr))
		}
	}
	return urls
}

func (s *Server) validateDynamic(username string, d *DynamicAuth) (bool, string) {
	if d == nil || d.Secret == "" || d.MaxTTL <= 0 {
		return false, ""
	}
	// Parse expiry as integer seconds
	var exp int64
	for i := 0; i < len(username); i++ {
		ch := username[i]
		if ch < '0' || ch > '9' {
			return false, ""
		}
		exp = exp*10 + int64(ch-'0')
	}
	now := time.Now().Unix()
	if exp < now || exp-now > int64(d.MaxTTL.Seconds()) {
		return false, ""
	}
	mac := hmac.New(sha1.New, []byte(d.Secret))
	_, _ = mac.Write([]byte(username))
	pass := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return true, pass
}

// GenerateRESTCredentials returns (username, credential) for time-limited TURN auth.
func (s *Server) GenerateRESTCredentials(ttl time.Duration) (string, string) {
	if s.cfg.Dynamic == nil || s.cfg.Dynamic.Secret == "" {
		return "", ""
	}
	exp := time.Now().Add(ttl).Unix()
	uname := fmt.Sprintf("%d", exp)
	mac := hmac.New(sha1.New, []byte(s.cfg.Dynamic.Secret))
	_, _ = mac.Write([]byte(uname))
	cred := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return uname, cred
}
