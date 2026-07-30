package pipe

import "time"

// Label is one bounded-cardinality metric dimension. Peer IDs and session IDs
// are never used as label values.
type Label struct {
	Key   string
	Value string
}

// Metrics receives measurements without imposing an observability vendor.
// Implementations must be safe for concurrent use and must not block.
//
// The metric names and labels emitted by this package are:
//
//	pipe.dial.attempts        counter   -
//	pipe.dial.results         counter   result
//	pipe.accept.attempts      counter   -
//	pipe.accept.results       counter   result
//	pipe.sessions.active      gauge     -
//	pipe.sessions.pending     gauge     -
//	pipe.signal.sent          counter   kind, result
//	pipe.signal.received      counter   kind, result
//	pipe.connect.duration     duration  role
//	pipe.restart.attempts     counter   -
//	pipe.restart.results      counter   result
//	pipe.restart.duration     duration  -
//	pipe.stream.bytes.read    counter   -
//	pipe.stream.bytes.written counter   -
//	pipe.keepalive.rtt        duration  -
//	pipe.keepalive.failures   counter   reason
//	pipe.protocol.failures    counter   scope
type Metrics interface {
	// Count adds delta to a counter.
	Count(name string, delta int64, labels ...Label)

	// Gauge records the current value of a gauge.
	Gauge(name string, value int64, labels ...Label)

	// Duration records an observed duration.
	Duration(name string, d time.Duration, labels ...Label)
}

// nopMetrics discards every measurement.
type nopMetrics struct{}

func (nopMetrics) Count(string, int64, ...Label)            {}
func (nopMetrics) Gauge(string, int64, ...Label)            {}
func (nopMetrics) Duration(string, time.Duration, ...Label) {}

// Metric names recorded by this package.
const (
	metricDialAttempts      = "pipe.dial.attempts"
	metricDialResults       = "pipe.dial.results"
	metricAcceptAttempts    = "pipe.accept.attempts"
	metricAcceptResults     = "pipe.accept.results"
	metricSessionsActive    = "pipe.sessions.active"
	metricSessionsPending   = "pipe.sessions.pending"
	metricSignalSent        = "pipe.signal.sent"
	metricSignalReceived    = "pipe.signal.received"
	metricConnectDuration   = "pipe.connect.duration"
	metricRestartAttempts   = "pipe.restart.attempts"
	metricRestartResults    = "pipe.restart.results"
	metricRestartDuration   = "pipe.restart.duration"
	metricStreamBytesRead   = "pipe.stream.bytes.read"
	metricStreamBytesWrite  = "pipe.stream.bytes.written"
	metricKeepAliveRTT      = "pipe.keepalive.rtt"
	metricKeepAliveFailures = "pipe.keepalive.failures"
	metricProtocolFailures  = "pipe.protocol.failures"
)

func labelResult(v string) Label { return Label{Key: "result", Value: v} }
func labelKind(k SignalKind) Label {
	return Label{Key: "kind", Value: string(k)}
}
func labelRole(r role) Label     { return Label{Key: "role", Value: r.String()} }
func labelScope(v string) Label  { return Label{Key: "scope", Value: v} }
func labelReason(v string) Label { return Label{Key: "reason", Value: v} }
