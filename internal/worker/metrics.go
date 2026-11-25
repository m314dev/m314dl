package worker

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/m314dev/m314dl/internal/restream"
	"github.com/m314dev/m314dl/internal/source"
)

// The numbers a controller reads off a channel. Totals come from the parts
// that already count (the HTTP client, the packager, the response writer);
// rates come from sampling those totals once a second, so the API stays a
// plain read with no clock arithmetic in the hot path.

// sampleEvery is how often the worker snapshots each channel's totals, and
// historySamples how many it keeps: a minute of history at one a second.
const (
	sampleEvery    = time.Second
	historySamples = 61
	rateWindow     = 10 * time.Second // bytes per second are averaged over this
	errorWindow    = time.Minute      // fetch errors are counted over this
	viewerWindow   = 30 * time.Second // a player that fetched within this is a viewer
	logLines       = 500              // lines a channel's log keeps
)

type sample struct {
	at           time.Time
	in, out, err int64
}

// meter keeps a channel's recent samples and derives rates from them.
type meter struct {
	mu sync.Mutex
	s  []sample // oldest first
}

func (m *meter) add(s sample) {
	m.mu.Lock()
	m.s = append(m.s, s)
	if len(m.s) > historySamples {
		m.s = m.s[len(m.s)-historySamples:]
	}
	m.mu.Unlock()
}

// rates returns bytes in and out per second over rateWindow and the errors
// over errorWindow. With less history than the window, what there is.
func (m *meter) rates(now time.Time) (inBps, outBps, errs int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.s) < 2 {
		return 0, 0, 0
	}
	last := m.s[len(m.s)-1]
	base := m.oldestAfter(now.Add(-rateWindow))
	if wall := last.at.Sub(base.at).Seconds(); wall > 0 {
		inBps = int64(float64(last.in-base.in) / wall)
		outBps = int64(float64(last.out-base.out) / wall)
	}
	errs = last.err - m.oldestAfter(now.Add(-errorWindow)).err
	return inBps, outBps, errs
}

// oldestAfter is the oldest sample taken at or after t, or the oldest kept.
// Caller holds m.mu.
func (m *meter) oldestAfter(t time.Time) sample {
	for _, s := range m.s {
		if !s.at.Before(t) {
			return s
		}
	}
	return m.s[0]
}

// sampler snapshots every channel's totals once a second for the life of the
// worker. One goroutine for all channels.
func (w *workerServer) sampler() {
	tick := time.NewTicker(sampleEvery)
	defer tick.Stop()
	for now := range tick.C {
		w.mu.RLock()
		chans := make([]*channel, 0, len(w.channels))
		for _, ch := range w.channels {
			chans = append(chans, ch)
		}
		w.mu.RUnlock()
		for _, ch := range chans {
			in, _, errs := ch.client.Traffic()
			ch.meter.add(sample{at: now, in: in, out: ch.out.Load(), err: errs})
		}
	}
}

// logRing keeps a channel's last logLines lines for GET /api/channels/{id}/log,
// so a misbehaving channel can be read about without the worker's stderr.
type logRing struct {
	mu    sync.Mutex
	lines []string
}

func (l *logRing) add(line string) {
	l.mu.Lock()
	l.lines = append(l.lines, line)
	if len(l.lines) > logLines {
		l.lines = l.lines[len(l.lines)-logLines:]
	}
	l.mu.Unlock()
}

func (l *logRing) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n") + "\n"
}

// countingWriter adds up what a channel sends its viewers. It passes Flush
// through, which the TS handler relies on to push each chunk out at once.
type countingWriter struct {
	http.ResponseWriter
	n *atomic.Int64
}

func (c countingWriter) Write(p []byte) (int, error) {
	n, err := c.ResponseWriter.Write(p)
	c.n.Add(int64(n))
	return n, err
}

func (c countingWriter) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func countBytes(h http.Handler, n *atomic.Int64) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(countingWriter{rw, n}, r)
	})
}

// Health thresholds. Constants on purpose: a controller reads one word and
// does not tune the worker.
const (
	// stalledAfterTargets is how many target durations the newest segment may
	// be old before the channel is stalled; players hold about three.
	stalledAfterTargets = 3
	// stalledAfterTS is the same for a raw TS pipe, which has no segment
	// duration to go by.
	stalledAfterTS = 15 * time.Second
	// downAfter is how long nothing may come out while the source keeps
	// failing before the channel is down rather than stalled. It is also how
	// long a fresh channel has to publish its first segment.
	downAfter = time.Minute
	// degradedRealtime is the publishing rate, in media seconds per wall
	// second, under which a channel that is still moving is falling behind.
	degradedRealtime = 0.9
	// degradedErrors is the fetch errors per minute past which a channel that
	// is still publishing is degraded.
	degradedErrors = 10
)

// health sums a channel up in one word for a controller: ok, degraded,
// stalled, down, or idle for an on-demand channel with nobody watching.
func health(state, kind string, uptime time.Duration, tracks []trackView, errsPerMin int64) string {
	switch state {
	case "idle", "starting":
		return "idle"
	case "error", "stopped":
		return "down"
	case "done":
		return "ok" // a finite source that finished is still served
	}
	var newest int64 = -1     // ms since the newest segment on any track
	var longest time.Duration // the longest target duration across the tracks
	realtime := 0.0
	for _, t := range tracks {
		if t.LastSegmentAgeMS >= 0 && (newest < 0 || t.LastSegmentAgeMS < newest) {
			newest = t.LastSegmentAgeMS
		}
		if d := time.Duration(t.targetDur * float64(time.Second)); d > longest {
			longest = d
		}
		if t.Type == "video" && t.Realtime > 0 && (realtime == 0 || t.Realtime < realtime) {
			realtime = t.Realtime
		}
	}
	// A raw TS pipe has no segment duration; a segmented source whose target
	// duration is not known yet gets the same allowance.
	stalledAfter := stalledAfterTS
	if kind != source.LiveTSKind && longest > 0 {
		stalledAfter = longest * stalledAfterTargets
	}
	switch {
	case newest < 0 && uptime > downAfter:
		return "down"
	case newest < 0:
		return "ok" // still starting up
	case time.Duration(newest)*time.Millisecond > downAfter && errsPerMin > 0:
		return "down"
	case time.Duration(newest)*time.Millisecond > stalledAfter:
		return "stalled"
	case realtime > 0 && realtime < degradedRealtime, errsPerMin > degradedErrors:
		return "degraded"
	}
	return "ok"
}

// ageMS is how many milliseconds ago t was, or -1 for a zero time.
func ageMS(t, now time.Time) int64 {
	if t.IsZero() {
		return -1
	}
	return now.Sub(t).Milliseconds()
}

func trackViews(stats []restream.Stats, now time.Time) []trackView {
	out := make([]trackView, 0, len(stats))
	for _, s := range stats {
		out = append(out, trackView{
			ID: s.ID, Type: s.Type, Bitrate: s.Bitrate, Width: s.Width, Height: s.Height,
			Seq: s.Seq, Published: s.Published, Window: s.Window, Realtime: s.Realtime,
			LastSegmentAgeMS: ageMS(s.LastAt, now), targetDur: s.TargetDur,
		})
	}
	return out
}

func fmtLine(format string, args ...any) string {
	return time.Now().UTC().Format("2006-01-02T15:04:05Z") + " " + fmt.Sprintf(format, args...)
}
