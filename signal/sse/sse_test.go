package sse_test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"ella.to/pipe/signal"
	"ella.to/pipe/signal/sse"
)

func setupSignalClient(t *testing.T) *sse.Client {
	server, err := sse.NewServer()
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	ts := httptest.NewServer(server)
	t.Cleanup(ts.Close)

	client, err := sse.NewClient(ts.URL)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	return client
}

func TestBasicSSE(t *testing.T) {
	client := setupSignalClient(t)

	inbox := signal.NewInboxRandom("test")

	done := make(chan struct{})

	go func() {
		defer close(done)

		recv, err := client.Receiver(inbox)
		if err != nil {
			t.Errorf("create receiver: %v", err)
			return
		}
		if closer, ok := recv.(signal.ReceiverCloser); ok {
			defer closer.Close()
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		msg, err := recv.Receive(ctx)
		if err != nil {
			t.Errorf("receive message: %v", err)
			return
		}

		var body string
		err = msg.DecodeBody(&body)
		if err != nil {
			t.Errorf("decode message body: %v", err)
			return
		}

		if msg.Type != signal.TypeExchange || body != "hello world" {
			t.Errorf("received message does not match sent message")
			return
		}
	}()

	msg := signal.CreateMsg(signal.TypeExchange, "hello world")
	err := client.Send(context.Background(), inbox, msg)
	if err != nil {
		t.Fatalf("send message: %v", err)
	}

	<-done
	fmt.Println("Basic SSE test completed successfully")
}

func TestClientsExchangeMessagesThroughServer(t *testing.T) {
	client := setupSignalClient(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	inbox := signal.NewInboxRandom("test")

	msgCh := make(chan *signal.Msg, 1)
	errCh := make(chan error, 1)

	go func() {
		r, recvErr := client.Receiver(inbox)
		if recvErr != nil {
			errCh <- recvErr
			return
		}
		if closer, ok := r.(signal.ReceiverCloser); ok {
			defer closer.Close()
		}

		msg, recvErr := r.Receive(ctx)
		if recvErr != nil {
			errCh <- recvErr
			return
		}
		msgCh <- msg
	}()

	msg := signal.CreateMsg(signal.TypeOffer, map[string]string{"payload": "ping"})

	err := client.Send(ctx, inbox, msg)
	if err != nil {
		t.Fatalf("client A send: %v", err)
	}

	select {
	case receivedMsg := <-msgCh:
		if receivedMsg.Type != msg.Type || string(receivedMsg.Body) != string(msg.Body) {
			t.Fatalf("received message does not match sent message")
		}
	case err := <-errCh:
		t.Fatalf("receiver error: %v", err)
	case <-ctx.Done():
		t.Fatal("test timed out waiting for message")
	}
}

func TestServerSendFailsWhenInboxFull(t *testing.T) {
	client := setupSignalClient(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	inbox := signal.NewInboxRandom("slow")

	msg := signal.CreateMsg(signal.TypeOffer, map[string]string{"data": "value"})

	for i := range 16 {
		if err := client.Send(ctx, inbox, msg); err != nil {
			t.Fatalf("send %d failed: %v", i, err)
		}
	}

	if err := client.Send(ctx, inbox, msg); err == nil {
		t.Fatal("expected send to fail when inbox is full")
	}
}

func TestReceiverCanBeClosed(t *testing.T) {
	client := setupSignalClient(t)
	inbox := signal.NewInboxRandom("close")

	recv, err := client.Receiver(inbox)
	if err != nil {
		t.Fatalf("create receiver: %v", err)
	}

	closer, ok := recv.(signal.ReceiverCloser)
	if !ok {
		t.Fatal("expected receiver to implement signal.ReceiverCloser")
	}

	if err := closer.Close(); err != nil {
		t.Fatalf("close receiver: %v", err)
	}
}
