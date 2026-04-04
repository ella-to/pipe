package signal

import (
	"testing"
)

func TestType_String(t *testing.T) {
	tests := []struct {
		t    Type
		want string
	}{
		{TypeExchange, "exchange"},
		{TypeOffer, "offer"},
		{TypeAnswer, "answer"},
		{TypeCandidate, "candidate"},
		{TypeFailed, "failed"},
		{Type(99), "unknown"},
	}

	for _, tt := range tests {
		if got := tt.t.String(); got != tt.want {
			t.Errorf("Type(%d).String() = %q, want %q", tt.t, got, tt.want)
		}
	}
}

func TestCreateMsg(t *testing.T) {
	msg := CreateMsg(TypeOffer, "test-payload")

	if msg.Type != TypeOffer {
		t.Errorf("Expected TypeOffer, got %v", msg.Type)
	}

	var payload string
	if err := msg.DecodeBody(&payload); err != nil {
		t.Fatalf("DecodeBody() error = %v", err)
	}

	if payload != "test-payload" {
		t.Errorf("Expected 'test-payload', got %q", payload)
	}
}

func TestCreateMsg_WithError(t *testing.T) {
	err := &testError{msg: "test error"}
	msg := CreateMsg(TypeFailed, err)

	if msg.Type != TypeFailed {
		t.Errorf("Expected TypeFailed, got %v", msg.Type)
	}

	var payload string
	if err := msg.DecodeBody(&payload); err != nil {
		t.Fatalf("DecodeBody() error = %v", err)
	}

	if payload != "test error" {
		t.Errorf("Expected 'test error', got %q", payload)
	}
}

func TestCreateMsg_Struct(t *testing.T) {
	type testStruct struct {
		Name  string `json:"name"`
		Value int    `json:"value"`
	}

	original := testStruct{Name: "test", Value: 42}
	msg := CreateMsg(TypeOffer, original)

	var decoded testStruct
	if err := msg.DecodeBody(&decoded); err != nil {
		t.Fatalf("DecodeBody() error = %v", err)
	}

	if decoded.Name != original.Name || decoded.Value != original.Value {
		t.Errorf("Decoded struct doesn't match original")
	}
}

func TestReceiverFunc(t *testing.T) {
	// This is a compile-time check that ReceiverFunc implements Receiver
	var _ Receiver = ReceiverFunc(nil)
}

type testError struct {
	msg string
}

func (e *testError) Error() string {
	return e.msg
}
