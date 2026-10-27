package engine

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/term"

	"github.com/m314dev/m314dl/internal/ui"
)

// Progress aggregates counters across all concurrently-downloading streams.
// Speed is a sliding 5-second window, not a lifetime average.
type Progress struct {
	total      atomic.Int64
	done       atomic.Int64
	bytes      atomic.Int64
	knownBytes atomic.Int64
	conns      atomic.Int64 // segment fetches in flight right now

	mu       sync.Mutex
	samples  []sample // ring of (time, bytes) for windowed speed
	live     bool
	start    time.Time
	end      time.Time     // when Render stopped; zero while it runs
	interval time.Duration // render tick override; 0 = default (1s TTY, 5s piped)
	name     string        // output file shown at the end of the terminal line
	tty      bool          // set by Render: draw the bar shape, not the piped one

	outMu sync.Mutex          // serializes status-line draws and Log
	lw    *progressLineWriter // set while Render runs
}

type sample struct {
	t time.Time
	b int64
}

// NewProgress creates a progress aggregator. interval overrides the render
// cadence for both the TTY and piped branches; 0 keeps the defaults.
func NewProgress(live bool, interval time.Duration) *Progress {
	return &Progress{live: live, start: time.Now(), interval: interval}
}

func (p *Progress) AddTotal(n int64)      { p.total.Add(n) }
func (p *Progress) AddDone(n int64)       { p.done.Add(n) }
func (p *Progress) SetKnownBytes(n int64) { p.knownBytes.Store(n) }

// AddConns adjusts the in-flight fetch count shown on the terminal line.
func (p *Progress) AddConns(n int64) { p.conns.Add(n) }

// SetName sets the file name the terminal line ends with. Leave it empty when
// one Progress is shared by several downloads.
func (p *Progress) SetName(name string) {
	p.mu.Lock()
	p.name = name
	p.mu.Unlock()
}

func (p *Progress) AddBytes(n int64) {
	cur := p.bytes.Add(n)
	now := time.Now()
	p.mu.Lock()
	// At most one sample per 100ms. A sample per Read let a few dozen busy
	// workers fill the ring within milliseconds, so the window Speed measures
	// shrank under its 0.1s floor and a fast download read "0B/s | ETA ?".
	if k := len(p.samples); k == 0 || now.Sub(p.samples[k-1].t) >= 100*time.Millisecond {
		p.samples = append(p.samples, sample{now, cur})
		if len(p.samples) > 256 {
			p.samples = p.samples[128:]
		}
	}
	p.mu.Unlock()
}

// Speed returns bytes/sec over the last 5 seconds.
func (p *Progress) Speed() float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	cur := p.bytes.Load()
	for _, s := range p.samples {
		if now.Sub(s.t) <= 5*time.Second {
			dt := now.Sub(s.t).Seconds()
			if dt < 0.1 {
				return 0
			}
			return float64(cur-s.b) / dt
		}
	}
	return 0
}

func fmtBytes(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.2fGiB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1fMiB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.0fKiB", float64(b)/(1<<10))
	}
	return fmt.Sprintf("%dB", b)
}

// Line renders one status line.
//
// Piped (not a terminal), the two shapes below (VOD: "pct% | done/total segs |
// bytes | speed/s | ETA x"; live: "REC elapsed | segs | bytes | speed/s") are a
// stable, machine-parseable contract. External tools and the -rpc server parse
// them, so keep the field order and separators stable; "ETA ?" is the deliberate
// unknown-ETA sentinel.
//
// On a terminal the line is for a person instead: a bar, the size so far with
// the estimated total, the fetches in flight, and the file name, in color when
// ui.Color allows. A terminal line is redrawn in place and never parsed.
func (p *Progress) Line() string {
	done, total := p.done.Load(), p.total.Load()
	bytes := p.bytes.Load()
	speed := p.Speed()
	elapsed := time.Since(p.start).Round(time.Second)
	pct, eta, estTotal := p.estimate(done, total, bytes, speed)
	if !p.tty {
		if p.live {
			return fmt.Sprintf("REC %s | %d segs | %s | %s/s", elapsed, done, fmtBytes(bytes), fmtBytes(int64(speed)))
		}
		return fmt.Sprintf("%5.1f%% | %d/%d segs | %s | %s/s | ETA %s", pct, done, total, fmtBytes(bytes), fmtBytes(int64(speed)), eta)
	}
	tail := ""
	if n := p.conns.Load(); n > 0 {
		word := "conns"
		if n == 1 {
			word = "conn"
		}
		tail += " | " + ui.Paint(ui.Dim, fmt.Sprintf("%d %s", n, word))
	}
	p.mu.Lock()
	name := p.name
	p.mu.Unlock()
	if name != "" {
		tail += " | " + name
	}
	rate := ui.Paint(ui.Green, fmtBytes(int64(speed))+"/s")
	if p.live {
		return fmt.Sprintf("%s %s | %d segs | %s | %s%s", ui.Paint(ui.Red, "REC"), elapsed, done, fmtBytes(bytes), rate, tail)
	}
	size := fmtBytes(bytes)
	if total > 0 && done >= total {
		eta = "0s" // the piped shape keeps its "?" sentinel; a person wants to see it finished
	} else if estTotal > 0 {
		size += "/~" + fmtBytes(estTotal)
	}
	return fmt.Sprintf("%s %s | %d/%d segs | %s | %s | %s%s",
		ui.Paint(ui.Cyan, bar(pct, 20)), ui.Paint(ui.Cyan, fmt.Sprintf("%5.1f%%", pct)),
		done, total, size, rate, ui.Paint(ui.Yellow, "ETA "+eta), tail)
}

// estimate derives the percentage, the ETA string ("?" when unknown) and the
// projected total size (0 when unknown) from the counters. Both projections
// assume the segments so far are typical, so they wait for a handful of them.
func (p *Progress) estimate(done, total, bytes int64, speed float64) (pct float64, eta string, estTotal int64) {
	if total > 0 {
		pct = float64(done) / float64(total) * 100
	}
	eta = "?"
	if known := p.knownBytes.Load(); known > 0 {
		estTotal = known
	} else if total > 0 && enoughDone(done, total) {
		estTotal = int64(float64(bytes) / float64(done) * float64(total))
	}
	if speed > 1 && total > done && enoughDone(done, total) {
		perSeg := float64(bytes) / float64(done)
		etaSec := perSeg * float64(total-done) / speed
		if etaSec < 360000 {
			eta = (time.Duration(etaSec) * time.Second).Round(time.Second).String()
		}
	}
	return pct, eta, estTotal
}

// enoughDone says whether enough segments have finished for a projection to
// mean something: nine of them, or a quarter of a short playlist, so a 7-segment
// download still shows an ETA instead of "?" until it is over.
func enoughDone(done, total int64) bool {
	return done > 8 || (done > 0 && done*4 >= total)
}

// bar draws "[████░░░░]" with width cells, filled in proportion to pct.
func bar(pct float64, width int) string {
	filled := int(float64(width) * pct / 100)
	if filled > width {
		filled = width
	}
	return "[" + strings.Repeat("█", filled) + strings.Repeat("░", width-filled) + "]"
}

// Summary describes the finished download for the final "done" line: the
// output size (outSize, or the bytes transferred when outSize is negative),
// how long the download phase took, and its average speed.
func (p *Progress) Summary(outSize int64) string {
	p.mu.Lock()
	end := p.end
	p.mu.Unlock()
	if end.IsZero() {
		end = time.Now()
	}
	if outSize < 0 {
		outSize = p.bytes.Load()
	}
	elapsed := end.Sub(p.start)
	avg := int64(0)
	if secs := elapsed.Seconds(); secs > 0 {
		avg = int64(float64(p.bytes.Load()) / secs)
	}
	return fmt.Sprintf("%s, %s at %s/s", fmtBytes(outSize), elapsed.Round(time.Second), fmtBytes(avg))
}

// Render prints progress to stderr until stop is closed. On a TTY it rewrites
// one line; otherwise it prints a plain line each interval (machine-parseable,
// survives redirection, no ANSI garbage). Default cadence is 1s on a TTY and
// 5s when piped, overridable via NewProgress's interval.
func (p *Progress) Render(stop <-chan struct{}) {
	isTTY := term.IsTerminal(int(os.Stderr.Fd()))
	interval := time.Second
	if !isTTY {
		interval = 5 * time.Second
	}
	if p.interval > 0 {
		interval = p.interval
		if interval < 50*time.Millisecond {
			interval = 50 * time.Millisecond // floor: don't flood the consumer
		}
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	lineWriter := progressLineWriter{out: os.Stderr, tty: isTTY, width: func() int {
		w, _, err := term.GetSize(int(os.Stderr.Fd()))
		if err != nil {
			return 0
		}
		return w
	}}
	p.outMu.Lock()
	p.tty = isTTY
	p.lw = &lineWriter
	p.outMu.Unlock()
	for {
		select {
		case <-stop:
			p.mu.Lock()
			p.end = time.Now()
			p.mu.Unlock()
			p.outMu.Lock()
			lineWriter.write(p.Line(), true)
			p.lw = nil
			p.outMu.Unlock()
			return
		case <-tick.C:
			p.outMu.Lock()
			lineWriter.write(p.Line(), false)
			p.outMu.Unlock()
		}
	}
}

// Log prints a message on its own line. On a TTY the status line is erased
// first and redrawn at the next tick, so messages from downloads sharing this
// Progress never land in the middle of it.
func (p *Progress) Log(line string) {
	p.outMu.Lock()
	defer p.outMu.Unlock()
	out := io.Writer(os.Stderr)
	if w := p.lw; w != nil {
		out = w.out
		if w.tty && w.previous > 0 {
			fmt.Fprintf(out, "\r%s\r", strings.Repeat(" ", w.previous))
			w.previous = 0
		}
	}
	fmt.Fprintln(out, line)
}

// progressLineWriter redraws one line in place. It pads with spaces to erase
// the previous draw instead of using a clear-line escape, which classic Windows
// consoles don't interpret by default. Widths are terminal columns (ui.Width),
// so color codes and the bar glyphs in the line don't count against the
// terminal width. The same width limit applies to ticks and the final line;
// leave one spare column to avoid terminal auto-wrap (including at an
// exact-width boundary).
type progressLineWriter struct {
	out      io.Writer
	tty      bool
	width    func() int
	previous int // columns drawn last time
}

func (w *progressLineWriter) write(line string, final bool) {
	if !w.tty {
		fmt.Fprintln(w.out, line)
		return
	}
	columns := 0
	if w.width != nil {
		columns = w.width()
	}
	limit := max(0, columns-1)
	if columns > 0 {
		line = ui.Truncate(line, limit)
	}
	cells := ui.Width(line)
	clearWidth := max(w.previous, cells)
	if columns > 0 && clearWidth > limit {
		clearWidth = limit
	}
	padding := strings.Repeat(" ", max(0, clearWidth-cells))
	suffix := ""
	if final {
		suffix = "\n"
	}
	fmt.Fprintf(w.out, "\r%s%s%s", line, padding, suffix)
	w.previous = cells
}
