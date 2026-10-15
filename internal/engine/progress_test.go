package engine

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestProgressLineClearsShorterFinalLine(t *testing.T) {
	var b bytes.Buffer
	w := progressLineWriter{out: &b, tty: true, width: func() int { return 80 }}
	w.write("ETA 32m44s", false)
	w.write("ETA ?", true)
	if got, want := b.String(), "\rETA 32m44s\rETA ?     \n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if strings.Contains(b.String(), "\x1b") {
		t.Fatal("ANSI sequence emitted")
	}
}

func TestProgressLineWidthAndResize(t *testing.T) {
	for _, final := range []bool{false, true} {
		for _, columns := range []int{1, 2, 4, 10} {
			var b bytes.Buffer
			w := progressLineWriter{out: &b, tty: true, width: func() int { return columns }}
			w.write(strings.Repeat("a", columns), final) // exactly terminal width
			want := "\r" + strings.Repeat("a", columns-1)
			if final {
				want += "\n"
			}
			if b.String() != want {
				t.Fatalf("columns=%d final=%v: %q, want %q", columns, final, b.String(), want)
			}
		}
	}
	var b bytes.Buffer
	columns := 80
	w := progressLineWriter{out: &b, tty: true, width: func() int { return columns }}
	w.write("a long status line", false)
	b.Reset()
	columns = 5
	w.write("ok", true)
	if b.String() != "\rok  \n" {
		t.Fatalf("resize padding: %q", b.String())
	}
}

func TestProgressPipedOutputUnchanged(t *testing.T) {
	var b bytes.Buffer
	w := progressLineWriter{out: &b, width: func() int { return 1 }}
	w.write("100.0% | 2/2 segs | 12B | 0B/s | ETA ?", true)
	if b.String() != "100.0% | 2/2 segs | 12B | 0B/s | ETA ?\n" {
		t.Fatalf("piped output: %q", b.String())
	}
}

// A burst of reads (dozens of workers on a fast link) must not collapse the
// speed window to zero.
func TestProgressSpeedSurvivesReadBurst(t *testing.T) {
	p := NewProgress(false, 0)
	p.AddBytes(1 << 20)
	time.Sleep(150 * time.Millisecond)
	for i := 0; i < 1000; i++ {
		p.AddBytes(32 << 10)
	}
	if p.Speed() <= 0 {
		t.Fatal("speed read 0 during a burst")
	}
}

// Log erases the status line before its message, so the two never share a line.
func TestProgressLogClearsStatusLine(t *testing.T) {
	var b bytes.Buffer
	p := NewProgress(false, 0)
	p.lw = &progressLineWriter{out: &b, tty: true, width: func() int { return 80 }}
	p.lw.write("  5.0% | 1/20 segs", false)
	p.Log("done: a.mkv")
	if got, want := b.String(), "\r  5.0% | 1/20 segs\r                  \rdone: a.mkv\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
