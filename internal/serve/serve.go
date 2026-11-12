// Package serve is the restream mode (-serve): it downloads the selected
// streams and republishes them live over HTTP instead of writing a file. Two
// output shapes share one run loop:
//
//   - HLS (-serve-format hls, the default): a multivariant playlist plus a
//     rolling window of segments, with one media playlist per track.
//   - MPEG-TS (-serve-format ts): a single continuous transport stream that is
//     copied out to every viewer. It needs one muxed TS source track.
package serve

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/m314dev/m314dl/internal/engine"
	"github.com/m314dev/m314dl/internal/httpx"
	"github.com/m314dev/m314dl/internal/manifest"
	"github.com/m314dev/m314dl/internal/mux"
	"github.com/m314dev/m314dl/internal/restream"
	"github.com/m314dev/m314dl/internal/source"
)

// Options is the subset of the CLI flags that shapes a restream: where to
// listen and what to publish.
type Options struct {
	Addr       string        // listen address (-serve)
	Format     string        // hls | ts | dash (-serve-format)
	Transcode  string        // ffmpeg output args for the ts remux path (-serve-transcode)
	FFmpegPath string        // -ffmpeg
	SpoolDir   string        // -spool-dir; "" means the current directory (see SpoolBase)
	Idle       time.Duration // -serve-idle: release a live source after this long with no viewer; 0 = always on
}

// MinIdle is the smallest idle period accepted. Below one playlist poll
// interval a channel would stop between two polls of a single healthy viewer.
const MinIdle = 5 * time.Second

// MediaPath is the path a format is served at under the channel root.
func MediaPath(format string) string {
	switch format {
	case "ts", "mpegts":
		return "/live.ts"
	case "dash", "mpd":
		return "/live.mpd"
	}
	return "/live.m3u8"
}

// SpoolBase picks the directory that restream spools are created under: the
// -spool-dir override if given, else the current directory, else the system
// temp dir when the current directory is not writable. The current directory
// is the default on purpose. A seekable VOD spools the whole decrypted asset
// there, and on most Linux hosts the system temp dir is tmpfs, a filesystem
// that lives in RAM. A large restream that "spooled to disk" there was filling
// memory until the OOM killer took the process. The current directory is real
// disk in the normal case, including a container's data volume. When the
// chosen location is RAM-backed anyway, SpoolBase warns on stderr but does not
// fail.
func SpoolBase(override string) (string, error) {
	base := override
	if base != "" {
		if err := os.MkdirAll(base, 0o700); err != nil {
			return "", fmt.Errorf("-spool-dir %s: %w", base, err)
		}
	} else {
		base = "."
		if probe, err := os.MkdirTemp(base, ".m314dl-probe-*"); err != nil {
			base = os.TempDir() // cwd not writable: run anyway; warn below if tmpfs
		} else {
			os.Remove(probe)
		}
	}
	if abs, err := filepath.Abs(base); err == nil {
		base = abs
	}
	if ramBacked(base) {
		fmt.Fprintf(os.Stderr, "warning: spool dir %s is RAM-backed (tmpfs); a large VOD restream can exhaust memory — pass -spool-dir <real-disk-path>\n", base)
	}
	return base, nil
}

// Presentation is the small interface the run loop needs from any output
// format: how to end the stream and how to describe its live state.
type Presentation interface {
	End()
	StatusLine() string
}

// Job is one source stream wired to the sink that publishes it. pace slows a
// finite source down to real time. That is needed only where the output has a
// live edge that could be overrun (the TS broadcast); the seekable VOD
// publisher spools to disk and lets players pull at their own rate.
type Job struct {
	st   *manifest.Stream
	sink engine.Sink
	tmp  string
	pace bool
}

// Resolver loads the source again and returns the same selection of streams
// as the first time, for an on-demand restart.
type Resolver func(ctx context.Context) ([]*manifest.Stream, error)

// Run builds the output for the selected streams and hands over to the run
// loop, which blocks until the restream ends (the one-shot `-serve` mode).
// With o.Idle set and a live source, the source runs only while someone is
// watching; resolve is how each restart finds the streams again.
func Run(ctx context.Context, o Options, client *httpx.Client, kind string,
	selected []*manifest.Stream, resolve Resolver, keys map[[16]byte][]byte, bbtsKey []byte,
	threadCeiling int, logv func(string, ...any)) error {

	base, err := SpoolBase(o.SpoolDir)
	if err != nil {
		return err
	}
	tmpDir, err := os.MkdirTemp(base, "m314dl-serve-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	if o.Idle > 0 && isLive(selected) {
		say := func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) }
		start := func(ctx context.Context) (Presentation, http.Handler, []Job, error) {
			sel, err := resolve(ctx)
			if err != nil {
				return nil, nil, nil, err
			}
			pres, h, _, jobs, err := BuildOutputs(o, sel, tmpDir, logv)
			return pres, h, jobs, err
		}
		run := func(ctx context.Context, jobs []Job) error {
			return RunJobs(ctx, client, kind, jobs, keys, bbtsKey, threadCeiling, logv)
		}
		for _, st := range selected {
			fmt.Fprintf(os.Stderr, "restream track %s\n", st)
		}
		return serveOnDemand(ctx, o, NewOnDemand(o.Idle, start, run, say), MediaPath(o.Format))
	}

	pres, handler, path, jobs, err := BuildOutputs(o, selected, tmpDir, logv)
	if err != nil {
		return fmt.Errorf("restream: %w", err)
	}
	for _, j := range jobs {
		fmt.Fprintf(os.Stderr, "restream track %s\n", j.st)
	}
	return serve(ctx, o, client, kind, jobs, pres, handler, path, keys, bbtsKey, threadCeiling, logv)
}

// BuildOutputs turns the selected streams into an output that is ready to run:
// the Presentation (End/StatusLine), the HTTP handler that serves it, the media
// path, and one download job per stream (with part files under tmpDir). It is
// the one place that maps -serve-format onto a packager, and both the one-shot
// `-serve` mode and the multi-channel worker use it. Subtitles are skipped
// (there is no live WebVTT segmentation yet).
func BuildOutputs(o Options, selected []*manifest.Stream, tmpDir string, logv func(string, ...any)) (Presentation, http.Handler, string, []Job, error) {
	if logv == nil {
		logv = func(string, ...any) {}
	}
	var streams []*manifest.Stream
	for _, st := range selected {
		if st.Type == manifest.Subtitles {
			logv("restream: skipping subtitle track %s (not supported yet)", st.ID)
			continue
		}
		// Both -serve and worker mode run this, so both log the selected audio
		// as well as the video. No source URL, headers or content keys are
		// logged here.
		logv("restream: selected track %s: %s", st.ID, st)
		streams = append(streams, st)
	}
	if len(streams) == 0 {
		return nil, nil, "", nil, fmt.Errorf("no video/audio streams selected")
	}

	switch o.Format {
	case "ts", "mpegts":
		if pure := singleMuxedTS(streams); pure != nil && o.Transcode == "" {
			// The source is already a muxed TS: copy the segments straight out,
			// with no ffmpeg involved.
			b := restream.NewTSBroadcaster()
			jobs := []Job{{st: pure, sink: restream.NewTSSink(b), tmp: filepath.Join(tmpDir, "ts"+source.RawExt(pure)), pace: !pure.Live}}
			return b, restream.NewTSServer(b), "/live.ts", jobs, nil
		}
		// An fMP4 source, a separate audio track, or a requested transcode: remux
		// with one long-running ffmpeg that feeds the broadcaster.
		ffmpeg, err := mux.FindFFmpeg(o.FFmpegPath)
		if err != nil {
			return nil, nil, "", nil, fmt.Errorf("ts output needs remuxing but ffmpeg not found (pass -ffmpeg <path>): %w", err)
		}
		var targs []string
		if o.Transcode != "" {
			targs = strings.Fields(o.Transcode)
		}
		b := restream.NewTSBroadcaster()
		rem, err := restream.NewRemuxTS(ffmpeg, len(streams), targs, b, logv)
		if err != nil {
			return nil, nil, "", nil, err
		}
		var jobs []Job
		for i, st := range streams {
			jobs = append(jobs, Job{st: st, sink: rem.Sink(i), tmp: filepath.Join(tmpDir, fmt.Sprintf("in%d", i)+source.RawExt(st)), pace: !st.Live})
		}
		return rem, restream.NewTSServer(b), "/live.ts", jobs, nil
	case "dash", "mpd":
		if err := requireFMP4(streams); err != nil {
			return nil, nil, "", nil, err
		}
		pub, jobs := buildPublisherJobs(streams, tmpDir, false)
		return pub, restream.NewServer(pub).DASHHandler(), "/live.mpd", jobs, nil
	case "", "hls":
		pub, jobs := buildPublisherJobs(streams, tmpDir, true)
		return pub, restream.NewServer(pub).Handler(), "/live.m3u8", jobs, nil
	default:
		return nil, nil, "", nil, fmt.Errorf("-serve-format %q: want hls, ts, or dash", o.Format)
	}
}

// buildPublisherJobs wires each stream to a track in a new Publisher. The HLS
// and DASH paths share it and differ only in the handler.
//
// The track shape follows the source. A live source keeps a bounded rolling
// window in memory, because a real live stream has no full timeline to seek
// in. A finite source is served as a seekable VOD: every segment is decrypted
// and spooled to a file under tmpDir, then served from disk, so the whole
// timeline is seekable while RAM holds only the metadata of each segment.
// Neither shape is paced. Live arrives at its own rhythm, and the VOD spool
// has no live edge to overrun, so players pull from the growing on-disk
// playlist at their own rate.
func buildPublisherJobs(streams []*manifest.Stream, tmpDir string, initChanges bool) (*restream.Publisher, []Job) {
	pub := restream.NewPublisher()
	pub.AllowInitChange = initChanges
	namer := newNamer()
	var jobs []Job
	for _, st := range streams {
		id := namer(st)
		sink := pub.AddTrack(restream.TrackFromStream(id, st, st.Live, tmpDir))
		jobs = append(jobs, Job{st: st, sink: sink, tmp: filepath.Join(tmpDir, id+source.RawExt(st))})
	}
	return pub, jobs
}

func isLive(streams []*manifest.Stream) bool {
	for _, st := range streams {
		if st.Live {
			return true
		}
	}
	return false
}

// listen starts the HTTP server for a restream and turns the first interrupt
// into a cancel of the returned context (a second one exits at once). The
// returned shutdown stops the server with a short grace period.
func listen(ctx context.Context, o Options, handler http.Handler, path string) (context.Context, context.CancelFunc, func(), error) {
	ln, err := net.Listen("tcp", o.Addr)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("restream: listen on %s: %w", o.Addr, err)
	}
	srv := &http.Server{Handler: handler}
	go srv.Serve(ln)
	fmt.Fprintf(os.Stderr, "\nrestreaming live at http://%s%s\n\n", displayAddr(ln.Addr()), path)

	ctx, cancel := context.WithCancel(ctx)
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Fprintln(os.Stderr, "\ninterrupt: stopping restream")
		cancel()
		<-sigCh
		os.Exit(130)
	}()
	shutdown := func() {
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer shutCancel()
		srv.Shutdown(shutCtx)
	}
	return ctx, cancel, shutdown, nil
}

// serveOnDemand publishes an on-demand channel until the operator interrupts
// it. The OnDemand handler starts and stops the source by itself.
func serveOnDemand(ctx context.Context, o Options, od *OnDemand, path string) error {
	ctx, cancel, shutdown, err := listen(ctx, o, od, path)
	if err != nil {
		return err
	}
	defer cancel()
	fmt.Fprintf(os.Stderr, "on demand: the source starts with the first viewer and stops after %s without one\n", o.Idle)
	statusStop := make(chan struct{})
	go statusLoop(od, statusStop)
	<-ctx.Done()
	close(statusStop)
	od.Close()
	shutdown()
	fmt.Fprintln(os.Stderr, "restream stopped")
	return nil
}

// serve runs the download jobs into their sinks while an HTTP server publishes
// the result. It runs until the source ends or the operator interrupts it.
func serve(ctx context.Context, o Options, client *httpx.Client, kind string,
	jobs []Job, pres Presentation, handler http.Handler, path string,
	keys map[[16]byte][]byte, bbtsKey []byte, threadCeiling int, logv func(string, ...any)) error {

	ctx, cancel, shutdown, err := listen(ctx, o, handler, path)
	if err != nil {
		return err
	}
	defer cancel()

	statusStop := make(chan struct{})
	go statusLoop(pres, statusStop)

	// On cancel, end the presentation at once. For the ffmpeg remux path this
	// closes the input pipes, so a download goroutine stuck writing to a stalled
	// ffmpeg gets EPIPE and g.Wait can return. Calling End twice is safe, so the
	// second End after the wait below does no harm.
	go func() {
		<-ctx.Done()
		pres.End()
	}()

	err = RunJobs(ctx, client, kind, jobs, keys, bbtsKey, threadCeiling, logv)
	close(statusStop)
	pres.End()

	// A real download error (not an interrupt) is fatal.
	if err != nil && ctx.Err() == nil {
		shutdown()
		return err
	}
	// A finite source finished publishing on its own. Keep serving the finished
	// stream (the HLS playlist now carries EXT-X-ENDLIST) until the operator
	// interrupts.
	if ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, "source complete — serving until Ctrl-C")
		<-ctx.Done()
	}
	shutdown()
	fmt.Fprintln(os.Stderr, "restream stopped")
	return nil
}

// RunJobs downloads every job's stream into its sink until the sources end or
// ctx is cancelled. It is the run loop that the one-shot `-serve` mode and the
// worker's per-channel goroutine share.
func RunJobs(ctx context.Context, client *httpx.Client, kind string, jobs []Job,
	keys map[[16]byte][]byte, bbtsKey []byte, threadCeiling int, logv func(string, ...any)) error {

	g, gctx := errgroup.WithContext(ctx)
	for _, j := range jobs {
		j := j
		g.Go(func() error {
			cfg := engine.Config{
				Client: client, Threads: threadCeiling, Keys: keys, BBTSKey: bbtsKey,
				Verbose: logv, Sink: j.sink,
				Warn: func(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) },
				// A finite TS broadcast source downloads as fast as it can. Pace it
				// to real time so it does not flood the viewers' buffers. The
				// publisher paths spool to disk instead and are never paced.
				PaceRealtime: j.pace,
			}
			refresh := source.RefreshFunc(client, kind, j.st, logv)
			if !j.st.Live {
				refresh = nil // finite source: publish it once, then finish
			}
			if err := engine.DownloadStream(gctx, cfg, j.st, j.tmp, refresh); err != nil {
				return fmt.Errorf("track %s: %w", j.st.ID, err)
			}
			return nil
		})
	}
	return g.Wait()
}

func statusLoop(pres Presentation, stop <-chan struct{}) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
			if line := pres.StatusLine(); line != "" {
				fmt.Fprintf(os.Stderr, "live: %s\n", line)
			}
		}
	}
}

// singleMuxedTS returns the one muxed TS video track that the pure Go path can
// copy straight out. It returns nil when the selection needs FFmpeg to remux
// (an fMP4 source, or a separate audio track to mux in).
func singleMuxedTS(streams []*manifest.Stream) *manifest.Stream {
	if len(streams) != 1 {
		return nil // separate audio (or more tracks) must be muxed together, so remux
	}
	st := streams[0]
	if st.Type != manifest.Video && st.Type != manifest.Unknown {
		return nil
	}
	if st.Init != nil || segmentIsFMP4TS(st) {
		return nil // fMP4 has to be remuxed to TS
	}
	return st
}

// requireFMP4 rejects a DASH output selection that contains a TS track. DASH
// segments must be fMP4, and remuxing TS to fMP4 is left for a later phase.
func requireFMP4(streams []*manifest.Stream) error {
	for _, st := range streams {
		if st.Init == nil && !segmentIsFMP4TS(st) {
			return fmt.Errorf("restream -serve-format dash: track %s is MPEG-TS, not fMP4; DASH output needs an fMP4 source (TS→fMP4 remux is a later phase)", st.ID)
		}
	}
	return nil
}

func segmentIsFMP4TS(st *manifest.Stream) bool {
	if len(st.Segments) == 0 {
		return false
	}
	u := st.Segments[0].URL
	return strings.Contains(u, ".m4s") || strings.Contains(u, ".cmf")
}

// newNamer returns a function that gives each track a short, unique, URL-safe
// id: "video", "video2", "audio", "audio-fr" and so on. Readable ids make the
// playlist URLs explain themselves.
func newNamer() func(*manifest.Stream) string {
	used := map[string]int{}
	return func(st *manifest.Stream) string {
		base := st.Type.String()
		if base == "subtitles" {
			base = "sub"
		}
		if st.Type != manifest.Video && st.Language != "" {
			base += "-" + source.SanitizeTag(st.Language)
		}
		used[base]++
		if used[base] == 1 {
			return base
		}
		return fmt.Sprintf("%s%d", base, used[base])
	}
}

// displayAddr turns a listener address into one that can be clicked, with a
// wildcard or empty host replaced by localhost.
func displayAddr(addr net.Addr) string {
	s := addr.String()
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return s
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "localhost"
	}
	return net.JoinHostPort(host, port)
}
