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
	server := sse.NewServer()
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

	inbox := "basic-test-inbox"

	done := make(chan struct{})

	go func() {
		defer close(done)

		recv, err := client.Receiver(inbox)
		if err != nil {
			t.Errorf("create receiver: %v", err)
			return
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

	inbox := "test-inbox"

	msgCh := make(chan *signal.Msg, 1)
	errCh := make(chan error, 1)

	go func() {
		r, recvErr := client.Receiver(inbox)
		if recvErr != nil {
			errCh <- recvErr
			return
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

	inbox := "slow"

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
