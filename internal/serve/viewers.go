package serve

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// Viewers counts a channel's audience from its HTTP traffic. A viewer is a
// distinct client address with a request open (the TS output holds one for
// the whole view) or one made within the window (an HLS or DASH player polls
// and fetches every few seconds). It is approximate behind a shared proxy, but
// honest for a status line, and the same count serves every output format.
type Viewers struct {
	window time.Duration
	now    func() time.Time

	mu       sync.Mutex
	active   map[string]int       // open requests per client address
	recent   map[string]time.Time // last request per client address
	lastSeen time.Time            // the newest request start or end
}

// NewViewers counts viewers over window.
func NewViewers(window time.Duration) *Viewers {
	return &Viewers{window: window, now: time.Now, active: map[string]int{}, recent: map[string]time.Time{}}
}

// Enter records a request and returns what to call when it ends.
func (v *Viewers) Enter(r *http.Request) (leave func()) {
	ip := clientIP(r)
	v.mu.Lock()
	v.active[ip]++
	v.recent[ip] = v.now()
	v.lastSeen = v.now()
	v.mu.Unlock()
	return func() {
		v.mu.Lock()
		if v.active[ip]--; v.active[ip] <= 0 {
			delete(v.active, ip)
		}
		v.lastSeen = v.now() // a long connection that just ended counts as seen now
		v.mu.Unlock()
	}
}

// Count is the number of viewers right now.
func (v *Viewers) Count() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	cutoff := v.now().Add(-v.window)
	n := 0
	for ip, at := range v.recent {
		if v.active[ip] > 0 || at.After(cutoff) {
			n++
		} else {
			delete(v.recent, ip) // ponytail: pruned on read; fine for the handful of viewers a channel has
		}
	}
	return n
}

// Quiet reports that no request is open and none has been seen for idle.
func (v *Viewers) Quiet(idle time.Duration) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.active) == 0 && v.now().Sub(v.lastSeen) >= idle
}

// Touch marks the audience as just seen, without a request. A session start
// counts, so a channel is not released in the seconds before its first bytes.
func (v *Viewers) Touch() {
	v.mu.Lock()
	v.lastSeen = v.now()
	v.mu.Unlock()
}

// Wrap counts the requests that pass through h.
func (v *Viewers) Wrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer v.Enter(r)()
		h.ServeHTTP(w, r)
	})
}

func clientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
