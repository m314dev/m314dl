package restream

import (
	"fmt"
	"io"
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
// ordered, decrypted input track into FFmpeg over its own pipe and reads one
// continuous MPEG-TS from FFmpeg's stdout into the broadcaster.
//
// The pipes differ by platform, see remux_unix.go and remux_windows.go. On
// Unix each track is an anonymous pipe that FFmpeg inherits as fd 3, 4 and so
// on. Windows gives a child only stdin, stdout and stderr, so there each track
// is a named pipe that FFmpeg opens by path. Neither touches the network. An
// earlier version used loopback TCP, and a user's machine turned out to drop
// even loopback connections, so sockets are out.
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
	inputs []io.WriteCloser // one per track, in -map order
	stderr *strings.Builder
	logv   func(string, ...any)
	closed sync.Once
}

// trackInput is one track's feed into FFmpeg: the end this process writes,
// and the ffmpeg arguments that name the other end, ending in "-i <source>".
type trackInput struct {
	w    io.WriteCloser
	args []string
}

// NewRemuxTS starts the FFmpeg remuxer with one input pipe per track.
// transcodeArgs replaces the output codec args; nil means stream copy.
func NewRemuxTS(ffmpeg string, ntracks int, transcodeArgs []string, b *TSBroadcaster, logv func(string, ...any)) (*RemuxTS, error) {
	if ntracks < 1 {
		return nil, fmt.Errorf("remux: no input tracks")
	}
	if logv == nil {
		logv = func(string, ...any) {}
	}

	cmd := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-nostdin")
	tracks, release, err := newTrackInputs(ntracks, cmd)
	if err != nil {
		return nil, err
	}
	defer release() // runs after Start, on every path out of here
	rm := &RemuxTS{b: b, cmd: cmd, stderr: new(strings.Builder), logv: logv}
	// No -thread_queue_size on the inputs. FFmpeg 9.1 dropped it as an input
	// option and refuses the whole command line when it sees one, and it had
	// done nothing for inputs since the 7.0 scheduler anyway. Older builds
	// keep their default queue; a full queue there only means the track
	// writer waits on its pipe a little longer.
	for _, t := range tracks {
		rm.inputs = append(rm.inputs, t.w)
		cmd.Args = append(cmd.Args, t.args...)
	}
	for i := 0; i < ntracks; i++ {
		cmd.Args = append(cmd.Args, "-map", fmt.Sprint(i))
	}
	if len(transcodeArgs) > 0 {
		cmd.Args = append(cmd.Args, transcodeArgs...)
	} else {
		cmd.Args = append(cmd.Args, "-c", "copy")
	}
	// Bound the muxer backlog so a lagging input cannot grow memory without
	// limit on endless live input (the worker's -shortest never fires there
	// either).
	cmd.Args = append(cmd.Args, "-max_muxing_queue_size", "1024", "-f", "mpegts", "pipe:1")

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		rm.End()
		return nil, err
	}
	cmd.Stderr = rm.stderr

	logv("ffmpeg remux: %s", strings.Join(cmd.Args, " "))
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
		// the cause ("moov atom not found", a failed open) on an earlier line
		// and ends with a generic "Error opening input files", which on its
		// own says nothing.
		if msg := strings.TrimSpace(rm.stderr.String()); msg != "" {
			if len(msg) > 2000 {
				msg = msg[len(msg)-2000:]
			}
			rm.logv("ffmpeg remux exited: %v\n%s", err, msg)
		} else {
			rm.logv("ffmpeg remux exited: %v", err)
		}
	}
	// FFmpeg is gone, so a track still waiting for it to open its end would
	// wait forever. Closing the inputs turns that wait into a write error.
	rm.End()
}

// Sink returns the engine.Sink that feeds input track i.
func (rm *RemuxTS) Sink(i int) engine.Sink { return remuxTrackSink{w: rm.inputs[i]} }

// End closes the input pipes so FFmpeg drains and exits; pump then ends the
// broadcaster. Safe to call more than once. Implements the presentation
// contract used by the restream run loop.
func (rm *RemuxTS) End() {
	rm.closed.Do(func() {
		for _, w := range rm.inputs {
			w.Close()
		}
	})
}

func (rm *RemuxTS) StatusLine() string { return rm.b.StatusLine() }

// remuxTrackSink writes one track's init and segments into its FFmpeg input
// pipe. For fMP4 the init (ftyp+moov) is written first, then the fragments.
// That is exactly a fragmented-MP4 byte stream, which FFmpeg demuxes from the
// pipe.
type remuxTrackSink struct{ w io.Writer }

func (s remuxTrackSink) Init(data []byte) error {
	_, err := s.w.Write(data)
	return err
}

func (s remuxTrackSink) Segment(_ engine.SegmentInfo, data []byte) error {
	_, err := s.w.Write(data)
	return err
}
