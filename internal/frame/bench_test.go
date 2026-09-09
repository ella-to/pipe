package frame

import (
	"io"
	"testing"
)

// BenchmarkStreamTransfer moves data through a stream pair over the in-memory
// channel and reports allocations, so that changes to the read path can be
// judged by numbers rather than by intuition.
func BenchmarkStreamTransfer(b *testing.B) {
	for _, tc := range []struct {
		name    string
		payload int
		write   int
	}{
		{"16KiB-frames/64KiB-writes", 16 << 10, 64 << 10},
		{"16KiB-frames/1MiB-writes", 16 << 10, 1 << 20},
		{"64KiB-frames/1MiB-writes", 64 << 10, 1 << 20},
	} {
		b.Run(tc.name, func(b *testing.B) {
			a, z := newFakePair(64)
			ws := NewStream(a, Config{MaxPayload: tc.payload})
			rs := NewStream(z, Config{MaxPayload: tc.payload})
			defer ws.Close()
			defer rs.Close()

			done := make(chan struct{})
			go func() {
				defer close(done)
				_, _ = io.Copy(io.Discard, rs)
			}()

			buf := make([]byte, tc.write)
			b.SetBytes(int64(tc.write))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := ws.Write(buf); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			_ = ws.Close()
			<-done
		})
	}
}
