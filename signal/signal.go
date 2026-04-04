package signal

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/rs/xid"
)

var ErrInvalidInbox = errors.New("invalid inbox format")

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
	} else if jsonRaw, ok := content.(json.RawMessage); ok {
		// we need to make a copy of jsonRaw to avoid data
		jsonRawCopy := make([]byte, len(jsonRaw))
		copy(jsonRawCopy, jsonRaw)

		return &Msg{
			Type: typ,
			Body: jsonRawCopy,
		}
	}

	encoded, _ := json.Marshal(content)

	return &Msg{
		Type: typ,
		Body: encoded,
	}
}

type Sender interface {
	Send(ctx context.Context, inbox *Inbox, msg *Msg) error
}

type Receiver interface {
	Receive(ctx context.Context) (*Msg, error)
}

type ReceiverCloser interface {
	Receiver
	Close() error
}

type Signal interface {
	Sender
	Receiver(inbox *Inbox) (Receiver, error)
}

type ReceiverFunc func(ctx context.Context) (*Msg, error)

func (f ReceiverFunc) Receive(ctx context.Context) (*Msg, error) {
	return f(ctx)
}

type Inbox struct {
	Id  string
	Ext string
}

func (i *Inbox) MarshalText() ([]byte, error) {
	return []byte(i.String()), nil
}

func (i *Inbox) UnmarshalText(text []byte) error {
	parsed, err := ParseInbox(string(text))
	if err != nil {
		return err
	}
	i.Id = parsed.Id
	i.Ext = parsed.Ext
	return nil
}

func (i *Inbox) IsMain() bool {
	return i.Ext == "inbox"
}

func (i *Inbox) String() string {
	return i.Id + "." + i.Ext
}

func NewInbox(id, ext string) *Inbox {
	return &Inbox{
		Id:  id,
		Ext: ext,
	}
}

func NewInboxRandom(id string) *Inbox {
	return NewInbox(id, xid.New().String())
}

func NewInboxMain(id string) *Inbox {
	return NewInbox(id, "inbox")
}

func ParseInbox(inbox string) (*Inbox, error) {
	var i int
	var found bool

	for i = range inbox {
		if inbox[i] == '.' {
			found = true
			break
		}
	}

	if !found {
		return nil, ErrInvalidInbox
	}

	if i == 0 || i == len(inbox)-1 {
		return nil, ErrInvalidInbox
	}

	return &Inbox{
		Id:  inbox[:i],
		Ext: inbox[i+1:],
	}, nil
}
