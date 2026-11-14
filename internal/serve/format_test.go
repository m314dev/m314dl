package serve

import (
	"testing"

	"github.com/m314dev/m314dl/internal/source"
)

// A raw live TS source can only go out as TS, so leaving the format unset
// picks ts for it instead of the usual hls. Other kinds keep what they were
// given, unset included.
func TestCheckFormat(t *testing.T) {
	for _, c := range []struct {
		kind, in, want string
		fails          bool
	}{
		{source.LiveTSKind, "", "ts", false},
		{source.LiveTSKind, "mpegts", "mpegts", false},
		{source.LiveTSKind, "hls", "", true},
		{source.LiveTSKind, "dash", "", true},
		{"hls", "", "", false},
		{"dash", "dash", "dash", false},
	} {
		got, err := CheckFormat(c.kind, c.in)
		if (err != nil) != c.fails || got != c.want {
			t.Errorf("CheckFormat(%q, %q) = %q, %v; want %q, fails=%v", c.kind, c.in, got, err, c.want, c.fails)
		}
	}
}
