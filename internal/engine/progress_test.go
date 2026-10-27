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

// On a terminal the line has a bar, the projected size, the connection count
// and the file name. Piped, it keeps the parseable shape unchanged.
func TestProgressTerminalLine(t *testing.T) {
	p := NewProgress(false, 0)
	p.AddTotal(20)
	p.AddDone(10)
	p.AddBytes(10 << 20)
	p.AddConns(4)
	p.SetName("movie.mp4")
	if got := p.Line(); !strings.HasPrefix(got, " 50.0% | 10/20 segs | 10.0MiB | ") || strings.Contains(got, "movie.mp4") {
		t.Fatalf("piped line changed: %q", got)
	}
	p.tty = true
	got := p.Line()
	for _, want := range []string{"[██████████░░░░░░░░░░]", " 50.0% | 10/20 segs | 10.0MiB/~20.0MiB | ", "ETA ?", "4 conns | movie.mp4"} {
		if !strings.Contains(got, want) {
			t.Fatalf("terminal line %q lacks %q", got, want)
		}
	}
	if strings.Contains(got, "\x1b") {
		t.Fatal("colors emitted with ui.Color off")
	}
	// the final line at 100% has a full bar, no projection and no "ETA ?"
	p.AddDone(10)
	if got := p.Line(); !strings.Contains(got, "[████████████████████] 100.0% | 20/20 segs | 10.0MiB | ") || !strings.Contains(got, "ETA 0s") {
		t.Fatalf("final line %q", got)
	}
	if s := p.Summary(-1); s != "10.0MiB, 0s at 0B/s" && !strings.HasPrefix(s, "10.0MiB, ") {
		t.Fatalf("summary %q", s)
	}
}

// A short playlist shows an ETA after a quarter of it is done; a long one
// waits for nine segments, because the first few are a poor sample.
func TestEnoughDone(t *testing.T) {
	for _, c := range []struct {
		done, total int64
		want        bool
	}{{0, 7, false}, {1, 7, false}, {2, 7, true}, {5, 100, false}, {9, 100, true}} {
		if got := enoughDone(c.done, c.total); got != c.want {
			t.Errorf("enoughDone(%d, %d) = %v, want %v", c.done, c.total, got, c.want)
		}
	}
}

// The redraw pads and truncates by terminal columns, so a colored line with
// bar glyphs is still erased cleanly and still fits the terminal.
func TestProgressLineWriterCountsColumnsNotBytes(t *testing.T) {
	var b bytes.Buffer
	w := progressLineWriter{out: &b, tty: true, width: func() int { return 8 }}
	w.write("\x1b[36m[██░░]\x1b[0m 50%", false) // 10 columns, cut to 7
	if got, want := b.String(), "\r\x1b[36m[██░░]\x1b[0m "; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if w.previous != 7 {
		t.Fatalf("previous = %d columns, want 7", w.previous)
	}
}
