package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/m314dev/m314dl/internal/manifest"
)

func TestSidecarSubPath(t *testing.T) {
	used := map[string]bool{}
	en := &manifest.Stream{Language: "en", ID: "s1"}
	got := sidecarSubPath("out.mkv", en, "srt", used)
	if got != "out.en.srt" {
		t.Fatalf("got %q, want out.en.srt", got)
	}
	used[got] = true
	// a second English track must not get the same name as the first
	en2 := &manifest.Stream{Language: "en", ID: "s2", Name: "forced"}
	got2 := sidecarSubPath("out.mkv", en2, "srt", used)
	if got2 == got {
		t.Fatalf("second en track collided: %q", got2)
	}
	if got2 != "out.en.forced.srt" {
		t.Fatalf("got %q, want out.en.forced.srt", got2)
	}
	// a track with no language falls back to "sub"
	if p := sidecarSubPath("v.mp4", &manifest.Stream{ID: "x"}, "vtt", map[string]bool{}); p != "v.sub.vtt" {
		t.Fatalf("got %q, want v.sub.vtt", p)
	}
}

// Temp files from before the per-download folder existed are moved into it
// and renamed to today's names, so an interrupted download still resumes
// after an upgrade.
func TestAdoptOldTempFiles(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "movie.mp4")
	for _, name := range []string{
		"movie.m314dl-video-1.mp4", "movie.m314dl-video-1.mp4.part7", "movie.m314dl-video-1.mp4.m314dl-state",
		"movie2.m314dl-video-1.mp4", // belongs to another download: left alone
	} {
		os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644)
	}
	tmp := filepath.Join(dir, "movie.m314dl")
	adoptOldTempFiles(out, tmp)
	for _, want := range []string{"video-1.mp4", "video-1.mp4.part7", "video-1.mp4.m314dl-state"} {
		if _, err := os.Stat(filepath.Join(tmp, want)); err != nil {
			t.Errorf("%s not adopted: %v", want, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "movie2.m314dl-video-1.mp4")); err != nil {
		t.Error("another download's temp file was moved")
	}
	// nothing to adopt: the folder is not created here (the caller does that)
	adoptOldTempFiles(filepath.Join(dir, "other.mp4"), filepath.Join(dir, "other.m314dl"))
	if _, err := os.Stat(filepath.Join(dir, "other.m314dl")); err == nil {
		t.Error("folder created with nothing to adopt")
	}
}
