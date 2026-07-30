package pipe

import "github.com/pion/webrtc/v4"

// PionOptions is the explicitly named escape hatch for tuning the WebRTC engine.
// It is the only place where Pion types appear in the public API, and the
// defaults are correct for ordinary use.
//
// Pipe always enables detached data channels and blocking data-channel writes
// before building the API, because the stream adapter owns reads, writes, and
// backpressure. Do not undo those settings.
type PionOptions struct {
	// ConfigureSettingEngine adjusts the setting engine once, before the shared
	// API is built. Use it for options such as network types, interface
	// filters, or a custom ICE UDP multiplexer.
	ConfigureSettingEngine func(*webrtc.SettingEngine)

	// ConfigureConfiguration adjusts the configuration applied to every
	// PeerConnection the endpoint creates. ICE servers and the transport policy
	// come from [Config] and are already applied.
	ConfigureConfiguration func(*webrtc.Configuration)
}
