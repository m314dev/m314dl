package restream

import (
	"fmt"
	"io"
	"net"
	"os/exec"
	"strings"
	"sync"

	"github.com/m314dev/m314dl/internal/engine"
)

// Cross-container remux to MPEG-TS through one long-lived FFmpeg process.
//
// The pure-Go TS path (mpegts.go) only works when the source is already a muxed
// transport stream. When it is not (an fMP4/CMAF source, or separate video and
// audio that must be muxed together), RemuxTS fills the gap. It feeds each
// ordered, decrypted input track into FFmpeg over its own loopback TCP
// connection and reads one continuous MPEG-TS from FFmpeg's stdout into the
// broadcaster.
//
// Why TCP and not pipes. Handing FFmpeg extra pipes as fd 3, 4 and so on only
// works where a child inherits numbered files from its parent, which is Unix.
// Windows passes a child exactly three handles (stdin, stdout, stderr), and Go
// refuses ExtraFiles there outright. A loopback listener per track needs no
// inheritance at all: FFmpeg connects to it like any URL, so the same code runs
// on every platform and the Linux tests cover what Windows users run.
//
// This is the one deliberate, bounded use of FFmpeg that the rest of the engine
// avoids, and it keeps the properties that made the pure path good:
//   - ONE process for the whole stream, instead of one per segment (the drm
//     worker spawned one per segment, an anti-pattern);
//   - NO -re pacing. Input arrives at the source's own segment cadence, so the
//     output is paced without a wall-clock throttle;
//   - FFmpeg only ever sees an already-decrypted, clear copy, so the
//     mov-demuxer CENC (common encryption) leak that forced the worker to
//     decrypt upstream never applies here.
//
// The default is stream copy (remux only). An explicit transcode arg list
// replaces the copy codecs when a real re-encode is asked for.

const remuxReadChunk = 256 << 10 // read FFmpeg stdout in ~256KB chunks

// RemuxTS runs one FFmpeg that muxes N input tracks into a broadcast TS.
type RemuxTS struct {
	b      *TSBroadcaster
	cmd    *exec.Cmd
	inputs []*trackConn // one per track, in -map order
	stderr *strings.Builder
	logv   func(string, ...any)
	closed sync.Once
}

// trackConn is one input track's loopback connection to FFmpeg. FFmpeg opens
// its inputs one at a time and probes each before opening the next, so the
// connection for track 1 does not exist until track 0 has delivered enough to
// be understood. The first Write therefore waits for FFmpeg to connect. Each
// track runs in its own goroutine upstream, so that wait never holds up
// another track's feed.
type trackConn struct {
	ln   net.Listener
	once sync.Once
	conn net.Conn
	err  error
}

func (t *trackConn) accept() {
	t.conn, t.err = t.ln.Accept()
	t.ln.Close() // one connection per track; nothing else gets to knock
}

func (t *trackConn) Write(p []byte) (int, error) {
	t.once.Do(t.accept)
	if t.err != nil {
		return 0, t.err
	}
	return t.conn.Write(p)
}

// Close shuts the track. Closing the listener first frees a Write that is
// still waiting in Accept; the once.Do then waits for that Write to settle
// before conn is read, so there is no race on it.
func (t *trackConn) Close() {
	t.ln.Close()
	t.once.Do(func() { t.err = net.ErrClosed })
	if t.conn != nil {
		t.conn.Close()
	}
}

// NewRemuxTS starts the FFmpeg remuxer. It opens one loopback listener per
// track and hands FFmpeg their tcp:// addresses as inputs. transcodeArgs
// replaces the output codec args; nil means stream copy.
func NewRemuxTS(ffmpeg string, ntracks int, transcodeArgs []string, b *TSBroadcaster, logv func(string, ...any)) (*RemuxTS, error) {
	if ntracks < 1 {
		return nil, fmt.Errorf("remux: no input tracks")
	}
	if logv == nil {
		logv = func(string, ...any) {}
	}

	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin"}
	var inputs []*trackConn
	for i := 0; i < ntracks; i++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			for _, in := range inputs {
				in.Close()
			}
			return nil, fmt.Errorf("remux: listen for track %d: %w", i, err)
		}
		inputs = append(inputs, &trackConn{ln: ln})
		// -thread_queue_size well above FFmpeg's default of 8. With several
		// input demuxers reading at once the default queue fills up, FFmpeg
		// logs "Thread message queue blocking" and can stall, and the track
		// writers then block with it. A deeper queue absorbs bursty per-input
		// arrival.
		args = append(args, "-thread_queue_size", "512", "-i", "tcp://"+ln.Addr().String())
	}
	for i := 0; i < ntracks; i++ {
		args = append(args, "-map", fmt.Sprint(i))
	}
	if len(transcodeArgs) > 0 {
		args = append(args, transcodeArgs...)
	} else {
		args = append(args, "-c", "copy")
	}
	// Bound the muxer backlog so a lagging input cannot grow memory without
	// limit on endless live input (the worker's -shortest never fires there
	// either).
	args = append(args, "-max_muxing_queue_size", "1024", "-f", "mpegts", "pipe:1")

	cmd := exec.Command(ffmpeg, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr

	rm := &RemuxTS{b: b, cmd: cmd, inputs: inputs, stderr: &stderr, logv: logv}
	logv("ffmpeg remux: %s %s", ffmpeg, strings.Join(args, " "))
	if err := cmd.Start(); err != nil {
		rm.End()
		return nil, fmt.Errorf("start ffmpeg remux: %w", err)
	}
	go rm.pump(stdout)
	return rm, nil
}

// pump reads the remuxed TS from FFmpeg and fans it out until FFmpeg exits.
// Only whole 188-byte packets are published; the unaligned tail of a read is
// carried into the next one. A subscriber joins the broadcast at a publish
// boundary, and starting it mid-packet would feed the player torn TS.
func (rm *RemuxTS) pump(stdout io.ReadCloser) {
	buf := make([]byte, remuxReadChunk)
	fill := 0
	for {
		n, err := stdout.Read(buf[fill:])
		if n > 0 {
			fill += n
			if aligned := fill - fill%tsPacketSize; aligned > 0 {
				chunk := make([]byte, aligned)
				copy(chunk, buf[:aligned])
				rm.b.publish(chunk)
				fill = copy(buf, buf[aligned:fill])
			}
		}
		if err != nil {
			break // a final sub-packet fragment is undecodable; drop it
		}
	}
	// FFmpeg output ended: no more segments will be broadcast.
	rm.b.End()
	if err := rm.cmd.Wait(); err != nil {
		// Keep the whole tail of stderr, not just the last line. ffmpeg prints
		// the cause ("Connection to tcp://... failed: ...", "moov atom not
		// found") on an earlier line and ends with a generic "Error opening
		// input files", which on its own says nothing.
		if msg := strings.TrimSpace(rm.stderr.String()); msg != "" {
			if len(msg) > 2000 {
				msg = msg[len(msg)-2000:]
			}
			rm.logv("ffmpeg remux exited: %v\n%s", err, msg)
		} else {
			rm.logv("ffmpeg remux exited: %v", err)
		}
	}
	// FFmpeg is gone, so a track still waiting for it to connect would wait
	// forever. Closing the inputs turns that wait into a write error, the same
	// way a pipe with no reader would have failed.
	rm.End()
}

// Sink returns the engine.Sink that feeds input track i.
func (rm *RemuxTS) Sink(i int) engine.Sink { return remuxTrackSink{w: rm.inputs[i]} }

// End closes the input connections so FFmpeg drains and exits; pump then ends
// the broadcaster. Safe to call more than once. Implements the presentation
// contract used by the restream run loop.
func (rm *RemuxTS) End() {
	rm.closed.Do(func() {
		for _, in := range rm.inputs {
			in.Close()
		}
	})
}

func (rm *RemuxTS) StatusLine() string { return rm.b.StatusLine() }

// remuxTrackSink writes one track's init and segments into its FFmpeg input
// connection. For fMP4 the init (ftyp+moov) is written first, then the
// fragments. That is exactly a fragmented-MP4 byte stream, which FFmpeg demuxes
// from the socket.
type remuxTrackSink struct{ w io.Writer }

func (s remuxTrackSink) Init(data []byte) error {
	_, err := s.w.Write(data)
	return err
}

func (s remuxTrackSink) Segment(_ engine.SegmentInfo, data []byte) error {
	_, err := s.w.Write(data)
	return err
}
