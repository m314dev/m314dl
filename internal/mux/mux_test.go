package mux

import "testing"

func TestFindFFmpegExplicit(t *testing.T) {
	// An explicit bare name that exists on PATH resolves. "sh" is always there.
	if p, err := FindFFmpeg("sh"); err != nil || p == "" {
		t.Fatalf("explicit name on PATH should resolve, got %q err=%v", p, err)
	}
	// A bad explicit path returns an error instead of falling back to the other
	// lookups.
	if _, err := FindFFmpeg("/no/such/ffmpeg-xyz"); err == nil {
		t.Fatal("bogus explicit ffmpeg path should error")
	}
}
