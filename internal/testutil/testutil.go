// Package testutil provides helpers shared by pipe's tests: goroutine leak
// detection and a fake detached DataChannel.
package testutil

import (
	"runtime"
	"strings"
	"testing"
	"time"
)

// CheckLeaks fails t if goroutines started during the test are still running
// when it ends. It retries for a short grace period so that ordinary shutdown
// races do not cause flakes.
//
// Call it as the first statement of a test so that its check runs after every
// later t.Cleanup, which is where resources are usually released:
//
//	testutil.CheckLeaks(t)
func CheckLeaks(t *testing.T) {
	t.Helper()
	before := goroutineNames()

	t.Cleanup(func() {
		deadline := time.Now().Add(3 * time.Second)
		var leaked []string
		for {
			leaked = leakedSince(before)
			if len(leaked) == 0 {
				return
			}
			if time.Now().After(deadline) {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Errorf("goroutines leaked:\n%s", strings.Join(leaked, "\n"))
	})
}

// leakedSince returns the goroutine descriptions that appeared after before was
// captured.
func leakedSince(before map[string]int) []string {
	var leaked []string
	for name, count := range goroutineNames() {
		if count > before[name] {
			leaked = append(leaked, name)
		}
	}
	return leaked
}

// goroutineNames counts running goroutines by their top user frame, ignoring
// runtime and testing internals.
func goroutineNames() map[string]int {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}

	out := make(map[string]int)
	for _, stack := range strings.Split(string(buf), "\n\n") {
		if name, ok := interestingFrame(stack); ok {
			out[name]++
		}
	}
	return out
}

// interestingFrame extracts the identifying frame of one goroutine stack.
func interestingFrame(stack string) (string, bool) {
	lines := strings.Split(stack, "\n")
	if len(lines) < 3 {
		return "", false
	}
	for i := 1; i < len(lines); i += 2 {
		fn := strings.TrimSpace(lines[i])
		switch {
		case fn == "",
			strings.HasPrefix(fn, "runtime."),
			strings.HasPrefix(fn, "testing."),
			strings.HasPrefix(fn, "os/signal."),
			strings.HasPrefix(fn, "internal/"):
			continue
		}
		return fn, true
	}
	return "", false
}
