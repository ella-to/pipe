// Package turn provides an embeddable TURN server built on pion/turn v5.
// It also answers STUN Binding requests on the same listeners.
//
// Features:
//   - Static username/password authentication
//   - Dynamic time-limited credentials (TURN REST API,
//     draft-uberti-behave-turn-rest-00)
//   - Per-user quotas: max concurrent allocations and max relay bandwidth
//   - UDP, TCP and TLS (TURNS) listeners
//   - Relay port range control
//   - Peer permission filtering and allocation lifecycle events
//
// The zero value of Server is ready to use:
//
//	srv := &turn.Server{}
//	err := srv.Start(turn.Config{
//		ListenAddr: ":3478",
//		PublicIP:   "203.0.113.10",
//		Users:      []turn.User{{Username: "alice", Password: "secret"}},
//	})
//
// See turn/examples for runnable examples from simple to advanced.
package turn

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/pion/turn/v5"
	"github.com/pion/webrtc/v4"
)

// Aliases to pion/turn v5 types so advanced callers don't need to import
// pion/turn directly.
type (
	// AuthHandler authenticates a TURN request. It returns the user id used
	// for quota accounting and events, the MD5 long-term credential key
	// (see GenerateAuthKey), and whether authentication succeeded.
	AuthHandler = turn.AuthHandler

	// RequestAttributes carries the attributes of the request being
	// authenticated (username, realm, source address, TLS state, method).
	RequestAttributes = turn.RequestAttributes

	// PermissionHandler filters CreatePermission / ChannelBind requests.
	// Return false to block the client (clientAddr) from reaching peerIP.
	PermissionHandler = turn.PermissionHandler

	// EventHandler is a set of optional callbacks fired at allocation
	// lifecycle hook points (created, deleted, permissions, channels, auth).
	EventHandler = turn.EventHandler
)

// GenerateAuthKey produces the long-term credential key stored/returned by an
// AuthHandler: MD5(username:realm:password).
func GenerateAuthKey(username, realm, password string) []byte {
	return turn.GenerateAuthKey(username, realm, password)
}

// User represents static auth credentials.
type User struct {
	Username string
	Password string
}

// Config controls the TURN server.
type Config struct {
	// ListenAddr is the local UDP address to bind, e.g. ":3478".
	// Defaults to ":3478".
	ListenAddr string

	// TCPListenAddr and TLSListenAddr optionally enable TURN over TCP and
	// TURNS (TLS) listeners.
	TCPListenAddr string
	TLSListenAddr string

	// TLS configuration for TLSListenAddr: either provide Cert/Key files or
	// a complete TLSConfig.
	TLSCertFile string
	TLSKeyFile  string
	TLSConfig   *tls.Config

	// PublicIP is the external IP address peers use to reach relays.
	// Required.
	PublicIP string

	// RelayMinPort/RelayMaxPort restrict the ports used for relay
	// allocations. If both are zero the system's ephemeral range is used.
	RelayMinPort uint16
	RelayMaxPort uint16

	// Realm is the authentication realm. Defaults to "ella.to".
	Realm string

	// Users is a static list of username/password pairs.
	Users []User

	// Dynamic enables time-limited credentials alongside (or instead of)
	// static Users.
	Dynamic *DynamicAuth

	// AuthHandler, when set, fully replaces the built-in authentication
	// (Users and Dynamic are ignored).
	AuthHandler AuthHandler

	// Quota enforces per-user allocation and bandwidth limits.
	Quota *Quota

	// PermissionHandler, when set, filters which peer IPs a client may
	// relay to. Defaults to allowing everything.
	PermissionHandler PermissionHandler

	// Events receives allocation lifecycle callbacks. All fields are
	// optional.
	Events EventHandler

	// Lifetimes. Zero values use pion/turn defaults (10 minutes each).
	AllocationLifetime time.Duration
	PermissionTimeout  time.Duration
	ChannelBindTimeout time.Duration
}

// Server is a TURN server using pion/turn.
type Server struct {
	cfg   Config
	pc    net.PacketConn
	tcpL  net.Listener
	tlsL  net.Listener
	srv   *turn.Server
	quota *quotaTracker
}

// Start launches the TURN server.
func (s *Server) Start(cfg Config) error {
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = ":3478"
	}
	if cfg.Realm == "" {
		cfg.Realm = "ella.to"
	}

	relayIP := net.ParseIP(cfg.PublicIP)
	if relayIP == nil {
		return fmt.Errorf("turn: invalid PublicIP %q", cfg.PublicIP)
	}

	s.cfg = cfg
	s.quota = newQuotaTracker()

	relayGen := s.buildRelayGenerator(relayIP)

	var (
		packetConns []turn.PacketConnConfig
		listeners   []turn.ListenerConfig
	)

	// UDP
	pc, err := net.ListenPacket("udp4", cfg.ListenAddr)
	if err != nil {
		return err
	}
	s.pc = pc
	// Persist actual addr (handles :0)
	s.cfg.ListenAddr = pc.LocalAddr().String()
	packetConns = append(packetConns, turn.PacketConnConfig{
		PacketConn:            pc,
		RelayAddressGenerator: relayGen,
		PermissionHandler:     cfg.PermissionHandler,
	})

	// TCP
	if cfg.TCPListenAddr != "" {
		tl, err := net.Listen("tcp", cfg.TCPListenAddr)
		if err != nil {
			s.closeListeners()
			return err
		}
		s.tcpL = tl
		s.cfg.TCPListenAddr = tl.Addr().String()
		listeners = append(listeners, turn.ListenerConfig{
			Listener:              tl,
			RelayAddressGenerator: relayGen,
			PermissionHandler:     cfg.PermissionHandler,
		})
	}

	// TLS (TURNS)
	if cfg.TLSListenAddr != "" {
		tconf := cfg.TLSConfig
		if tconf == nil && cfg.TLSCertFile != "" && cfg.TLSKeyFile != "" {
			cert, err := tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile)
			if err != nil {
				s.closeListeners()
				return err
			}
			tconf = &tls.Config{Certificates: []tls.Certificate{cert}}
		}
		if tconf == nil {
			s.closeListeners()
			return errors.New("turn: TLSListenAddr set but no TLS certificate provided")
		}
		tl, err := tls.Listen("tcp", cfg.TLSListenAddr, tconf)
		if err != nil {
			s.closeListeners()
			return err
		}
		s.tlsL = tl
		s.cfg.TLSListenAddr = tl.Addr().String()
		listeners = append(listeners, turn.ListenerConfig{
			Listener:              tl,
			RelayAddressGenerator: relayGen,
			PermissionHandler:     cfg.PermissionHandler,
		})
	}

	srv, err := turn.NewServer(turn.ServerConfig{
		Realm:              cfg.Realm,
		AuthHandler:        s.buildAuthHandler(),
		QuotaHandler:       s.buildQuotaHandler(),
		EventHandler:       s.buildEventHandler(),
		PacketConnConfigs:  packetConns,
		ListenerConfigs:    listeners,
		AllocationLifetime: cfg.AllocationLifetime,
		PermissionTimeout:  cfg.PermissionTimeout,
		ChannelBindTimeout: cfg.ChannelBindTimeout,
	})
	if err != nil {
		s.closeListeners()
		return err
	}

	s.srv = srv
	return nil
}

// Close stops the TURN server.
func (s *Server) Close() error {
	if s.srv != nil {
		err := s.srv.Close()
		s.srv = nil
		// pion/turn closes the PacketConn and listeners
		s.pc = nil
		s.tcpL = nil
		s.tlsL = nil
		return err
	}
	// Fallback cleanup if srv was never created
	s.closeListeners()
	return nil
}

func (s *Server) closeListeners() {
	if s.pc != nil {
		_ = s.pc.Close()
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
}

func (s *Server) buildRelayGenerator(relayIP net.IP) turn.RelayAddressGenerator {
	var gen turn.RelayAddressGenerator
	if s.cfg.RelayMinPort != 0 || s.cfg.RelayMaxPort != 0 {
		gen = &turn.RelayAddressGeneratorPortRange{
			RelayAddress: relayIP,
			Address:      "0.0.0.0",
			MinPort:      s.cfg.RelayMinPort,
			MaxPort:      s.cfg.RelayMaxPort,
		}
	} else {
		gen = &turn.RelayAddressGeneratorStatic{
			RelayAddress: relayIP,
			Address:      "0.0.0.0",
		}
	}
	if s.cfg.Quota.hasBandwidthLimit() {
		gen = &bandwidthLimitedGenerator{
			RelayAddressGenerator: gen,
			quota:                 s.cfg.Quota,
			tracker:               s.quota,
		}
	}
	return gen
}

func (s *Server) buildEventHandler() EventHandler {
	events := s.cfg.Events
	handler := events
	handler.OnAllocationCreated = func(srcAddr, dstAddr net.Addr, protocol, userID, realm string,
		relayAddr net.Addr, requestedPort int,
	) {
		s.quota.inc(userID)
		if events.OnAllocationCreated != nil {
			events.OnAllocationCreated(srcAddr, dstAddr, protocol, userID, realm, relayAddr, requestedPort)
		}
	}
	handler.OnAllocationDeleted = func(srcAddr, dstAddr net.Addr, protocol, userID, realm string) {
		s.quota.dec(userID)
		if events.OnAllocationDeleted != nil {
			events.OnAllocationDeleted(srcAddr, dstAddr, protocol, userID, realm)
		}
	}
	return handler
}

func (s *Server) buildQuotaHandler() turn.QuotaHandler {
	quota := s.cfg.Quota
	if quota == nil {
		return nil
	}
	return func(userID, realm string, srcAddr net.Addr) bool {
		max := quota.forUser(userID).MaxAllocations
		if max <= 0 {
			return true
		}
		return s.quota.count(userID) < max
	}
}

// ActiveAllocations returns the number of live allocations for a user id.
// For static users the user id is the username; for dynamic credentials
// created with GenerateCredentials it is the userID argument.
func (s *Server) ActiveAllocations(userID string) int {
	return s.quota.count(userID)
}

// TotalAllocations returns the number of live allocations across all users.
func (s *Server) TotalAllocations() int {
	return s.quota.total()
}

// URLs returns the TURN URLs (turn:/turns:) that clients should use,
// derived from the configured listeners and PublicIP.
func (s *Server) URLs() []string {
	urls := []string{}
	if addr := s.listenAddrToPublic(s.cfg.ListenAddr); addr != "" {
		urls = append(urls, fmt.Sprintf("turn:%s?transport=udp", addr))
	}
	if addr := s.listenAddrToPublic(s.cfg.TCPListenAddr); addr != "" {
		urls = append(urls, fmt.Sprintf("turn:%s?transport=tcp", addr))
	}
	if addr := s.listenAddrToPublic(s.cfg.TLSListenAddr); addr != "" {
		urls = append(urls, fmt.Sprintf("turns:%s?transport=tcp", addr))
	}
	return urls
}

// ICEServerFor returns a webrtc.ICEServer for a given static user.
func (s *Server) ICEServerFor(username string) webrtc.ICEServer {
	return webrtc.ICEServer{
		URLs:       s.URLs(),
		Username:   username,
		Credential: s.passwordFor(username),
	}
}

// ICEServerForDynamic returns a webrtc.ICEServer with freshly generated
// time-limited credentials for the given user id. Requires Config.Dynamic.
func (s *Server) ICEServerForDynamic(userID string, ttl time.Duration) (webrtc.ICEServer, error) {
	username, credential, err := s.GenerateCredentials(userID, ttl)
	if err != nil {
		return webrtc.ICEServer{}, err
	}
	return webrtc.ICEServer{
		URLs:       s.URLs(),
		Username:   username,
		Credential: credential,
	}, nil
}

func (s *Server) passwordFor(username string) string {
	for _, u := range s.cfg.Users {
		if u.Username == username {
			return u.Password
		}
	}
	return ""
}

// listenAddrToPublic returns the public host:port to use in ICE URLs.
// If listenAddr is host:port and PublicIP is set, this returns PublicIP:port.
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
