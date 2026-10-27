package engine

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// Bounds for adaptive concurrency. Downloading segments is limited by the
// network, so these are counts of requests in flight, not CPU threads. The
// controller moves between min and max to find the point where more parallel
// requests stop helping. That way the user does not have to pass -t, and
// rate-limited servers are not hammered.
const (
	adaptiveMin   = 4
	adaptiveStart = 16
	adaptiveMax   = 64
)

// limiter is a concurrency gate whose size can change while it runs. A worker
// acquires a slot before a fetch and releases it after; the controller changes
// the ceiling live. A fixed semaphore cannot do this: setLimit may shrink the
// ceiling below the number already in flight, and new acquires then block
// until enough slots are released.
type limiter struct {
	mu       sync.Mutex
	cond     *sync.Cond
	limit    int
	inflight int
	closed   bool
}

func newLimiter(n int) *limiter {
	l := &limiter{limit: n}
	l.cond = sync.NewCond(&l.mu)
	return l
}

// acquire blocks until an in-flight slot is free or ctx is done.
func (l *limiter) acquire(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	for l.inflight >= l.limit && !l.closed {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		l.cond.Wait()
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	l.inflight++
	return nil
}

func (l *limiter) release() {
	l.mu.Lock()
	l.inflight--
	l.cond.Signal()
	l.mu.Unlock()
}

func (l *limiter) setLimit(n int) {
	l.mu.Lock()
	if n > l.limit {
		l.cond.Broadcast() // wake waiters that can now proceed
	}
	l.limit = n
	l.mu.Unlock()
}

func (l *limiter) getLimit() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.limit
}

// unblock wakes every waiter so each one re-checks ctx. It is called on
// cancellation.
func (l *limiter) unblock() {
	l.mu.Lock()
	l.closed = true
	l.cond.Broadcast()
	l.mu.Unlock()
}

// controller adjusts a limiter with AIMD (additive increase, multiplicative
// decrease: grow in small steps, cut by half) based on measured throughput. It
// never goes above a ceiling, which is the user's -t, or adaptiveMax when -t is
// unset. Workers report bytes, hard errors and rate-limit pressure; the control
// loop reads those counters once per tick.
type controller struct {
	lim        *limiter
	minC, maxC int
	fixed      bool // user pinned -t: hold maxC, back off only on real pressure
	bytes      atomic.Int64
	errs       atomic.Int64
	press      atomic.Int64
}

// newController builds a controller whose concurrency never exceeds ceiling.
// It starts at the usual ramp point (or the ceiling, if lower) and can shrink
// down to the floor (or the ceiling, if that is lower still).
func newController(ceiling int) *controller {
	if ceiling < 1 {
		ceiling = adaptiveMax
	}
	start := adaptiveStart
	if ceiling < start {
		start = ceiling
	}
	minC := adaptiveMin
	if ceiling < minC {
		minC = ceiling
	}
	return &controller{lim: newLimiter(start), minC: minC, maxC: ceiling}
}

// newFixedController honors a -t the user pinned. It starts at the requested
// count rather than the ramp point, and stays there. The auto-tuner instead
// climbs from adaptiveStart and moves up and down around the point where more
// concurrency stops helping. Passing -t 60 should mean "use 60", the same as
// N_m3u8DL's --thread-count. Real rate-limit pressure still backs it off, and
// then it climbs straight back.
func newFixedController(n int) *controller {
	c := newController(n)
	c.fixed = true
	c.lim.setLimit(c.maxC) // start at the pinned count instead of adaptiveStart
	return c
}

func (c *controller) addBytes(n int)    { c.bytes.Add(int64(n)) }
func (c *controller) addErr()           { c.errs.Add(1) }
func (c *controller) addPressure(n int) { c.press.Add(int64(n)) }

// ctlState is the state the control loop carries from one tick to the next.
// It is kept separate so the decision itself is a pure function that tests
// can call directly.
type ctlState struct {
	slowStart bool
	cooldown  int
	prevTP    float64
}

// decide returns the next limit and state, given the current limit, this
// tick's throughput (in bytes) and its error count. During slow start the
// limit doubles while throughput keeps climbing. After that, additive increase
// and multiplicative decrease hold it near the point where more concurrency
// stops helping. Any hard error halves the limit and starts a cooldown.
func decide(limit int, tp float64, errs int64, minC, maxC int, s ctlState) (int, ctlState) {
	step := limit / 8
	if step < 1 {
		step = 1
	}
	switch {
	case errs > 0:
		limit = maxInt(minC, limit/2)
		s.slowStart = false
		s.cooldown = 2
	case s.cooldown > 0:
		s.cooldown--
	case s.slowStart:
		if tp > s.prevTP*1.05 {
			limit = minInt(maxC, limit*2)
		} else {
			s.slowStart = false
		}
	case tp > s.prevTP*1.10:
		limit = minInt(maxC, limit+step)
	case tp < s.prevTP*0.90:
		limit = maxInt(minC, limit-step)
	}
	s.prevTP = tp
	return limit, s
}

// decideFixed is the control rule when the user pinned -t: hold maxC, halve on
// real rate-limit pressure, then climb straight back. It never lowers the limit
// because of throughput, so a plateau (the normal steady state on a real CDN)
// cannot drag concurrency below what the user asked for.
func decideFixed(limit int, errs int64, minC, maxC int, s ctlState) (int, ctlState) {
	switch {
	case errs > 0:
		limit = maxInt(minC, limit/2)
		s.cooldown = 2
	case s.cooldown > 0:
		s.cooldown--
	case limit < maxC:
		step := limit / 8
		if step < 1 {
			step = 1
		}
		limit = minInt(maxC, limit+step)
	}
	return limit, s
}

// run is the control loop; it exits when ctx is cancelled.
func (c *controller) run(ctx context.Context, verbose func(string, ...any)) {
	const tick = 500 * time.Millisecond
	t := time.NewTicker(tick)
	defer t.Stop()
	s := ctlState{slowStart: true}
	var lastBytes int64
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		total := c.bytes.Load()
		tp := float64(total - lastBytes)
		lastBytes = total
		// rate-limit pressure counts the same as a hard error for backing off
		errs := c.errs.Swap(0) + c.press.Swap(0)
		cur := c.lim.getLimit()
		var next int
		var ns ctlState
		if c.fixed {
			next, ns = decideFixed(cur, errs, c.minC, c.maxC, s)
		} else {
			next, ns = decide(cur, tp, errs, c.minC, c.maxC, s)
		}
		s = ns
		if next != cur {
			c.lim.setLimit(next)
			verbose("adaptive: concurrency %d→%d (%.1f MiB/s, %d backoff)", cur, next, tp/(1<<20), errs)
		}
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
