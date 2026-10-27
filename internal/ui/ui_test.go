package ui

import "testing"

func TestPaintOffIsIdentity(t *testing.T) {
	Color = false
	if got := Paint(Red, "error"); got != "error" {
		t.Fatalf("Paint with Color off = %q", got)
	}
	Color = true
	defer func() { Color = false }()
	if got := Paint(Red, "error"); got != "\x1b[31merror\x1b[0m" {
		t.Fatalf("Paint = %q", got)
	}
	if got := Paint(Red, ""); got != "" {
		t.Fatalf("Paint of empty string = %q", got)
	}
}

// Width ignores escape codes and counts the bar glyphs as one column each.
func TestWidthAndTruncate(t *testing.T) {
	line := "\x1b[36m[██░░]\x1b[0m 50.0% | \x1b[32m1MiB/s\x1b[0m"
	if got := Width(line); got != 21 {
		t.Fatalf("Width = %d, want 21", got)
	}
	if got := Truncate(line, 30); got != line {
		t.Fatalf("Truncate wider than line changed it: %q", got)
	}
	// The cut lands inside the green span, which must be closed.
	got := Truncate(line, 17)
	if want := "\x1b[36m[██░░]\x1b[0m 50.0% | \x1b[32m1M\x1b[0m"; got != want {
		t.Fatalf("Truncate = %q, want %q", got, want)
	}
	if Width(got) != 17 {
		t.Fatalf("truncated width = %d", Width(got))
	}
	// Plain ASCII behaves exactly like a byte slice.
	if got := Truncate("abcdef", 3); got != "abc" {
		t.Fatalf("ASCII Truncate = %q", got)
	}
}
