package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/m314dev/m314dl/internal/httpx"
)

// partPath is the file a segment's raw bytes stream into before the writer
// decrypts them and appends them to the output. Raw means still encrypted and
// still carrying any fake image header. The name comes from the output path,
// so concurrent downloads never collide, and it is the same on every rerun, so
// a killed download resumes from the partial bytes already on disk. With dir
// set (Config.PartDir) the part files live there instead of beside the output.
func partPath(dir, outPath string, idx int64) string {
	if dir != "" {
		outPath = filepath.Join(dir, filepath.Base(outPath))
	}
	return fmt.Sprintf("%s.part%d", outPath, idx)
}

// downloadSegment streams one segment's raw body into its part file. It
// resumes from whatever bytes are already there, using HTTP byte ranges, and
// retries failures that may be temporary. It does not decrypt. The writer does
// that in index order, one segment at a time, so peak memory is one segment
// instead of one per worker. It returns the number of rate-limit responses
// (429 or 5xx) it saw, for the controller.
func downloadSegment(ctx context.Context, cfg Config, ctl *controller, it item, path string) (int, error) {
	// The segment's byte range (HLS EXT-X-BYTERANGE or DASH SegmentBase), or
	// the whole object when there is none.
	var segStart, segEnd int64 = 0, -1
	if it.rng != nil {
		segStart, segEnd = it.rng.Start, it.rng.End
	}
	segLen := int64(-1)
	if segEnd >= 0 {
		segLen = segEnd - segStart + 1
	}

	var have int64
	if fi, err := os.Stat(path); err == nil {
		have = fi.Size()
	}
	if segLen >= 0 && have >= segLen {
		return 0, nil // ranged segment already complete on disk
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	if _, err := f.Seek(have, io.SeekStart); err != nil {
		return 0, err
	}

	pressure := 0
	backoff := 500 * time.Millisecond
	u, refreshed := it.url, false
	for attempt := 0; ; attempt++ {
		if cfg.fileURL != nil {
			u = cfg.fileURL.get() // another worker may have re-resolved it
		}
		rngHdr := resumeRange(it.rng != nil, segStart, segEnd, have)
		// Idle watchdog for this attempt. A CDN that is throttling us (common
		// when concurrency is pushed high) often keeps the connection open and
		// sends nothing, instead of returning 429. Without the watchdog the read
		// below would block forever and the whole download would sit at 0 B/s.
		// The watchdog cancels this attempt when no byte arrives for
		// segIdleTimeout, whether headers or body, so the retry loop picks up
		// from the bytes already on disk. ctx.Err() stays nil because only the
		// child context was cancelled. That is why the paths below must retry a
		// cancelled attempt instead of failing the segment.
		reqCtx, cancel := context.WithCancel(ctx)
		wd := time.AfterFunc(segIdleTimeout, cancel)
		resp, err := cfg.Client.RangeGet(reqCtx, u, rngHdr)
		if err != nil {
			wd.Stop()
			cancel()
			if ctx.Err() != nil || attempt >= cfg.Client.Retries() {
				return pressure, err
			}
			if !retryWait(ctx, &backoff, 0) {
				return pressure, ctx.Err()
			}
			continue
		}

		body := io.Reader(resp.Body)
		switch {
		case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable:
			resp.Body.Close()
			wd.Stop()
			cancel()
			return pressure, nil // server says we already have it all
		case resp.StatusCode == http.StatusOK && have > 0:
			// server ignored Range: restart from the beginning
			resp.Body.Close()
			wd.Stop()
			cancel()
			if err := f.Truncate(0); err != nil {
				return pressure, err
			}
			f.Seek(0, io.SeekStart)
			have = 0
			continue
		case resp.StatusCode == http.StatusOK && it.rng != nil && segStart > 0 && resp.ContentLength != segLen:
			// A ranged segment was answered with the whole object: a 200 whose
			// length is not the slice's. Writing that as the segment would put
			// the start of the object where this slice belongs, so refuse
			// instead of corrupting the output.
			resp.Body.Close()
			wd.Stop()
			cancel()
			f.Close()
			os.Remove(path) // empty; don't litter the output dir
			return pressure, ErrRangeIgnored
		case resp.StatusCode == http.StatusOK && it.rng != nil && segLen >= 0:
			// A slice starting at offset 0 was answered with the whole object.
			// The object starts where the slice starts, so keep the slice's
			// length of it and drop the rest.
			body = io.LimitReader(resp.Body, segLen)
		case resp.StatusCode >= 400:
			code := resp.StatusCode
			ra := httpx.ParseRetryAfter(resp.Header.Get("Retry-After"))
			io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			wd.Stop()
			cancel()
			if httpx.Retriable(code) {
				pressure++
			}
			// The token was refused (see fileURL): resolve the URL again, once
			// per segment. Only once, so a server that refuses for some other
			// reason still fails the segment.
			if (code == http.StatusNotAcceptable || code == http.StatusForbidden) &&
				cfg.fileURL != nil && !refreshed && ctx.Err() == nil {
				refreshed = true
				if err := cfg.fileURL.refresh(ctx, cfg.Client, u); err == nil {
					cfg.Verbose("HTTP %d on %s; re-resolved the file URL", code, u)
					continue
				}
			}
			if !httpx.Retriable(code) || attempt >= cfg.Client.Retries() || ctx.Err() != nil {
				return pressure, &httpx.StatusError{Code: code, URL: u}
			}
			if !retryWait(ctx, &backoff, ra) {
				return pressure, ctx.Err()
			}
			continue
		}

		// 2xx: stream the body to disk, counting bytes as they arrive. Every
		// byte received resets the watchdog, so a slow transfer that is still
		// alive is never cut.
		n, rerr := streamBody(body, f, cfg.Progress, ctl, func() {
			wd.Reset(segIdleTimeout)
		})
		resp.Body.Close()
		wd.Stop()
		cancel()
		have += n
		if rerr == nil && segLen >= 0 && have < segLen {
			// A 206 shorter than the slice we asked for (some CDNs cap the
			// length of a range). We are not done yet; the retry below picks up
			// from the bytes we have.
			rerr = io.ErrUnexpectedEOF
		}
		if rerr == nil {
			return pressure, nil // complete
		}
		// The body failed part way through (a watchdog cancel counts): retry,
		// resuming from the bytes we kept. A watchdog stall counts as rate-limit
		// pressure. The guard checks the parent ctx on purpose: a watchdog
		// cancel leaves it nil, so we retry, while a user abort cancels the
		// parent and we stop.
		if attempt >= cfg.Client.Retries() || ctx.Err() != nil {
			return pressure, fmt.Errorf("read body: %w", rerr)
		}
		pressure++
		if !retryWait(ctx, &backoff, 0) {
			return pressure, ctx.Err()
		}
	}
}

// ErrRangeIgnored means a server answered a byte-range request for a slice
// past offset 0 with a 200 and the whole object. The segment fails instead of
// corrupting the output, and DownloadFile falls back to one connection.
var ErrRangeIgnored = errors.New("server ignored Range request")

// segIdleTimeout is how long one segment fetch may go without receiving any
// bytes before it is abandoned and retried. It is long enough that a slow link
// that is still alive is never cut, and short enough that a connection being
// silently throttled recovers on its own instead of stalling the whole download
// at 0 B/s.
const segIdleTimeout = 30 * time.Second

// resumeRange builds the Range header that asks for the part not downloaded
// yet.
func resumeRange(ranged bool, segStart, segEnd, have int64) string {
	if ranged {
		start := segStart + have
		if segEnd >= 0 {
			return fmt.Sprintf("bytes=%d-%d", start, segEnd)
		}
		return fmt.Sprintf("bytes=%d-", start)
	}
	if have > 0 {
		return fmt.Sprintf("bytes=%d-", have)
	}
	return ""
}

// copyBufs recycles the 1 MiB read buffers. Allocating a fresh one per segment
// was 84% of everything a 2000-segment download allocated, and zeroing those
// buffers alone took 15% of its CPU time.
var copyBufs = sync.Pool{New: func() any { b := make([]byte, 1<<20); return &b }}

// streamBody copies body into f and reports bytes to the progress line and the
// controller as they arrive. That keeps the throughput reading smooth and the
// byte counter moving during a very large segment.
func streamBody(body io.Reader, f *os.File, prog *Progress, ctl *controller, keepalive func()) (int64, error) {
	bp := copyBufs.Get().(*[]byte)
	defer copyBufs.Put(bp)
	buf := *bp
	var total int64
	for {
		n, rerr := body.Read(buf)
		if n > 0 {
			if keepalive != nil {
				keepalive() // bytes arrived: reset the idle watchdog
			}
			if _, werr := f.Write(buf[:n]); werr != nil {
				return total, werr
			}
			total += int64(n)
			if prog != nil {
				prog.AddBytes(int64(n))
			}
			if ctl != nil {
				ctl.addBytes(n)
			}
		}
		if rerr == io.EOF {
			return total, nil
		}
		if rerr != nil {
			return total, rerr
		}
	}
}

// retryWait sleeps before a retry. The wait doubles each time, with some random
// jitter added, unless the server sent a Retry-After, which is used instead
// (capped). It returns false if ctx is cancelled while waiting.
func retryWait(ctx context.Context, backoff *time.Duration, retryAfter time.Duration) bool {
	wait := *backoff + time.Duration(rand.Int64N(int64(*backoff/2)))
	if retryAfter > 0 {
		wait = retryAfter
		if wait > 30*time.Second {
			wait = 30 * time.Second
		}
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(wait):
	}
	if *backoff < 8*time.Second {
		*backoff *= 2
	}
	return true
}
