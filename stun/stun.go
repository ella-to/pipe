package stun

import (
	"fmt"
	"net"

	"github.com/pion/turn/v4"
	"github.com/pion/webrtc/v4"
)

// Server is a minimal STUN server implemented using pion/turn's server core.
// It responds to STUN Binding requests on the configured UDP address.
// Note: This server does not provide TURN relaying; see the turn package for that.
type Server struct {
	cfg Config
	pc  net.PacketConn
	srv *turn.Server
}

// Config controls the STUN server.
// ListenAddr is the local UDP address to bind, e.g. ":3478".
// PublicAddr is the host:port that clients should use to reach this server
// (IP or DNS name plus port). It is used for generating ICE server URLs.
type Config struct {
	ListenAddr string
	PublicAddr string
}

// Start launches the STUN server.
func (s *Server) Start(cfg Config) error {
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = ":3478"
	}
	if cfg.PublicAddr == "" {
		cfg.PublicAddr = cfg.ListenAddr
	}
	s.cfg = cfg

	pc, err := net.ListenPacket("udp4", cfg.ListenAddr)
	if err != nil {
		return err
	}
	s.pc = pc
	// capture the actual listen address (handles :0)
	s.cfg.ListenAddr = pc.LocalAddr().String()

	srv, err := turn.NewServer(turn.ServerConfig{
		// STUN Binding requests do not require auth; TURN allocations aren't supported here
		Realm:       "stun",
		AuthHandler: func(username, realm string, srcAddr net.Addr) ([]byte, bool) { return nil, false },
		PacketConnConfigs: []turn.PacketConnConfig{
			{
				PacketConn: pc,
				// No RelayAddressGenerator necessary for pure STUN
			},
		},
	})
	if err != nil {
		_ = pc.Close()
		return err
	}
	s.srv = srv
	return nil
}

// Close stops the STUN server.
func (s *Server) Close() error {
	if s.srv != nil {
		err := s.srv.Close()
		s.srv = nil
		s.pc = nil // pion/turn closes the PacketConn
		return err
	}
	// Fallback if srv was never created but pc was
	if s.pc != nil {
		err := s.pc.Close()
		s.pc = nil
		return err
	}
	return nil
}

// ICEServer returns a webrtc.ICEServer that clients can use to reach this STUN server.
func (s *Server) ICEServer() webrtc.ICEServer {
	return webrtc.ICEServer{
		URLs: []string{fmt.Sprintf("stun:%s", s.cfg.PublicAddr)},
	}
}
