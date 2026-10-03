package config

import (
	"os"
	"strconv"
	"time"
)

// Duration reads a timing knob: a Go duration ("1.5s") or integer
// milliseconds; unset or invalid gives def. Tests shorten tower's timings
// through these.
func Duration(name string, def time.Duration) time.Duration {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	if d, err := time.ParseDuration(v); err == nil && d >= 0 {
		return d
	}
	if ms, err := strconv.Atoi(v); err == nil && ms >= 0 {
		return time.Duration(ms) * time.Millisecond
	}
	return def
}

// Flag reads an on/off knob: "0", "false", "no" and "off" are off, unset
// gives def, anything else is on.
func Flag(name string, def bool) bool {
	switch v := os.Getenv(name); v {
	case "":
		return def
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}
