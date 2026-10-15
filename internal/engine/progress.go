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
)

// Progress aggregates counters across all concurrently-downloading streams.
// Speed is a sliding 5-second window, not a lifetime average.
type Progress struct {
	total      atomic.Int64
	done       atomic.Int64
	bytes      atomic.Int64
	knownBytes atomic.Int64

	mu       sync.Mutex
	samples  []sample // ring of (time, bytes) for windowed speed
	live     bool
	start    time.Time
	interval time.Duration // render tick override; 0 = default (1s TTY, 5s piped)

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

// Line renders one status line. The two shapes below (VOD: "pct% | done/total
// segs | bytes | speed/s | ETA x"; live: "REC elapsed | segs | bytes | speed/s")
// are a stable, machine-parseable contract — external tools parse the piped
// stderr output — so keep the field order and separators stable; "ETA ?" is the
// deliberate unknown-ETA sentinel.
func (p *Progress) Line() string {
	done, total := p.done.Load(), p.total.Load()
	bytes := p.bytes.Load()
	speed := p.Speed()
	elapsed := time.Since(p.start).Round(time.Second)
	if p.live {
		return fmt.Sprintf("REC %s | %d segs | %s | %s/s", elapsed, done, fmtBytes(bytes), fmtBytes(int64(speed)))
	}
	pct := 0.0
	if total > 0 {
		pct = float64(done) / float64(total) * 100
	}
	eta := "?"
	if speed > 1 && total > done && done > 8 {
		perSeg := float64(bytes) / float64(done)
		etaSec := perSeg * float64(total-done) / speed
		if etaSec < 360000 {
			eta = (time.Duration(etaSec) * time.Second).Round(time.Second).String()
		}
	}
	return fmt.Sprintf("%5.1f%% | %d/%d segs | %s | %s/s | ETA %s", pct, done, total, fmtBytes(bytes), fmtBytes(int64(speed)), eta)
}

// Render prints progress to stderr until stop is closed. On a TTY it rewrites
// one line; otherwise it prints a plain line each interval (machine-parseable,
// survives redirection — no ANSI garbage). Default cadence is 1s on a TTY and
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
	p.lw = &lineWriter
	p.outMu.Unlock()
	for {
		select {
		case <-stop:
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

// Progress lines are ASCII. Pad to erase the previous line without ANSI/VT
// sequences, which classic Windows consoles don't interpret by default.
// The same width limit applies to ticks and the final line; leave one spare
// column to avoid terminal auto-wrap (including at an exact-width boundary).
type progressLineWriter struct {
	out      io.Writer
	tty      bool
	width    func() int
	previous int
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
		if len(line) > limit {
			line = line[:limit]
		}
	}
	clearWidth := max(w.previous, len(line))
	if columns > 0 && clearWidth > limit {
		clearWidth = limit
	}
	padding := strings.Repeat(" ", max(0, clearWidth-len(line)))
	suffix := ""
	if final {
		suffix = "\n"
	}
	fmt.Fprintf(w.out, "\r%s%s%s", line, padding, suffix)
	w.previous = len(line)
}
