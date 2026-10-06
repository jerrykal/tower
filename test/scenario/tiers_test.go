package scenario

import (
	"flag"
	"strconv"
	"testing"
)

// The suite runs in two tiers. A scenario that measures or bounds a time
// (LC, LH, LV, LD, LS08, U08 and a few more) runs alone: the serial
// tier, which go test runs first. Every other scenario calls parallel
// and runs alongside others once the serial tier is done, at most
// parallelWorlds at a time.

// parallelWorlds is -parallel's number: TOWER_PARALLEL (default 4) unless
// the command line gives it.
var parallelWorlds = 1

// parallel puts the test in the parallel tier. It comes first in the
// test, before its world.
func parallel(t *testing.T) {
	t.Helper()
	t.Parallel()
}

// setParallel sets parallelWorlds, and -test.parallel to it.
func setParallel() {
	flag.Parse()
	given := false
	flag.Visit(func(f *flag.Flag) { given = given || f.Name == "test.parallel" })
	if !given {
		n := envInt("TOWER_PARALLEL")
		if n <= 0 {
			n = 4
		}
		flag.Set("test.parallel", strconv.Itoa(n))
	}
	n, _ := strconv.Atoi(flag.Lookup("test.parallel").Value.String())
	parallelWorlds = max(n, 1)
}
