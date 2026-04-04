package testsignal

import (
	"context"
	"testing"
	"time"

	"ella.to/pipe/signal"
)

func TestBus_SendReceive(t *testing.T) {
	bus := New()
	ctx := context.Background()

	inbox := signal.NewInboxRandom("test")

	receiver, err := bus.Receiver(inbox)
	if err != nil {
		t.Fatalf("Receiver() error = %v", err)
	}

	msg := signal.CreateMsg(signal.TypeOffer, "test-payload")

	go func() {
		time.Sleep(10 * time.Millisecond)
		if err := bus.Send(ctx, inbox, msg); err != nil {
			t.Errorf("Send() error = %v", err)
		}
	}()

	received, err := receiver.Receive(ctx)
	if err != nil {
		t.Fatalf("Receive() error = %v", err)
	}

	if received.Type != signal.TypeOffer {
		t.Fatalf("Expected TypeOffer, got %v", received.Type)
	}
}

func TestBus_ContextCancellation(t *testing.T) {
	bus := New()
	ctx, cancel := context.WithCancel(context.Background())

	inbox := signal.NewInboxRandom("test")

	receiver, err := bus.Receiver(inbox)
	if err != nil {
		t.Fatalf("Receiver() error = %v", err)
	}

	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	_, err = receiver.Receive(ctx)
	if err == nil {
		t.Fatal("Expected error on cancelled context")
	}
}

func TestBus_SendContextCancellation(t *testing.T) {
	bus := New()
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer cancel()

	temp := signal.NewInboxRandom("test")

	// Fill the inbox buffer first
	inbox := bus.getInbox(temp)
	for i := 0; i < cap(inbox); i++ {
		inbox <- signal.CreateMsg(signal.TypeOffer, "filler")
	}

	// Now Send should block and eventually timeout
	msg := signal.CreateMsg(signal.TypeOffer, "test")
	err := bus.Send(ctx, temp, msg)
	if err == nil {
		t.Fatal("Expected error when context expires on full inbox")
	}
}

func TestBus_MultipleInboxes(t *testing.T) {
	bus := New()
	ctx := context.Background()

	inbox1 := signal.NewInboxRandom("inbox-1")
	inbox2 := signal.NewInboxRandom("inbox-2")

	recv1, _ := bus.Receiver(inbox1)
	recv2, _ := bus.Receiver(inbox2)

	msg1 := signal.CreateMsg(signal.TypeOffer, "msg1")
	msg2 := signal.CreateMsg(signal.TypeAnswer, "msg2")

	go func() {
		_ = bus.Send(ctx, inbox1, msg1)
		_ = bus.Send(ctx, inbox2, msg2)
	}()

	r1, _ := recv1.Receive(ctx)
	r2, _ := recv2.Receive(ctx)

	if r1.Type != signal.TypeOffer {
		t.Fatalf("inbox-1: expected TypeOffer, got %v", r1.Type)
	}
	if r2.Type != signal.TypeAnswer {
		t.Fatalf("inbox-2: expected TypeAnswer, got %v", r2.Type)
	}
}
