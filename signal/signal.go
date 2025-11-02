package signal

import (
	"context"
	"encoding/json"
)

type Type int

const (
	_ Type = iota
	TypeExchange
	TypeOffer
	TypeAnswer
	TypeCandidate
	TypeFailed
)

func (t Type) String() string {
	switch t {
	case TypeExchange:
		return "exchange"
	case TypeOffer:
		return "offer"
	case TypeAnswer:
		return "answer"
	case TypeCandidate:
		return "candidate"
	case TypeFailed:
		return "failed"
	default:
		return "unknown"
	}
}

type Msg struct {
	Type Type
	Body json.RawMessage
}

func (m *Msg) DecodeBody(ptr any) error {
	return json.Unmarshal(m.Body, ptr)
}

func CreateMsg(typ Type, content any) *Msg {
	if err, ok := content.(error); ok {
		content = err.Error()
	}

	encoded, _ := json.Marshal(content)

	return &Msg{
		Type: typ,
		Body: encoded,
	}
}

type Sender interface {
	Send(ctx context.Context, inbox string, msg *Msg) error
}

type Receiver interface {
	Receive(ctx context.Context) (*Msg, error)
}

type Signal interface {
	Sender
	Receiver(inbox string) (Receiver, error)
}

type ReceiverFunc func(ctx context.Context) (*Msg, error)

func (f ReceiverFunc) Receive(ctx context.Context) (*Msg, error) {
	return f(ctx)
}
