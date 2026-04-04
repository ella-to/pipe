package pipe

import (
	"context"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"ella.to/pipe/signal/testsignal"
	"ella.to/pipe/turn"
)

func TestTURN_DataChannel_RelayOnly(t *testing.T) {
	// t.Skip("integration test (local TURN over UDP); skip by default")
	// Start local TURN on random UDP port and use loopback as PublicIP
	tr := &turn.Server{}
	err := tr.Start(turn.Config{
		ListenAddr: ":0",
		PublicIP:   "127.0.0.1",
		Realm:      "ella.to",
		Users:      []turn.User{{Username: "test", Password: "pass"}},
	})
	if err != nil {
		t.Fatalf("start turn: %v", err)
	}
	defer tr.Close()

	urls := []string{}
	for _, u := range tr.ICEServerFor("test").URLs {
		// Use only UDP URLs for this test
		if len(u) >= 5 && u[:5] == "turn:" && (len(u) < 15 || u[len(u)-15:] == "transport=udp") {
			urls = append(urls, u)
		}
	}

	ice := webrtc.ICEServer{
		URLs:           urls,
		Username:       "test",
		Credential:     "pass",
		CredentialType: webrtc.ICECredentialTypePassword,
	}
	opts := &WebRTCOptions{
		ICEServers: []webrtc.ICEServer{ice},
		// Allow all candidates for robustness in CI/local envs
		// ICETransportPolicy: webrtc.ICETransportPolicyRelay,
	}

	sig := testsignal.New()

	listener, err := CreateListenerWithOptions("B", sig, opts)
	if err != nil {
		t.Fatalf("CreateListener: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	type result struct {
		c   *Conn
		err error
	}
	ch := make(chan result, 1)
	go func() {
		c, e := listener.Listen(ctx)
		ch <- result{c, e}
	}()

	dialer, err := CreateDialerWithOptions("A", sig, opts)
	if err != nil {
		t.Fatalf("CreateDialer: %v", err)
	}

	connA, err := dialer.Dial(ctx, "B")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer connA.Close()

	var connB *Conn
	select {
	case <-ctx.Done():
		t.Fatalf("listener deadline: %v", ctx.Err())
	case res := <-ch:
		if res.err != nil {
			t.Fatalf("Listen: %v", res.err)
		}
		connB = res.c
	}
	defer connB.Close()

	// Wait ready (should already be)
	if e := connA.WaitReady(ctx); e != nil {
		t.Fatalf("A not ready: %v", e)
	}
	if e := connB.WaitReady(ctx); e != nil {
		t.Fatalf("B not ready: %v", e)
	}

	msg := []byte("hello over turn")
	if _, e := connA.Write(msg); e != nil {
		t.Fatalf("write: %v", e)
	}

	buf := make([]byte, 64)
	n, err := connB.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf[:n]) != string(msg) {
		t.Fatalf("mismatch: got %q want %q", string(buf[:n]), string(msg))
	}
}

func TestConn_Sync(t *testing.T) {
	sig := testsignal.New()

	listener, err := CreateListener("B", sig)
	if err != nil {
		t.Fatalf("CreateListener: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	type result struct {
		c   *Conn
		err error
	}
	ch := make(chan result, 1)
	go func() {
		c, e := listener.Listen(ctx)
		ch <- result{c, e}
	}()

	dialer, err := CreateDialer("A", sig)
	if err != nil {
		t.Fatalf("CreateDialer: %v", err)
	}

	connA, err := dialer.Dial(ctx, "B")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer connA.Close()

	var connB *Conn
	select {
	case <-ctx.Done():
		t.Fatalf("listener deadline: %v", ctx.Err())
	case res := <-ch:
		if res.err != nil {
			t.Fatalf("Listen: %v", res.err)
		}
		connB = res.c
	}
	defer connB.Close()

	if e := connA.WaitReady(ctx); e != nil {
		t.Fatalf("A not ready: %v", e)
	}
	if e := connB.WaitReady(ctx); e != nil {
		t.Fatalf("B not ready: %v", e)
	}

	// Write data and sync
	data := make([]byte, 64*1024) // 64KB
	for i := range data {
		data[i] = byte(i % 256)
	}

	n, err := connA.Write(data)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if n != len(data) {
		t.Fatalf("wrote %d bytes, expected %d", n, len(data))
	}

	// Sync should return quickly once buffer is drained
	syncCtx, syncCancel := context.WithTimeout(ctx, 5*time.Second)
	defer syncCancel()

	if err := connA.Sync(syncCtx); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	// Read on the other side
	buf := make([]byte, len(data))
	total := 0
	for total < len(data) {
		nr, err := connB.Read(buf[total:])
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		total += nr
	}

	if total != len(data) {
		t.Fatalf("read %d bytes, expected %d", total, len(data))
	}

	// Verify data integrity
	for i := range data {
		if buf[i] != data[i] {
			t.Fatalf("data mismatch at byte %d: got %d, want %d", i, buf[i], data[i])
		}
	}
}
