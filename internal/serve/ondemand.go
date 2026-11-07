package serve

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/m314dev/m314dl/internal/restream"
)

// Starter builds one session of a channel: it loads the source again, selects
// the same streams and builds the output. ctx is cancelled when the session
// stops.
type Starter func(ctx context.Context) (Presentation, http.Handler, []Job, error)

// OnDemand keeps a live channel's source running only while someone is
// watching. It is the HTTP handler for the channel. The first request after a
// quiet spell starts a session and waits for it; a session is stopped once no
// request has come in for the idle period and no connection is open. The
// published URLs never change, and a new session carries the HLS media
// sequence on from the one before so a player that kept polling sees one
// stream with a discontinuity, not a stream that started over.
//
// A viewer is any request under the channel's path, or an open connection
// (the TS output holds one for the whole view). The viewer count is the
// number of distinct client addresses seen in the last 30 seconds or the idle
// period, whichever is shorter, which is approximate behind a shared proxy
// but honest for a status line.
type OnDemand struct {
	idle  time.Duration
	start Starter
	run   func(ctx context.Context, jobs []Job) error
	log   func(string, ...any)
	now   func() time.Time

	mu       sync.Mutex
	state    string        // idle | starting | running | stopping | closed
	change   chan struct{} // closed when a starting or stopping state ends
	handler  http.Handler
	pres     Presentation
	cancel   context.CancelFunc
	done     chan struct{} // closed when the session's run returns
	lastErr  string
	prevPub  *restream.Publisher
	lastSeen time.Time
	active   map[string]int       // open requests per client address
	recent   map[string]time.Time // last request per client address
}

// NewOnDemand returns a channel handler that starts the source with start and
// drives it with run while viewers are present, stopping it after idle
// without any. log receives one line per start and stop.
func NewOnDemand(idle time.Duration, start Starter, run func(context.Context, []Job) error, log func(string, ...any)) *OnDemand {
	if log == nil {
		log = func(string, ...any) {}
	}
	return &OnDemand{
		idle: idle, start: start, run: run, log: log, now: time.Now,
		state: "idle", active: map[string]int{}, recent: map[string]time.Time{},
	}
}

func (od *OnDemand) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	od.mu.Lock()
	if od.state == "closed" {
		od.mu.Unlock()
		http.NotFound(w, r)
		return
	}
	od.active[ip]++
	od.recent[ip] = od.now()
	od.lastSeen = od.now()
	od.mu.Unlock()
	defer func() {
		od.mu.Lock()
		if od.active[ip]--; od.active[ip] <= 0 {
			delete(od.active, ip)
		}
		od.lastSeen = od.now() // a long connection that just ended counts as seen now
		od.mu.Unlock()
	}()

	h, err := od.session(r.Context())
	if err != nil {
		http.Error(w, "stream unavailable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	h.ServeHTTP(w, r)
}

// session returns the running session's handler, starting one when the
// channel is idle. A request that lands while a start or stop is under way
// waits for it to finish and then looks again.
func (od *OnDemand) session(ctx context.Context) (http.Handler, error) {
	for {
		od.mu.Lock()
		switch od.state {
		case "running":
			h := od.handler
			od.mu.Unlock()
			return h, nil
		case "starting", "stopping":
			wait := od.change
			od.mu.Unlock()
			select {
			case <-wait:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		case "closed":
			od.mu.Unlock()
			return nil, errors.New("channel closed")
		default: // idle
			od.state = "starting"
			od.change = make(chan struct{})
			od.mu.Unlock()
			err := od.startSession()
			od.mu.Lock()
			close(od.change)
			od.mu.Unlock()
			if err != nil {
				return nil, err
			}
		}
	}
}

// startSession runs the Starter and, on success, installs the session and
// starts its download and its idle watcher. Called with state "starting".
func (od *OnDemand) startSession() error {
	od.log("on demand: viewer arrived, starting the source")
	ctx, cancel := context.WithCancel(context.Background())
	pres, h, jobs, err := od.start(ctx)
	od.mu.Lock()
	defer od.mu.Unlock()
	if err != nil {
		cancel()
		od.state, od.lastErr = "idle", err.Error()
		od.log("on demand: start failed: %v", err)
		return err
	}
	if od.state == "closed" {
		cancel()
		pres.End()
		return errors.New("channel closed")
	}
	if pub, ok := pres.(*restream.Publisher); ok && od.prevPub != nil {
		pub.ContinueFrom(od.prevPub)
	}
	done := make(chan struct{})
	od.handler, od.pres, od.cancel, od.done = h, pres, cancel, done
	od.state, od.lastErr = "running", ""
	od.lastSeen = od.now()
	go func() {
		// End the presentation on cancel too, so a remux writer stuck on a
		// stalled ffmpeg gets an error and the run can return.
		<-ctx.Done()
		pres.End()
	}()
	go func() {
		err := od.run(ctx, jobs)
		pres.End()
		od.mu.Lock()
		if od.state == "running" {
			// The source ended or failed on its own. Back to idle; the next
			// viewer starts it again.
			od.retireLocked()
			if err != nil {
				od.lastErr = err.Error()
				od.log("on demand: source failed: %v", err)
			} else {
				od.log("on demand: source ended")
			}
		}
		close(done)
		od.mu.Unlock()
	}()
	go od.watch(done)
	return nil
}

// retireLocked drops the finished session and remembers its publisher so the
// next one can continue its numbering. Caller holds od.mu.
func (od *OnDemand) retireLocked() {
	if pub, ok := od.pres.(*restream.Publisher); ok {
		od.prevPub = pub
	}
	od.handler, od.pres, od.cancel, od.done = nil, nil, nil, nil
	if od.state != "closed" {
		od.state = "idle"
	}
}

// watch stops the session once nobody has asked for anything for the idle
// period and no connection is open. It returns when the session is gone.
func (od *OnDemand) watch(done chan struct{}) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-done:
			return
		case <-tick.C:
		}
		od.mu.Lock()
		quiet := od.state == "running" && len(od.active) == 0 && od.now().Sub(od.lastSeen) >= od.idle
		if !quiet {
			od.mu.Unlock()
			continue
		}
		od.state = "stopping"
		od.change = make(chan struct{})
		cancel := od.cancel
		od.mu.Unlock()
		od.log("on demand: no viewers for %s, releasing the source", od.idle)
		cancel()
		<-done
		od.mu.Lock()
		od.retireLocked()
		close(od.change)
		od.mu.Unlock()
		return
	}
}

// Close stops the channel for good; later requests get 404. It implements
// Presentation.End.
func (od *OnDemand) Close() {
	od.mu.Lock()
	cancel, done := od.cancel, od.done
	od.state = "closed"
	od.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

// End implements Presentation.
func (od *OnDemand) End() { od.Close() }

// State is idle, starting, running or closed.
func (od *OnDemand) State() string {
	od.mu.Lock()
	defer od.mu.Unlock()
	if od.state == "stopping" {
		return "idle"
	}
	return od.state
}

// LastError is the message of the most recent failed start or run, or "".
func (od *OnDemand) LastError() string {
	od.mu.Lock()
	defer od.mu.Unlock()
	return od.lastErr
}

// Viewers counts distinct client addresses with an open request or a request
// within the viewer window.
func (od *OnDemand) Viewers() int {
	od.mu.Lock()
	defer od.mu.Unlock()
	return od.viewersLocked()
}

func (od *OnDemand) viewersLocked() int {
	window := 30 * time.Second
	if od.idle < window {
		window = od.idle
	}
	cutoff := od.now().Add(-window)
	n := 0
	for ip, at := range od.recent {
		if od.active[ip] > 0 || at.After(cutoff) {
			n++
		} else {
			delete(od.recent, ip) // ponytail: pruned on read; fine for the handful of viewers a channel has
		}
	}
	return n
}

// StatusLine implements Presentation.
func (od *OnDemand) StatusLine() string {
	od.mu.Lock()
	defer od.mu.Unlock()
	switch od.state {
	case "running":
		inner := ""
		if od.pres != nil {
			inner = " | " + od.pres.StatusLine()
		}
		return fmt.Sprintf("%d viewers%s", od.viewersLocked(), inner)
	case "starting":
		return "starting"
	case "closed":
		return "closed"
	}
	if od.lastErr != "" {
		return "idle (last error: " + od.lastErr + ")"
	}
	return "idle (no viewers)"
}

func clientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
