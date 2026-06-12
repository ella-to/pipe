package pipe_test

import (
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	"ella.to/pipe"
	"ella.to/pipe/signal/testsignal"
)

// setupBenchPair establishes a connected dialer/listener pair over an
// in-memory signaling bus and real local WebRTC transport.
func setupBenchPair(b *testing.B) (dialerConn, listenerConn *pipe.Conn) {
	b.Helper()

	sig := testsignal.New()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	listener, err := pipe.CreateListener("bench-listener", sig)
	if err != nil {
		b.Fatalf("create listener: %v", err)
	}

	type listenResult struct {
		conn *pipe.Conn
		err  error
	}
	listenCh := make(chan listenResult, 1)
	go func() {
		c, err := listener.Listen(ctx)
		if err == nil {
			err = c.WaitReady(ctx)
		}
		listenCh <- listenResult{conn: c, err: err}
	}()

	dialer, err := pipe.CreateDialer("bench-dialer", sig)
	if err != nil {
		b.Fatalf("create dialer: %v", err)
	}

	dconn, err := dialer.Dial(ctx, "bench-listener")
	if err != nil {
		b.Fatalf("dial: %v", err)
	}

	res := <-listenCh
	if res.err != nil {
		b.Fatalf("listen: %v", res.err)
	}

	b.Cleanup(func() {
		_ = dconn.Close()
		_ = res.conn.Close()
	})

	return dconn, res.conn
}

func benchmarkThroughput(b *testing.B, w io.Writer, r io.Reader, size int) {
	payload := make([]byte, size)
	total := int64(size) * int64(b.N)

	b.SetBytes(int64(size))
	b.ResetTimer()

	errCh := make(chan error, 1)
	go func() {
		_, err := io.CopyN(io.Discard, r, total)
		errCh <- err
	}()

	for i := 0; i < b.N; i++ {
		if _, err := w.Write(payload); err != nil {
			b.Fatalf("write: %v", err)
		}
	}

	if err := <-errCh; err != nil {
		b.Fatalf("read: %v", err)
	}
}

// BenchmarkConnThroughput measures one-way raw Conn throughput for a range
// of write sizes.
func BenchmarkConnThroughput(b *testing.B) {
	for _, size := range []int{4 * 1024, 64 * 1024, 1024 * 1024} {
		b.Run(fmt.Sprintf("%dKB", size/1024), func(b *testing.B) {
			dconn, lconn := setupBenchPair(b)
			benchmarkThroughput(b, dconn, lconn, size)
		})
	}
}

// BenchmarkConnRoundTrip measures request/response latency over the raw Conn.
func BenchmarkConnRoundTrip(b *testing.B) {
	dconn, lconn := setupBenchPair(b)

	go func() {
		_, _ = io.Copy(lconn, lconn)
	}()

	payload := make([]byte, 1024)
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if _, err := dconn.Write(payload); err != nil {
			b.Fatalf("write: %v", err)
		}
		if _, err := io.ReadFull(dconn, payload); err != nil {
			b.Fatalf("read: %v", err)
		}
	}
}

// BenchmarkStreamThroughput measures one-way throughput over a multiplexed
// stream (smux on top of the data channel).
func BenchmarkStreamThroughput(b *testing.B) {
	for _, size := range []int{4 * 1024, 64 * 1024, 1024 * 1024} {
		b.Run(fmt.Sprintf("%dKB", size/1024), func(b *testing.B) {
			dconn, lconn := setupBenchPair(b)

			type acceptResult struct {
				stream io.ReadWriteCloser
				err    error
			}
			acceptCh := make(chan acceptResult, 1)
			go func() {
				s, _, err := lconn.Stream()
				acceptCh <- acceptResult{stream: s, err: err}
			}()

			ws, _, err := dconn.Stream()
			if err != nil {
				b.Fatalf("open stream: %v", err)
			}
			res := <-acceptCh
			if res.err != nil {
				b.Fatalf("accept stream: %v", res.err)
			}

			benchmarkThroughput(b, ws, res.stream, size)
		})
	}
}

// BenchmarkStreamOpen measures the cost of opening and accepting a
// multiplexed stream.
func BenchmarkStreamOpen(b *testing.B) {
	dconn, lconn := setupBenchPair(b)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			s, _, err := lconn.Stream()
			if err != nil {
				return
			}
			_ = s.Close()
		}
	}()

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		s, _, err := dconn.Stream()
		if err != nil {
			b.Fatalf("open stream: %v", err)
		}
		_ = s.Close()
	}

	b.StopTimer()
	_ = dconn.Close()
	_ = lconn.Close()
	<-done
}
