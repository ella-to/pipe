package pipe

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const (
	testMsgID     = "0123456789abcdef0123456789abcdef"
	testSessionID = "fedcba9876543210fedcba9876543210"
)

func signal(kind SignalKind, payload string) Signal {
	sig := Signal{
		Version:   ProtocolVersion,
		ID:        testMsgID,
		SessionID: testSessionID,
		Kind:      kind,
		From:      "alice",
		To:        "bob",
	}
	if payload != "" {
		sig.Payload = json.RawMessage(payload)
	}
	return sig
}

// TestGoldenSignals pins the wire encoding of every kind. A change here is a
// protocol change and requires a new version or kind.
func TestGoldenSignals(t *testing.T) {
	tests := []struct {
		name string
		sig  Signal
		json string
	}{
		{
			"offer",
			signal(KindOffer, `{"sdp":"v=0\r\n"}`),
			`{"v":1,"id":"` + testMsgID + `","session_id":"` + testSessionID +
				`","kind":"offer","from":"alice","to":"bob","payload":{"sdp":"v=0\r\n"}}`,
		},
		{
			"answer",
			signal(KindAnswer, `{"sdp":"v=0\r\n"}`),
			`{"v":1,"id":"` + testMsgID + `","session_id":"` + testSessionID +
				`","kind":"answer","from":"alice","to":"bob","payload":{"sdp":"v=0\r\n"}}`,
		},
		{
			"candidate",
			signal(KindCandidate, `{"candidate":"candidate:1 1 udp 1 10.0.0.1 1 typ host","sdp_mid":"0","sdp_mline_index":0}`),
			`{"v":1,"id":"` + testMsgID + `","session_id":"` + testSessionID +
				`","kind":"candidate","from":"alice","to":"bob","payload":` +
				`{"candidate":"candidate:1 1 udp 1 10.0.0.1 1 typ host","sdp_mid":"0","sdp_mline_index":0}}`,
		},
		{
			"ice-complete",
			signal(KindICEComplete, ""),
			`{"v":1,"id":"` + testMsgID + `","session_id":"` + testSessionID +
				`","kind":"ice-complete","from":"alice","to":"bob"}`,
		},
		{
			"restart",
			signal(KindRestart, `{"generation":2,"sdp":"v=0\r\n"}`),
			`{"v":1,"id":"` + testMsgID + `","session_id":"` + testSessionID +
				`","kind":"restart","from":"alice","to":"bob","payload":{"generation":2,"sdp":"v=0\r\n"}}`,
		},
		{
			"reject",
			signal(KindReject, `{"code":"not_listening","reason":"peer is not accepting connections"}`),
			`{"v":1,"id":"` + testMsgID + `","session_id":"` + testSessionID +
				`","kind":"reject","from":"alice","to":"bob","payload":` +
				`{"code":"not_listening","reason":"peer is not accepting connections"}}`,
		},
		{
			"close",
			signal(KindClose, `{"code":"normal","reason":""}`),
			`{"v":1,"id":"` + testMsgID + `","session_id":"` + testSessionID +
				`","kind":"close","from":"alice","to":"bob","payload":{"code":"normal","reason":""}}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.sig.Validate(); err != nil {
				t.Fatalf("Validate rejected a valid signal: %v", err)
			}

			encoded, err := json.Marshal(tc.sig)
			if err != nil {
				t.Fatal(err)
			}
			if string(encoded) != tc.json {
				t.Errorf("encoding mismatch\n got %s\nwant %s", encoded, tc.json)
			}

			var back Signal
			if err := json.Unmarshal([]byte(tc.json), &back); err != nil {
				t.Fatal(err)
			}
			if err := back.Validate(); err != nil {
				t.Errorf("Validate rejected the decoded signal: %v", err)
			}
			if back.Kind != tc.sig.Kind || back.ID != tc.sig.ID || back.SessionID != tc.sig.SessionID {
				t.Errorf("round trip changed the envelope: %+v", back)
			}
		})
	}
}

func TestSignalValidation(t *testing.T) {
	tests := []struct {
		name string
		sig  Signal
		want string
	}{
		{
			"wrong version", func() Signal { s := signal(KindICEComplete, ""); s.Version = 2; return s }(),
			"unsupported signal version",
		},
		{
			"missing id", func() Signal { s := signal(KindICEComplete, ""); s.ID = ""; return s }(),
			"invalid signal id",
		},
		{
			"uppercase hex id", func() Signal { s := signal(KindICEComplete, ""); s.ID = strings.ToUpper(testMsgID); return s }(),
			"invalid signal id",
		},
		{
			"short id", func() Signal { s := signal(KindICEComplete, ""); s.ID = "abc"; return s }(),
			"invalid signal id",
		},
		{
			"bad session id", func() Signal { s := signal(KindICEComplete, ""); s.SessionID = "zzz"; return s }(),
			"invalid session id",
		},
		{"empty from", func() Signal { s := signal(KindICEComplete, ""); s.From = ""; return s }(), "invalid from"},
		{"empty to", func() Signal { s := signal(KindICEComplete, ""); s.To = ""; return s }(), "invalid to"},
		{
			"self addressed", func() Signal { s := signal(KindICEComplete, ""); s.To = s.From; return s }(),
			"addressed to its own sender",
		},
		{"unknown kind", signal(SignalKind("nope"), ""), "unknown signal kind"},
		{"offer without a payload", signal(KindOffer, ""), "payload is missing"},
		{"offer with an empty sdp", signal(KindOffer, `{"sdp":""}`), "sdp is empty"},
		{"offer with a malformed payload", signal(KindOffer, `{`), "decode signal payload"},
		{"offer with trailing data", signal(KindOffer, `{"sdp":"x"} {}`), "trailing data"},
		{"oversize sdp", signal(KindOffer, `{"sdp":"`+strings.Repeat("s", MaxSDPSize+1)+`"}`), "exceeds"},
		{"empty candidate", signal(KindCandidate, `{"candidate":""}`), "candidate is empty"},
		{
			"oversize candidate", signal(KindCandidate, `{"candidate":"`+strings.Repeat("c", MaxCandidateSize+1)+`"}`),
			"exceeds",
		},
		{
			"oversize username fragment",
			signal(KindCandidate, `{"candidate":"c","username_fragment":"`+strings.Repeat("u", 257)+`"}`),
			"username fragment",
		},
		{"ice-complete with a payload", signal(KindICEComplete, `{"sdp":"x"}`), "must not carry a payload"},
		{
			"restart with generation zero", signal(KindRestart, `{"generation":0,"sdp":"x"}`),
			"generation must be greater than zero",
		},
		{"unknown reject code", signal(KindReject, `{"code":"whatever"}`), "unknown reject code"},
		{
			"oversize reject reason",
			signal(KindReject, `{"code":"busy","reason":"`+strings.Repeat("r", MaxReasonLength+1)+`"}`), "exceeds",
		},
		{"unknown close code", signal(KindClose, `{"code":"whatever"}`), "unknown close code"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.sig.Validate()
			if err == nil {
				t.Fatal("Validate accepted an invalid signal")
			}
			if !errors.Is(err, ErrProtocol) {
				t.Errorf("error %v does not match ErrProtocol", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestSignalToleratesUnknownFields proves additive version 1 fields do not break
// older peers.
func TestSignalToleratesUnknownFields(t *testing.T) {
	raw := `{"v":1,"id":"` + testMsgID + `","session_id":"` + testSessionID +
		`","kind":"offer","from":"alice","to":"bob","payload":{"sdp":"v=0","future":42},"extra":"ignored"}`

	var sig Signal
	if err := json.Unmarshal([]byte(raw), &sig); err != nil {
		t.Fatal(err)
	}
	if err := sig.Validate(); err != nil {
		t.Fatalf("Validate rejected additive fields: %v", err)
	}
}

func TestSignalAcceptsUUIDIdentifiers(t *testing.T) {
	sig := signal(KindICEComplete, "")
	sig.ID = "5b7ce6e6-9f96-4f0b-9d4a-4b0f0f2a1e2c"
	sig.SessionID = "9c1e7a3d-2b4c-4e5f-8a9b-0c1d2e3f4a5b"

	if err := sig.Validate(); err != nil {
		t.Fatalf("Validate rejected canonical UUIDs: %v", err)
	}

	sig.ID = "5B7CE6E6-9F96-4F0B-9D4A-4B0F0F2A1E2C"
	if err := sig.Validate(); err == nil {
		t.Error("Validate accepted an uppercase UUID")
	}

	sig.ID = "5b7ce6e6x9f96-4f0b-9d4a-4b0f0f2a1e2c"
	if err := sig.Validate(); err == nil {
		t.Error("Validate accepted a UUID with a misplaced separator")
	}
}

func TestICECompleteAcceptsEmptyObjects(t *testing.T) {
	for _, payload := range []string{"", "{}", "null"} {
		sig := signal(KindICEComplete, payload)
		if err := sig.Validate(); err != nil {
			t.Errorf("Validate rejected an ice-complete payload of %q: %v", payload, err)
		}
	}
}

func TestNewIDIsRandomAndValid(t *testing.T) {
	seen := make(map[string]bool, 512)
	for range 512 {
		id := NewID()
		if !validID(id) {
			t.Fatalf("NewID produced an invalid identifier %q", id)
		}
		if seen[id] {
			t.Fatalf("NewID repeated %q", id)
		}
		seen[id] = true
	}
}

func TestNewSignalBuildsValidEnvelopes(t *testing.T) {
	sig, err := newSignal(KindOffer, "alice", "bob", testSessionID, sdpPayload{SDP: "v=0"})
	if err != nil {
		t.Fatal(err)
	}
	if err := sig.Validate(); err != nil {
		t.Fatalf("newSignal built an invalid envelope: %v", err)
	}
	if sig.ID == "" || sig.ID == testSessionID {
		t.Errorf("ID = %q, want a fresh random value", sig.ID)
	}

	// Building an envelope that cannot be valid must fail rather than emit it.
	if _, err := newSignal(KindOffer, "alice", "bob", testSessionID, sdpPayload{}); err == nil {
		t.Error("newSignal accepted an empty SDP")
	}
	if _, err := newSignal(KindOffer, "alice", "alice", testSessionID, sdpPayload{SDP: "v=0"}); err == nil {
		t.Error("newSignal accepted a self-addressed envelope")
	}
}

func TestRejectAndCloseCodeSets(t *testing.T) {
	valid := []RejectCode{
		RejectNotListening, RejectBusy, RejectUnauthorized,
		RejectUnsupportedVersion, RejectInvalidOffer, RejectInternal,
	}
	for _, code := range valid {
		if !code.valid() {
			t.Errorf("reject code %q should be valid", code)
		}
	}
	if RejectCode("other").valid() {
		t.Error("an unknown reject code was accepted")
	}

	for _, code := range []CloseCode{CloseNormal, CloseGoingAway, CloseProtocolError, CloseTimeout, CloseInternal} {
		if !code.valid() {
			t.Errorf("close code %q should be valid", code)
		}
	}
	if CloseCode("other").valid() {
		t.Error("an unknown close code was accepted")
	}
}

func FuzzSignalValidate(f *testing.F) {
	f.Add(`{"v":1,"id":"` + testMsgID + `","session_id":"` + testSessionID +
		`","kind":"offer","from":"a","to":"b","payload":{"sdp":"v=0"}}`)
	f.Add(`{"v":1,"kind":"candidate"}`)
	f.Add(`{}`)
	f.Add(`{"v":1,"id":"` + testMsgID + `","session_id":"` + testSessionID +
		`","kind":"close","from":"a","to":"b","payload":{"code":"normal"}}`)

	f.Fuzz(func(t *testing.T, raw string) {
		if len(raw) > MaxEnvelopeSize {
			return
		}
		var sig Signal
		if err := json.Unmarshal([]byte(raw), &sig); err != nil {
			return
		}
		err := sig.Validate()
		if err != nil && !errors.Is(err, ErrProtocol) {
			t.Fatalf("Validate error %v does not match ErrProtocol", err)
		}
		if err != nil {
			return
		}
		// A signal that validates must survive a re-encode and validate again.
		encoded, err := json.Marshal(sig)
		if err != nil {
			t.Fatalf("Marshal rejected a valid signal: %v", err)
		}
		var again Signal
		if err := json.Unmarshal(encoded, &again); err != nil {
			t.Fatalf("Unmarshal rejected a re-encoded signal: %v", err)
		}
		if err := again.Validate(); err != nil {
			t.Fatalf("a re-encoded valid signal became invalid: %v", err)
		}
	})
}
