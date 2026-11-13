package source

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"time"

	"github.com/m314dev/m314dl/internal/engine"
	"github.com/m314dev/m314dl/internal/httpx"
	"github.com/m314dev/m314dl/internal/manifest"
)

// A live MPEG-TS source is a raw transport stream served over HTTP with no
// manifest and no length. It has no segments, so the restream modes keep it
// out of the segment engine: PipeLiveTS hands the body to the TS output in
// whole packets as it arrives. A plain download records it like a file.

// LiveTSKind is the kind LoadManifest returns for a live MPEG-TS source. Its
// master is exactly the one a direct file gets, so every download path treats
// it as a file. Only the restream modes tell the two apart: they publish
// LiveTSTrack and run it with PipeLiveTS.
const LiveTSKind = "livets"

const (
	tsPacket = 188
	// tsProbePackets is how many packets in a row, 188 bytes apart and each
	// starting with the 0x47 sync byte, make a body MPEG-TS. Seven is one UDP
	// datagram's worth. A file that only carries a few 0x47 bytes at that
	// spacing, like the blob the file tests download, is not mistaken for one.
	tsProbePackets = 7
	// tsProbeBytes is how much of the body the check reads: enough for the run
	// to start anywhere in the first packet's worth of bytes.
	tsProbeBytes = tsPacket * (tsProbePackets + 1)
)

// isLiveTS reports whether a direct file is a live MPEG-TS stream: the server
// gave no length and the body is TS packets. TS with a length stays a file.
// The Content-Type is not checked, since the same stream can come with any
// type. It reads a little past head; the caller closes the body right after.
func isLiveTS(resp *http.Response, head []byte) bool {
	if resp.StatusCode != http.StatusOK || resp.ContentLength >= 0 {
		return false
	}
	b := head
	if len(b) < tsProbeBytes {
		more, _ := io.ReadAll(io.LimitReader(resp.Body, int64(tsProbeBytes-len(b))))
		b = append(b, more...)
	}
	return findSync(b, tsProbePackets) >= 0
}

// findSync returns the offset of the first run of n whole packets in b: n
// 0x47 sync bytes, 188 bytes apart. It returns -1 when b holds no such run.
func findSync(b []byte, n int) int {
	for off := 0; off+n*tsPacket <= len(b); off++ {
		run := true
		for i := 0; i < n; i++ {
			if b[off+i*tsPacket] != 0x47 {
				run = false
				break
			}
		}
		if run {
			return off
		}
	}
	return -1
}

// LiveTSTrack is the track a restream publishes for a LiveTSKind master: the
// source's own transport stream, video with the audio muxed in, live.
func LiveTSTrack(m *manifest.Master) *manifest.Stream {
	st := *m.Streams[0]
	st.Type, st.ID, st.MuxedAudio, st.Live = manifest.Video, "ts", true, true
	return &st
}

// The pipe's timings. They are vars so tests can shorten them.
var (
	// pipeIdle is how long a connection may go without a byte, from the
	// request on, before it is dropped and opened again.
	pipeIdle = 15 * time.Second
	// pipeSteady is how long a connection must have run for its end to count
	// as an ordinary drop, which is reopened at once. A shorter one that still
	// brought media waits pipeBackoff first, so a source that keeps hanging up
	// right away is not hammered.
	pipeSteady = 5 * time.Second
	// pipeBackoff is the first wait before opening the source again after a
	// short or failed connection. It doubles up to pipeMaxBackoff, and starts
	// over after a steady connection.
	pipeBackoff = 500 * time.Millisecond
)

const (
	pipeMaxBackoff = 8 * time.Second
	// pipeAttempts is how many connections in a row may bring no media before
	// PipeLiveTS gives up: refused, silent until the idle check, or not
	// MPEG-TS. A connection that brought media starts the count over.
	pipeAttempts = 5
	// pipeChunk is the least PipeLiveTS hands the sink at once. A live body
	// arrives a few KB per read. Publishing every read would put hundreds of
	// tiny units a second through the TS broadcaster, where a viewer's slot
	// count instead of its byte budget would decide when it is too far behind.
	// 87 packets (16 KB) is a few dozen units a second at TV bitrates.
	pipeChunk = 87 * tsPacket
	// pipeMaxSkip is how many bytes PipeLiveTS throws away looking for packets
	// before it decides the connection is not carrying MPEG-TS.
	pipeMaxSkip = 1 << 20
)

var (
	errNotTS  = errors.New("the source is not sending MPEG-TS")
	errClosed = errors.New("the source closed the connection")
	// errSink marks an error from the sink. It ends the pipe, since opening
	// the source again cannot fix the output.
	errSink = errors.New("output")
)

// PipeLiveTS copies a live MPEG-TS source into sink, whole packets at a time,
// until ctx ends. One connection's body is the stream, so nothing goes to
// disk and nothing waits for the body to end. When the connection ends or
// stalls, PipeLiveTS opens the source again by the URL as given. When that URL
// redirects to an expiring token, this also fetches a fresh token. The sink
// sees one stream with a jump at the seam, and the TS output's continuity
// counters carry on across it, as across an HLS segment boundary. PipeLiveTS
// returns an error once pipeAttempts connections in a row have brought no
// media, and at once when the sink fails.
func PipeLiveTS(ctx context.Context, client *httpx.Client, st *manifest.Stream, sink engine.Sink, logv func(string, ...any)) error {
	if logv == nil {
		logv = func(string, ...any) {}
	}
	failed, backoff := 0, pipeBackoff
	for {
		start := time.Now()
		n, err := pipeOnce(ctx, client, st.PlaylistURL, sink)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, errSink) {
			return err
		}
		if n > 0 {
			failed = 0
			if time.Since(start) >= pipeSteady {
				backoff = pipeBackoff
				logv("live ts: connection ended after %d bytes (%v); reconnecting", n, err)
				continue
			}
		} else if failed++; failed >= pipeAttempts {
			return fmt.Errorf("live ts: %d connections in a row brought no media, the last: %w", failed, err)
		}
		logv("live ts: connection ended after %d bytes (%v); reconnecting in %s", n, err, backoff)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff + time.Duration(rand.Int64N(int64(backoff/2)+1))):
		}
		backoff = min(2*backoff, pipeMaxBackoff)
	}
}

// pipeOnce runs one connection: it opens u and pipes the body into sink until
// the body ends, fails, or goes pipeIdle without a byte. It returns the bytes
// handed to sink and why the connection ended (never nil).
func pipeOnce(parent context.Context, client *httpx.Client, u string, sink engine.Sink) (int64, error) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	wd := time.AfterFunc(pipeIdle, cancel)
	defer wd.Stop()
	var n int64
	resp, err := client.Open(ctx, u, "")
	if err == nil {
		n, err = pipeBody(resp.Body, sink, func() { wd.Reset(pipeIdle) })
		resp.Body.Close()
	}
	if ctx.Err() != nil && parent.Err() == nil && !errors.Is(err, errSink) {
		err = fmt.Errorf("no data for %s", pipeIdle) // the idle check cut it
	}
	return n, err
}

// pipeBody hands one connection's body to sink in whole TS packets, at least
// pipeChunk bytes at a time. It finds the first packet before sending
// anything, and finds the packets again whenever the stream loses its
// alignment, so the sink never gets a torn packet. keepalive runs whenever
// bytes arrive. It returns the bytes handed to sink and why the body ended
// (never nil); a sink error comes back wrapped in errSink.
func pipeBody(body io.Reader, sink engine.Sink, keepalive func()) (int64, error) {
	buf := make([]byte, 4*pipeChunk)
	fill, synced := 0, false
	var sent, skipped int64 // skipped: bytes dropped looking for packets since the last send
	for {
		n, rerr := body.Read(buf[fill:])
		if n > 0 {
			keepalive()
			fill += n
		}
		for {
			if !synced {
				off := findSync(buf[:fill], tsProbePackets)
				if off < 0 {
					// A run may start in the last bytes, so keep those.
					keep := min(fill, tsProbePackets*tsPacket-1)
					skipped += int64(fill - keep)
					fill = copy(buf, buf[fill-keep:fill])
					break
				}
				skipped += int64(off)
				fill = copy(buf, buf[off:fill])
				synced = true
			}
			good := 0 // whole packets from the start of buf that are still in sync
			for good+tsPacket <= fill && buf[good] == 0x47 {
				good += tsPacket
			}
			lost := good < fill && buf[good] != 0x47
			if good > 0 && (lost || good >= pipeChunk || rerr != nil) {
				if err := sink.Segment(engine.SegmentInfo{}, buf[:good]); err != nil {
					return sent, fmt.Errorf("%w: %w", errSink, err)
				}
				sent += int64(good)
				skipped = 0
				fill = copy(buf, buf[good:fill])
			}
			if !lost {
				break
			}
			synced = false
		}
		if skipped > pipeMaxSkip {
			return sent, errNotTS
		}
		if rerr == io.EOF {
			if sent == 0 && (fill > 0 || skipped > 0) {
				return 0, errNotTS
			}
			return sent, errClosed
		}
		if rerr != nil {
			return sent, rerr
		}
	}
}
