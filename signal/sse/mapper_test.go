package sse_test

import (
	"testing"
	"time"

	"ella.to/pipe/signal"
	"ella.to/pipe/signal/sse"
)

func TestMapperGetReturnsBufferedChannel(t *testing.T) {
	m := sse.NewMapper()

	ch := m.Get("inbox")
	if ch == nil {
		t.Fatal("expected channel, got nil")
	}

	select {
	case ch <- &signal.Msg{}:
	default:
		t.Fatal("expected buffered channel to accept value")
	}
}

func TestMapperGetReturnsSameChannel(t *testing.T) {
	m := sse.NewMapper()

	ch1 := m.Get("shared")
	ch2 := m.Get("shared")

	if ch1 != ch2 {
		t.Fatal("expected same channel instance for repeated Get")
	}
}

func TestMapperDeleteClosesChannel(t *testing.T) {
	m := sse.NewMapper()
	inbox := "closing"

	ch := m.Get(inbox)
	go func() {
		ch <- &signal.Msg{}
	}()

	if msg := <-ch; msg == nil {
		t.Fatal("expected message before delete")
	}

	m.Delete(inbox)

	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected channel to be closed after delete")
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timeout waiting for channel close")
	}
}

func TestMapperDeleteCreatesNewChannel(t *testing.T) {
	m := sse.NewMapper()
	inbox := "reuse"

	ch1 := m.Get(inbox)
	m.Delete(inbox)

	select {
	case _, ok := <-ch1:
		if ok {
			t.Fatal("expected original channel to be closed")
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timeout waiting for original channel to close")
	}

	ch2 := m.Get(inbox)
	if ch1 == ch2 {
		t.Fatal("expected new channel instance after delete")
	}
}

func TestMapperUnsubscribeRemovesIdleEntry(t *testing.T) {
	m := sse.NewMapper()
	inbox := "subscriber"

	ch := m.Subscribe(inbox)
	if m.Len() != 1 {
		t.Fatalf("expected 1 entry after subscribe, got %d", m.Len())
	}

	m.Unsubscribe(inbox, ch)
	if m.Len() != 0 {
		t.Fatalf("expected entry to be removed after last unsubscribe, got %d", m.Len())
	}
}

func TestMapperUnsubscribeKeepsBufferedEntry(t *testing.T) {
	m := sse.NewMapper()
	inbox := "buffered"

	ch := m.Subscribe(inbox)
	ch <- &signal.Msg{}

	m.Unsubscribe(inbox, ch)
	if m.Len() != 1 {
		t.Fatalf("expected buffered entry to be kept for redelivery, got %d", m.Len())
	}

	// A new subscriber must observe the same channel and drain the message.
	ch2 := m.Subscribe(inbox)
	if ch2 != ch {
		t.Fatal("expected same channel for buffered inbox")
	}
	select {
	case <-ch2:
	default:
		t.Fatal("expected buffered message")
	}
}

func TestMapperUnsubscribeWithConcurrentResubscribe(t *testing.T) {
	m := sse.NewMapper()
	inbox := "reconnect"

	ch1 := m.Subscribe(inbox)
	ch2 := m.Subscribe(inbox) // new connection attaches before old detaches
	m.Unsubscribe(inbox, ch1)

	if m.Len() != 1 {
		t.Fatalf("expected entry to survive while a subscriber remains, got %d", m.Len())
	}
	if ch1 != ch2 {
		t.Fatal("expected shared channel")
	}
}
