package pipe

import "time"

// ConnectionState is the coarse, stable lifecycle state of a connection. Pion's
// own transport states stay internal.
type ConnectionState int

// Connection states. The lifecycle is:
//
//	new -> signaling -> connecting -> connected -> recovering -> connected
//	                                       \                        /
//	                                        ---> closing -> closed <-
//
// The transition into StateClosed happens exactly once.
const (
	// StateNew is the state of a session that has not begun negotiating.
	StateNew ConnectionState = iota

	// StateSignaling means the offer/answer exchange is in progress.
	StateSignaling

	// StateConnecting means descriptions are exchanged and connectivity is
	// being established.
	StateConnecting

	// StateConnected means the byte stream is usable.
	StateConnected

	// StateRecovering means connectivity was lost and bounded recovery is in
	// progress.
	StateRecovering

	// StateClosing means teardown has begun.
	StateClosing

	// StateClosed is terminal.
	StateClosed
)

// String implements [fmt.Stringer].
func (s ConnectionState) String() string {
	switch s {
	case StateNew:
		return "new"
	case StateSignaling:
		return "signaling"
	case StateConnecting:
		return "connecting"
	case StateConnected:
		return "connected"
	case StateRecovering:
		return "recovering"
	case StateClosing:
		return "closing"
	case StateClosed:
		return "closed"
	default:
		return "unknown"
	}
}

// CandidateType names the kind of ICE candidate selected for a connection.
type CandidateType string

// Candidate types reported in [ConnStats].
const (
	// CandidateUnknown means no pair has been selected or Pion did not report
	// one.
	CandidateUnknown CandidateType = ""
	// CandidateHost is a local interface address.
	CandidateHost CandidateType = "host"
	// CandidateServerReflexive is an address observed through STUN.
	CandidateServerReflexive CandidateType = "srflx"
	// CandidatePeerReflexive is an address learned during connectivity checks.
	CandidatePeerReflexive CandidateType = "prflx"
	// CandidateRelay is a TURN relay address.
	CandidateRelay CandidateType = "relay"
)

// ConnStats is a snapshot of one connection's counters. Snapshots are taken
// without stopping I/O, so counters may advance between fields.
type ConnStats struct {
	// State is the connection state at snapshot time.
	State ConnectionState

	// BytesRead counts application bytes returned by Read.
	BytesRead uint64

	// BytesWritten counts application bytes accepted by Write.
	BytesWritten uint64

	// FramesRead counts decoded stream frames, including control frames.
	FramesRead uint64

	// FramesWritten counts encoded stream frames, including control frames.
	FramesWritten uint64

	// EstablishedAt is when the connection became usable.
	EstablishedAt time.Time

	// ConnectDuration is how long negotiation took.
	ConnectDuration time.Duration

	// ICERestarts counts completed ICE restarts on this connection.
	ICERestarts int

	// LocalCandidate is the local candidate type of the selected pair.
	LocalCandidate CandidateType

	// RemoteCandidate is the remote candidate type of the selected pair.
	RemoteCandidate CandidateType

	// KeepAliveRTT is the round-trip time of the most recent successful
	// keepalive probe, or zero when keepalive is disabled or has not completed
	// a probe.
	KeepAliveRTT time.Duration
}

// role distinguishes the two negotiation roles of a session.
type role int

const (
	// roleOfferer created the session with an offer, i.e. it dialed.
	roleOfferer role = iota
	// roleAnswerer received an offer, i.e. it accepted.
	roleAnswerer
)

// String implements [fmt.Stringer].
func (r role) String() string {
	if r == roleOfferer {
		return "offerer"
	}
	return "answerer"
}
