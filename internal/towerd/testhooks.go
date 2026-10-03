package towerd

import (
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/jerrykal/tower/internal/proto"
)

// Test hooks of the scenario suite, read from the environment. None of
// them changes anything when unset.

// protoRange is the protocol range this towerd speaks: proto.Min–Max, or
// TOWER_TEST_PROTO ("lo-hi") so the suite can run old and new peers.
func protoRange() (int, int) {
	if v := os.Getenv("TOWER_TEST_PROTO"); v != "" {
		lo, hi, ok := strings.Cut(v, "-")
		a, err1 := strconv.Atoi(lo)
		b, err2 := strconv.Atoi(hi)
		if ok && err1 == nil && err2 == nil && a <= b {
			return a, b
		}
	}
	return proto.Min, proto.Max
}

// testFuture: a remote also sends a message type no build knows.
func testFuture() bool { return os.Getenv("TOWER_TEST_FUTURE") != "" }

var (
	padOnce sync.Once
	pad     string
)

// testPad pads states and views to TOWER_TEST_PAD bytes, to stream large
// messages without making thousands of windows.
func testPad() string {
	padOnce.Do(func() {
		if n, err := strconv.Atoi(os.Getenv("TOWER_TEST_PAD")); err == nil && n > 0 {
			pad = strings.Repeat("x", n)
		}
	})
	return pad
}
