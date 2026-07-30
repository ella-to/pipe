package frame

import (
	"bytes"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

func TestGoldenVectors(t *testing.T) {
	tests := []struct {
		name    string
		typ     Type
		payload []byte
		hex     string
	}{
		{"empty data", TypeData, nil, "0101000000000000"},
		{"one byte of data", TypeData, []byte{0x41}, "010100000000000141"},
		{"three bytes of data", TypeData, []byte("abc"), "0101000000000003616263"},
		{"ping", TypePing, []byte{1, 2, 3, 4, 5, 6, 7, 8}, "01020000000000080102030405060708"},
		{"pong", TypePong, []byte{8, 7, 6, 5, 4, 3, 2, 1}, "01030000000000080807060504030201"},
		{"close, normal", TypeClose, EncodeClose(CloseNormal, ""), "0104000000000002" + "0000"},
		{"close, with reason", TypeClose, EncodeClose(CloseProtocolError, "bad"), "0104000000000005" + "0001" + "626164"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Append(nil, tc.typ, tc.payload)
			if err != nil {
				t.Fatalf("Append: %v", err)
			}
			if h := hex.EncodeToString(got); h != tc.hex {
				t.Errorf("Append = %s, want %s", h, tc.hex)
			}

			buf := make([]byte, HeaderSize+len(tc.payload))
			encoded, err := Encode(buf, tc.typ, tc.payload)
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			if !bytes.Equal(encoded, got) {
				t.Errorf("Encode and Append disagree")
			}

			hdr, payload, err := Parse(got, PayloadLimit)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if hdr.Version != Version || hdr.Type != tc.typ || hdr.Flags != 0 {
				t.Errorf("header = %+v", hdr)
			}
			if int(hdr.Length) != len(tc.payload) {
				t.Errorf("length = %d, want %d", hdr.Length, len(tc.payload))
			}
			if !bytes.Equal(payload, tc.payload) && !(len(payload) == 0 && len(tc.payload) == 0) {
				t.Errorf("payload = %x, want %x", payload, tc.payload)
			}
		})
	}
}

func TestParseRejectsInvalidFrames(t *testing.T) {
	valid, err := Append(nil, TypeData, []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		msg  []byte
		want string
	}{
		{"empty", nil, "shorter than"},
		{"truncated header", valid[:7], "shorter than"},
		{"truncated payload", valid[:HeaderSize+2], "does not match"},
		{"trailing bytes", append(bytes.Clone(valid), 0x00), "does not match"},
		{"bad version", mutate(valid, 0, 2), "unsupported frame version"},
		{"unknown type", mutate(valid, 1, 0x7f), "unknown frame type"},
		{"unknown flags", mutate(valid, 2, 0x01), "unknown frame flags"},
		{"short ping", mustFrame(t, TypePing, []byte{1, 2, 3}), "must be 8 bytes"},
		{"short close", mustFrame(t, TypeClose, []byte{1}), "2 byte code"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := Parse(tc.msg, PayloadLimit)
			if err == nil {
				t.Fatal("Parse accepted an invalid frame")
			}
			if !errors.Is(err, ErrProtocol) {
				t.Errorf("error %v does not match ErrProtocol", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestParseEnforcesNegotiatedLimit(t *testing.T) {
	msg, err := Append(nil, TypeData, bytes.Repeat([]byte("x"), 100))
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := Parse(msg, 50); err == nil {
		t.Fatal("Parse accepted a payload above the negotiated limit")
	} else if !strings.Contains(err.Error(), "negotiated") {
		t.Errorf("error = %q, want it to mention the negotiated limit", err)
	}

	if _, _, err := Parse(msg, 100); err != nil {
		t.Errorf("Parse rejected a payload at the limit: %v", err)
	}
}

// TestParseNeverAllocatesFromPeerLength proves the decoder validates the length
// field against the bytes actually present, so a hostile length cannot drive an
// allocation.
func TestParseNeverAllocatesFromPeerLength(t *testing.T) {
	msg := []byte{Version, byte(TypeData), 0, 0, 0xff, 0xff, 0xff, 0xff}
	if _, _, err := Parse(msg, PayloadLimit); err == nil {
		t.Fatal("Parse accepted a 4 GiB length with no payload")
	}
}

func TestEncodeRejectsOversizePayload(t *testing.T) {
	payload := make([]byte, PayloadLimit+1)
	if _, err := Append(nil, TypeData, payload); !errors.Is(err, ErrProtocol) {
		t.Errorf("Append error = %v, want ErrProtocol", err)
	}
	if _, err := Encode(make([]byte, HeaderSize+len(payload)), TypeData, payload); !errors.Is(err, ErrProtocol) {
		t.Errorf("Encode error = %v, want ErrProtocol", err)
	}
}

func TestEncodeRejectsSmallDestination(t *testing.T) {
	if _, err := Encode(make([]byte, 4), TypeData, []byte("x")); err == nil {
		t.Fatal("Encode accepted an undersized destination")
	}
}

func TestCloseRoundTrip(t *testing.T) {
	info, err := ParseClose(EncodeClose(CloseGoingAway, "shutting down"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Code != CloseGoingAway || info.Reason != "shutting down" {
		t.Errorf("info = %+v", info)
	}
	if got := info.String(); !strings.Contains(got, "shutting down") {
		t.Errorf("String = %q", got)
	}
	if got := (CloseInfo{Code: 7}).String(); got != "code 7" {
		t.Errorf("String = %q, want %q", got, "code 7")
	}
}

func TestEncodeCloseTruncatesOnRuneBoundary(t *testing.T) {
	// A multi-byte rune straddling the limit must be dropped whole.
	reason := strings.Repeat("a", MaxCloseReason-1) + "é"
	body := EncodeClose(CloseNormal, reason)

	info, err := ParseClose(body)
	if err != nil {
		t.Fatalf("ParseClose rejected a truncated reason: %v", err)
	}
	if strings.HasSuffix(info.Reason, "\xc3") {
		t.Error("truncation split a rune")
	}
	if len(info.Reason) > MaxCloseReason {
		t.Errorf("reason of %d bytes exceeds the limit", len(info.Reason))
	}
}

func TestParseCloseRejectsInvalidUTF8(t *testing.T) {
	body := append([]byte{0, 0}, 0xff, 0xfe)
	if _, err := ParseClose(body); !errors.Is(err, ErrProtocol) {
		t.Errorf("error = %v, want ErrProtocol", err)
	}
}

func TestTypeString(t *testing.T) {
	cases := map[Type]string{
		TypeData:  "data",
		TypePing:  "ping",
		TypePong:  "pong",
		TypeClose: "close",
		Type(9):   "unknown(0x09)",
	}
	for typ, want := range cases {
		if got := typ.String(); got != want {
			t.Errorf("Type(%d).String() = %q, want %q", typ, got, want)
		}
	}
}

func FuzzParse(f *testing.F) {
	seeds := [][]byte{
		nil,
		{1},
		{Version, byte(TypeData), 0, 0, 0, 0, 0, 0},
		{Version, byte(TypePing), 0, 0, 0, 0, 0, 8, 1, 2, 3, 4, 5, 6, 7, 8},
		{Version, byte(TypeClose), 0, 0, 0, 0, 0, 2, 0, 0},
		{Version, byte(TypeData), 0, 0, 0xff, 0xff, 0xff, 0xff},
	}
	for _, seed := range seeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		hdr, payload, err := Parse(data, 16<<10)
		if err != nil {
			if !errors.Is(err, ErrProtocol) {
				t.Fatalf("decode error %v does not match ErrProtocol", err)
			}
			return
		}
		if int(hdr.Length) != len(payload) {
			t.Fatalf("header length %d does not match the %d byte payload", hdr.Length, len(payload))
		}
		if hdr.Type == TypeClose {
			if _, err := ParseClose(payload); err != nil && !errors.Is(err, ErrProtocol) {
				t.Fatalf("ParseClose error %v does not match ErrProtocol", err)
			}
		}
		// A frame that parses must re-encode to the same bytes.
		again, err := Append(nil, hdr.Type, payload)
		if err != nil {
			t.Fatalf("Append rejected a parsed frame: %v", err)
		}
		if !bytes.Equal(again, data) {
			t.Fatalf("re-encoding changed the frame:\n got %x\nwant %x", again, data)
		}
	})
}

func mutate(src []byte, index int, value byte) []byte {
	out := bytes.Clone(src)
	out[index] = value
	return out
}

func mustFrame(t *testing.T, typ Type, payload []byte) []byte {
	t.Helper()
	// Build the frame without the per-type checks Parse applies, so the test can
	// feed a deliberately malformed control frame to the decoder.
	out, err := Append(nil, typ, payload)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
