package pipe_test

import (
	"bytes"
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"ella.to/pipe"
	"ella.to/pipe/signal/testsignal"
)

func TestConn_Stream_SingleStream(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	bus := testsignal.New()

	dialer, err := pipe.CreateDialer("dialer", bus)
	if err != nil {
		t.Fatalf("CreateDialer failed: %v", err)
	}

	listener, err := pipe.CreateListener("listener", bus)
	if err != nil {
		t.Fatalf("CreateListener failed: %v", err)
	}

	var (
		dialerConn   *pipe.Conn
		listenerConn *pipe.Conn
	)

	// Start listener. The accepted conn is handed back over a channel so the
	// main goroutine's read happens-after the listener goroutine's write
	// (avoids a data race on a shared variable that a bare time.Sleep cannot).
	listenerConnCh := make(chan *pipe.Conn, 1)
	go func() {
		conn, lerr := listener.Listen(ctx)
		if lerr != nil {
			t.Errorf("Listen failed: %v", lerr)
			close(listenerConnCh)
			return
		}
		listenerConnCh <- conn
	}()

	// Dial
	dialerConn, err = dialer.Dial(ctx, "listener")
	if err != nil {
		t.Fatalf("Dial failed: %v", err)
	}
	defer dialerConn.Close()

	// Wait for the listener to accept and hand back its connection.
	listenerConn = <-listenerConnCh
	if listenerConn == nil {
		t.Fatal("listener returned no connection")
	}

	// Test single stream
	testData := []byte("hello from dialer stream")

	// Open stream on dialer side
	dialerStream, _, err := dialerConn.Stream()
	if err != nil {
		t.Fatalf("dialerConn.Stream() failed: %v", err)
	}
	defer dialerStream.Close()

	// Accept stream on listener side
	listenerStream, _, err := listenerConn.Stream()
	if err != nil {
		t.Fatalf("listenerConn.Stream() failed: %v", err)
	}
	defer listenerStream.Close()

	// Send data
	_, err = dialerStream.Write(testData)
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	// Receive data
	buf := make([]byte, len(testData))
	_, err = io.ReadFull(listenerStream, buf)
	if err != nil {
		t.Fatalf("ReadFull failed: %v", err)
	}

	if !bytes.Equal(buf, testData) {
		t.Errorf("Data mismatch: got %q, want %q", buf, testData)
	}

	// Verify Read/Write on Conn now returns error
	_, err = dialerConn.Read(make([]byte, 10))
	if err != pipe.ErrMultiplexingActive {
		t.Errorf("Expected ErrMultiplexingActive from Read, got: %v", err)
	}

	_, err = dialerConn.Write([]byte("test"))
	if err != pipe.ErrMultiplexingActive {
		t.Errorf("Expected ErrMultiplexingActive from Write, got: %v", err)
	}

	listenerConn.Close()
}

func TestConn_Stream_MultipleStreams(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	bus := testsignal.New()

	dialer, err := pipe.CreateDialer("dialer", bus)
	if err != nil {
		t.Fatalf("CreateDialer failed: %v", err)
	}

	listener, err := pipe.CreateListener("listener", bus)
	if err != nil {
		t.Fatalf("CreateListener failed: %v", err)
	}

	var (
		dialerConn   *pipe.Conn
		listenerConn *pipe.Conn
	)

	// Start listener. The accepted conn is handed back over a channel so the
	// main goroutine's read happens-after the listener goroutine's write
	// (avoids a data race on a shared variable that a bare time.Sleep cannot).
	listenerConnCh := make(chan *pipe.Conn, 1)
	go func() {
		conn, lerr := listener.Listen(ctx)
		if lerr != nil {
			t.Errorf("Listen failed: %v", lerr)
			close(listenerConnCh)
			return
		}
		listenerConnCh <- conn
	}()

	// Dial
	dialerConn, err = dialer.Dial(ctx, "listener")
	if err != nil {
		t.Fatalf("Dial failed: %v", err)
	}
	defer dialerConn.Close()

	// Wait for the listener to accept and hand back its connection.
	listenerConn = <-listenerConnCh
	if listenerConn == nil {
		t.Fatal("listener returned no connection")
	}

	const numStreams = 3
	messages := []string{
		"stream 0 message",
		"stream 1 message",
		"stream 2 message",
	}

	// Create multiple streams on dialer side
	dialerStreams := make([]io.ReadWriteCloser, numStreams)
	for i := 0; i < numStreams; i++ {
		dialerStreams[i], _, err = dialerConn.Stream()
		if err != nil {
			t.Fatalf("dialerConn.Stream() %d failed: %v", i, err)
		}
		defer dialerStreams[i].Close()
	}

	// Accept streams on listener side (should match in order)
	listenerStreams := make([]io.ReadWriteCloser, numStreams)
	for i := 0; i < numStreams; i++ {
		listenerStreams[i], _, err = listenerConn.Stream()
		if err != nil {
			t.Fatalf("listenerConn.Stream() %d failed: %v", i, err)
		}
		defer listenerStreams[i].Close()
	}

	// Verify NumStreams
	if n := dialerConn.NumStreams(); n != numStreams {
		t.Errorf("dialerConn.NumStreams() = %d, want %d", n, numStreams)
	}
	if n := listenerConn.NumStreams(); n != numStreams {
		t.Errorf("listenerConn.NumStreams() = %d, want %d", n, numStreams)
	}

	// Send data on each stream
	for i, msg := range messages {
		_, err = dialerStreams[i].Write([]byte(msg))
		if err != nil {
			t.Fatalf("Write on stream %d failed: %v", i, err)
		}
	}

	// Receive and verify data on each stream
	for i, expectedMsg := range messages {
		buf := make([]byte, len(expectedMsg))
		_, err = io.ReadFull(listenerStreams[i], buf)
		if err != nil {
			t.Fatalf("ReadFull on stream %d failed: %v", i, err)
		}
		if string(buf) != expectedMsg {
			t.Errorf("Stream %d: got %q, want %q", i, string(buf), expectedMsg)
		}
	}

	listenerConn.Close()
}

func TestConn_Stream_Bidirectional(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	bus := testsignal.New()

	dialer, err := pipe.CreateDialer("dialer", bus)
	if err != nil {
		t.Fatalf("CreateDialer failed: %v", err)
	}

	listener, err := pipe.CreateListener("listener", bus)
	if err != nil {
		t.Fatalf("CreateListener failed: %v", err)
	}

	var (
		dialerConn   *pipe.Conn
		listenerConn *pipe.Conn
	)

	// Start listener. The accepted conn is handed back over a channel so the
	// main goroutine's read happens-after the listener goroutine's write
	// (avoids a data race on a shared variable that a bare time.Sleep cannot).
	listenerConnCh := make(chan *pipe.Conn, 1)
	go func() {
		conn, lerr := listener.Listen(ctx)
		if lerr != nil {
			t.Errorf("Listen failed: %v", lerr)
			close(listenerConnCh)
			return
		}
		listenerConnCh <- conn
	}()

	// Dial
	dialerConn, err = dialer.Dial(ctx, "listener")
	if err != nil {
		t.Fatalf("Dial failed: %v", err)
	}
	defer dialerConn.Close()

	// Wait for the listener to accept and hand back its connection.
	listenerConn = <-listenerConnCh
	if listenerConn == nil {
		t.Fatal("listener returned no connection")
	}

	// Open stream on dialer
	dialerStream, _, err := dialerConn.Stream()
	if err != nil {
		t.Fatalf("dialerConn.Stream() failed: %v", err)
	}
	defer dialerStream.Close()

	// Accept stream on listener
	listenerStream, _, err := listenerConn.Stream()
	if err != nil {
		t.Fatalf("listenerConn.Stream() failed: %v", err)
	}
	defer listenerStream.Close()

	// Test bidirectional communication
	dialerMsg := []byte("hello from dialer")
	listenerMsg := []byte("hello from listener")

	var recvWg sync.WaitGroup
	recvWg.Add(2)

	// Dialer sends, listener receives
	go func() {
		defer recvWg.Done()
		buf := make([]byte, len(dialerMsg))
		if _, err := io.ReadFull(listenerStream, buf); err != nil {
			t.Errorf("listener ReadFull failed: %v", err)
			return
		}
		if !bytes.Equal(buf, dialerMsg) {
			t.Errorf("listener got %q, want %q", buf, dialerMsg)
		}
	}()

	// Listener sends, dialer receives
	go func() {
		defer recvWg.Done()
		buf := make([]byte, len(listenerMsg))
		if _, err := io.ReadFull(dialerStream, buf); err != nil {
			t.Errorf("dialer ReadFull failed: %v", err)
			return
		}
		if !bytes.Equal(buf, listenerMsg) {
			t.Errorf("dialer got %q, want %q", buf, listenerMsg)
		}
	}()

	// Send from both sides
	if _, err := dialerStream.Write(dialerMsg); err != nil {
		t.Fatalf("dialer Write failed: %v", err)
	}
	if _, err := listenerStream.Write(listenerMsg); err != nil {
		t.Fatalf("listener Write failed: %v", err)
	}

	recvWg.Wait()
	listenerConn.Close()
}

func TestConn_Stream_InitialValue(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	bus := testsignal.New()

	dialer, err := pipe.CreateDialer("dialer", bus)
	if err != nil {
		t.Fatalf("CreateDialer failed: %v", err)
	}

	listener, err := pipe.CreateListener("listener", bus)
	if err != nil {
		t.Fatalf("CreateListener failed: %v", err)
	}

	var (
		dialerConn   *pipe.Conn
		listenerConn *pipe.Conn
	)

	// Start listener. The accepted conn is handed back over a channel so the
	// main goroutine's read happens-after the listener goroutine's write
	// (avoids a data race on a shared variable that a bare time.Sleep cannot).
	listenerConnCh := make(chan *pipe.Conn, 1)
	go func() {
		conn, lerr := listener.Listen(ctx)
		if lerr != nil {
			t.Errorf("Listen failed: %v", lerr)
			close(listenerConnCh)
			return
		}
		listenerConnCh <- conn
	}()

	// Dial
	dialerConn, err = dialer.Dial(ctx, "listener")
	if err != nil {
		t.Fatalf("Dial failed: %v", err)
	}
	defer dialerConn.Close()

	// Wait for the listener to accept and hand back its connection.
	listenerConn = <-listenerConnCh
	if listenerConn == nil {
		t.Fatal("listener returned no connection")
	}

	initial := []byte("token:stream-1")
	payload := []byte("hello over stream")

	dialerStream, dialerInit, err := dialerConn.Stream(initial...)
	if err != nil {
		t.Fatalf("dialerConn.Stream(initial) failed: %v", err)
	}
	defer dialerStream.Close()

	if dialerInit != nil {
		t.Errorf("dialer init value = %q, want nil", dialerInit)
	}

	listenerStream, listenerInit, err := listenerConn.Stream()
	if err != nil {
		t.Fatalf("listenerConn.Stream() failed: %v", err)
	}
	defer listenerStream.Close()

	if !bytes.Equal(listenerInit, initial) {
		t.Errorf("listener initial value = %q, want %q", listenerInit, initial)
	}

	if _, err := dialerStream.Write(payload); err != nil {
		t.Fatalf("dialer stream Write failed: %v", err)
	}

	buf := make([]byte, len(payload))
	if _, err := io.ReadFull(listenerStream, buf); err != nil {
		t.Fatalf("listener stream ReadFull failed: %v", err)
	}

	if !bytes.Equal(buf, payload) {
		t.Errorf("payload mismatch: got %q, want %q", buf, payload)
	}

	listenerConn.Close()
}

func TestConn_Stream_InitialValueOptional(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	bus := testsignal.New()

	dialer, err := pipe.CreateDialer("dialer", bus)
	if err != nil {
		t.Fatalf("CreateDialer failed: %v", err)
	}

	listener, err := pipe.CreateListener("listener", bus)
	if err != nil {
		t.Fatalf("CreateListener failed: %v", err)
	}

	var (
		dialerConn   *pipe.Conn
		listenerConn *pipe.Conn
	)

	// Start listener. The accepted conn is handed back over a channel so the
	// main goroutine's read happens-after the listener goroutine's write
	// (avoids a data race on a shared variable that a bare time.Sleep cannot).
	listenerConnCh := make(chan *pipe.Conn, 1)
	go func() {
		conn, lerr := listener.Listen(ctx)
		if lerr != nil {
			t.Errorf("Listen failed: %v", lerr)
			close(listenerConnCh)
			return
		}
		listenerConnCh <- conn
	}()

	// Dial
	dialerConn, err = dialer.Dial(ctx, "listener")
	if err != nil {
		t.Fatalf("Dial failed: %v", err)
	}
	defer dialerConn.Close()

	// Wait for the listener to accept and hand back its connection.
	listenerConn = <-listenerConnCh
	if listenerConn == nil {
		t.Fatal("listener returned no connection")
	}

	dialerStream, dialerInit, err := dialerConn.Stream()
	if err != nil {
		t.Fatalf("dialerConn.Stream(nil) failed: %v", err)
	}
	defer dialerStream.Close()

	if dialerInit != nil {
		t.Errorf("dialer init value = %q, want nil", dialerInit)
	}

	listenerStream, listenerInit, err := listenerConn.Stream()
	if err != nil {
		t.Fatalf("listenerConn.Stream() failed: %v", err)
	}
	defer listenerStream.Close()

	if listenerInit != nil {
		t.Errorf("listener initial value = %q, want nil", listenerInit)
	}

	listenerConn.Close()
}

func TestConn_Stream_ListenerCannotSetInitialValue(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	bus := testsignal.New()

	dialer, err := pipe.CreateDialer("dialer", bus)
	if err != nil {
		t.Fatalf("CreateDialer failed: %v", err)
	}

	listener, err := pipe.CreateListener("listener", bus)
	if err != nil {
		t.Fatalf("CreateListener failed: %v", err)
	}

	var listenerConn *pipe.Conn

	// Start listener. The accepted conn is handed back over a channel so the
	// main goroutine's read happens-after the listener goroutine's write
	// (avoids a data race on a shared variable that a bare time.Sleep cannot).
	listenerConnCh := make(chan *pipe.Conn, 1)
	go func() {
		conn, lerr := listener.Listen(ctx)
		if lerr != nil {
			t.Errorf("Listen failed: %v", lerr)
			close(listenerConnCh)
			return
		}
		listenerConnCh <- conn
	}()

	conn, err := dialer.Dial(ctx, "listener")
	if err != nil {
		t.Fatalf("Dial failed: %v", err)
	}
	defer conn.Close()

	listenerConn = <-listenerConnCh
	if listenerConn == nil {
		t.Fatal("listener returned no connection")
	}

	if _, _, err := listenerConn.Stream([]byte("not-allowed")...); err != pipe.ErrInitialValueOnListener {
		t.Fatalf("listenerConn.Stream(non-empty) error = %v, want %v", err, pipe.ErrInitialValueOnListener)
	}

	listenerConn.Close()
}

func TestConn_NumStreams_BeforeStream(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	bus := testsignal.New()

	dialer, err := pipe.CreateDialer("dialer", bus)
	if err != nil {
		t.Fatalf("CreateDialer failed: %v", err)
	}

	listener, err := pipe.CreateListener("listener", bus)
	if err != nil {
		t.Fatalf("CreateListener failed: %v", err)
	}

	var listenerConn *pipe.Conn

	// Start listener. The accepted conn is handed back over a channel so the
	// main goroutine's read happens-after the listener goroutine's write
	// (avoids a data race on a shared variable that a bare time.Sleep cannot).
	listenerConnCh := make(chan *pipe.Conn, 1)
	go func() {
		conn, lerr := listener.Listen(ctx)
		if lerr != nil {
			t.Errorf("Listen failed: %v", lerr)
			close(listenerConnCh)
			return
		}
		listenerConnCh <- conn
	}()

	// Dial
	conn, err := dialer.Dial(ctx, "listener")
	if err != nil {
		t.Fatalf("Dial failed: %v", err)
	}
	defer conn.Close()

	// Wait for the listener to accept and hand back its connection.
	listenerConn = <-listenerConnCh

	// NumStreams should be 0 before any Stream() call
	if n := conn.NumStreams(); n != 0 {
		t.Errorf("dialerConn NumStreams before Stream() = %d, want 0", n)
	}

	if listenerConn != nil {
		if n := listenerConn.NumStreams(); n != 0 {
			t.Errorf("listenerConn NumStreams before Stream() = %d, want 0", n)
		}
		listenerConn.Close()
	}

}
