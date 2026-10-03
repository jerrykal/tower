package proto

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNegotiate(t *testing.T) {
	cases := []struct {
		aMin, aMax, bMin, bMax, want int
		err                          bool
	}{
		{1, 1, 1, 1, 1, false},
		{1, 2, 1, 1, 1, false},
		{1, 3, 2, 5, 3, false},
		{0, 0, 1, 1, 0, true},
		{2, 2, 1, 1, 0, true},
	}
	for _, c := range cases {
		got, err := Negotiate(c.aMin, c.aMax, c.bMin, c.bMax)
		if (err != nil) != c.err || got != c.want {
			t.Errorf("Negotiate(%d-%d, %d-%d) = %d, %v", c.aMin, c.aMax, c.bMin, c.bMax, got, err)
		}
	}
	_, err := Negotiate(0, 0, 1, 1)
	if !strings.Contains(err.Error(), "0-0") || !strings.Contains(err.Error(), "1-1") {
		t.Errorf("the refusal names both ranges: %v", err)
	}
}

func TestNewer(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"0.0.2", "0.0.1", true},
		{"0.0.1", "0.0.2", false},
		{"0.1.0", "0.0.9", true},
		{"1.0.0", "0.9.9", true},
		{"0.0.1-dev+abc", "0.0.1", true},
		{"0.0.1", "0.0.1-dev+abc", false},
		{"0.0.1-dev+abc", "0.0.1-dev+def", false},
		{"0.0.1", "0.0.1", false},
		{"v0.0.2", "0.0.1", true},
	}
	for _, c := range cases {
		if got := Newer(c.a, c.b); got != c.want {
			t.Errorf("Newer(%q, %q) = %v", c.a, c.b, got)
		}
	}
}

func TestMsgRoundTripAndUnknown(t *testing.T) {
	m := Msg{T: TState, State: &State{Seq: 3, Inst: "1:2", Sessions: []Session{{ID: "$1", Name: "a\tb'\"#{pid}", Ago: 5}}}}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsRune(string(b), '\n') {
		t.Fatal("a message must encode to one line")
	}
	var back Msg
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.State.Sessions[0].Name != m.State.Sessions[0].Name {
		t.Fatalf("name changed: %q", back.State.Sessions[0].Name)
	}
	// A newer peer's message type and fields decode without error.
	var u Msg
	if err := json.Unmarshal([]byte(`{"t":"future","future":{"x":1},"hello":{"min":1,"max":2,"id":"x","extra":true}}`), &u); err != nil {
		t.Fatalf("unknown fields must be ignored: %v", err)
	}
	if u.T != "future" {
		t.Fatal(u.T)
	}
}

func TestRef(t *testing.T) {
	r := Ref{Host: "abcd", Name: "pc", Session: "$3", Label: "work", Window: "@2"}
	if r.String() != "pc:work:@2" {
		t.Fatal(r.String())
	}
	if (Ref{}).IsZero() != true || r.IsZero() {
		t.Fatal("IsZero")
	}
	if !r.SameSession(Ref{Host: "abcd", Session: "$3"}) || r.SameSession(Ref{Host: "abcd", Session: "$3", Inst: "9:9"}) {
		t.Fatal("SameSession compares host, session and instance")
	}
}

func TestParseClient(t *testing.T) {
	c, err := ParseClient("42:1700000000:/dev/ttys003")
	if err != nil || c != (ClientID{42, "1700000000", "/dev/ttys003"}) || c.String() != "42:1700000000:/dev/ttys003" {
		t.Fatalf("%+v %v", c, err)
	}
	if c, _ := ParseClient("1:2:client:with:colons"); c.Name != "client:with:colons" {
		t.Fatalf("name %q", c.Name)
	}
	for _, bad := range []string{"", "x:1:n", "0:1:n", "1:2", "1:2:"} {
		if _, err := ParseClient(bad); err == nil {
			t.Fatalf("%q parsed", bad)
		}
	}
}
