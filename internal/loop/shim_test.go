package loop

import (
	"bytes"
	"slices"
	"testing"
)

func TestAttachCommand(t *testing.T) {
	got := AttachCommand("$3", "@7", "123:456")
	want := []string{"attach-session", "-t", "$3", ";", "if-shell", "-F", "#{!=:#{pid}:#{start_time},123:456}", "detach-client -E 'exit 43'", ";", "select-window", "-t", "@7"}
	if !slices.Equal(got, want) {
		t.Fatalf("%q", got)
	}
	if got := AttachCommand("$3", "", ""); !slices.Equal(got, []string{"attach-session", "-t", "$3"}) {
		t.Fatalf("%q", got)
	}
}

func TestReadLineTakesOneLine(t *testing.T) {
	r := bytes.NewBufferString("\r\n{\"s\":\"$1\"}\nkeys typed after")
	line, err := readLine(r)
	if err != nil || string(line) != `{"s":"$1"}` {
		t.Fatalf("%q %v", line, err)
	}
	if r.String() != "keys typed after" {
		t.Fatalf("read past the line: %q left", r.String())
	}
}
