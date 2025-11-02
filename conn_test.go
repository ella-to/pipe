package pipe_test

import (
	"bytes"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"ella.to/pipe"
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

func TestBasicConn(t *testing.T) {
	sig := setupSignalClient(t)
	ctx := t.Context()

	pipe.SetStatusFunc(func(c *pipe.Conn, status webrtc.PeerConnectionState) {
		slog.Info("peer connection state changed", "id", c.ID(), "status", status)
	})

	data := strings.Repeat("Hello World", 1000)

	go func() {
		listener, err := pipe.CreateListener("alice", sig)
		if err != nil {
			t.Errorf("create listener: %v", err)
			return
		}

		c, err := listener.Listen(ctx)
		if err != nil {
			t.Errorf("listener listen: %v", err)
			return
		}

		err = c.WaitReady(ctx)
		if err != nil {
			t.Errorf("listener wait ready: %v", err)
			return
		}

		n, err := io.Copy(c, strings.NewReader(data))
		if err != nil {
			t.Errorf("conn copy write: %v", err)
			return
		}

		if n != int64(len(data)) {
			t.Errorf("expected to write %d bytes, wrote %d", len(data), n)
		}

		c.Close()
	}()

	dialer, err := pipe.CreateDialer("bob", sig)
	if err != nil {
		t.Fatalf("create dialer: %v", err)
	}

	c, err := dialer.Dial(ctx, "alice")
	if err != nil {
		t.Fatalf("dialer dial: %v", err)
	}

	if err = c.WaitReady(ctx); err != nil {
		t.Fatalf("dialer wait ready: %v", err)
	}

	var buffer bytes.Buffer

	_, err = io.Copy(&buffer, c)
	if err != nil {
		t.Fatalf("conn copy read: %v", err)
	}

	err = c.Close()
	if err != nil {
		t.Fatalf("conn close: %v", err)
	}

	if buffer.String() != data {
		t.Fatal("data mismatch")
	}

	time.Sleep(1 * time.Second)
}
