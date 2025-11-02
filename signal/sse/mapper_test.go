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
