// m314dl — HLS/DASH/MSS media downloader.
//
// Usage: m314dl [flags] <URL>
// URL may be a master/media playlist (.m3u8), a DASH manifest (.mpd), a Smooth
// Streaming manifest (.ism/Manifest), a web page (m314dl scrapes it for
// stream URLs), or a plain file (mp4, mkv, zip, iso, …), which is fetched in
// parallel byte ranges with resume and written as-is.
package main

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/m314dev/m314dl/internal/curl"
	"github.com/m314dev/m314dl/internal/engine"
	"github.com/m314dev/m314dl/internal/hls"
	"github.com/m314dev/m314dl/internal/httpx"
	"github.com/m314dev/m314dl/internal/manifest"
	"github.com/m314dev/m314dl/internal/mp4"
	"github.com/m314dev/m314dl/internal/mux"
	"github.com/m314dev/m314dl/internal/pick"
	"github.com/m314dev/m314dl/internal/rpc"
	"github.com/m314dev/m314dl/internal/serve"
	"github.com/m314dev/m314dl/internal/source"
	"github.com/m314dev/m314dl/internal/subs"
	"github.com/m314dev/m314dl/internal/update"
	"github.com/m314dev/m314dl/internal/worker"
)

var version = "0.3.6" // set by goreleaser via -ldflags "-X main.version=..."

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ", ") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

type helpOption struct {
	name  string
	value string
}

type options struct {
	output           string
	threads          int
	multiTracks      bool
	headers          multiFlag
	curl             string
	cookies          string
	proxy            string
	insecure         bool
	retries          int
	noUpdateCheck    bool
	listOnly         bool
	sv, sa, ss       string
	adKeywords       multiFlag
	appendURLParams  bool
	liveLimit        time.Duration
	liveFromStart    bool
	noMux            bool
	keepTemp         bool
	subFormat        string
	subExternal      bool
	ffmpegPath       string
	verbose          bool
	timeout          time.Duration
	progressInterval time.Duration
	keys             multiFlag
	bbtsKey          string
	rpc              string
	rpcSecret        string
	rpcMaxJobs       int
	rpcRetain        time.Duration
	serve            string
	serveFormat      string
	serveTranscode   string
	spoolDir         string
	worker           string
	workerSecret     string
	workerMaxChan    int
	pick             string
	titles           int
	group            string
	match            string
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "m314dl: error: %v\n", err)
		os.Exit(1)
	}
}

// printUsage keeps the command-line help short enough to scan. The standard
// flag package prints one long alphabetical list, which hides the options most
// people need for a normal download among the server-only settings.
func printUsage(w io.Writer) {
	fmt.Fprintf(w, "m314dl %s — HLS/DASH/MSS media downloader\n\n", version)
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  m314dl [options] URL")
	fmt.Fprintln(w, "  m314dl -serve ADDRESS [options] URL")
	fmt.Fprintln(w, "  m314dl -rpc ADDRESS [server options]")
	fmt.Fprintln(w, "  m314dl -worker ADDRESS [server options]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "URL can be an HLS/DASH/Smooth playlist, a web page containing one, a normal file,")
	fmt.Fprintln(w, "or an IPTV M3U list (URL or local file) whose entries are downloaded one by one.")
	fmt.Fprintln(w, "Flags may go before or after the URL.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Examples:")
	fmt.Fprintln(w, "  m314dl URL")
	fmt.Fprintln(w, "      Download the best video, its preferred audio, and subtitles.")
	fmt.Fprintln(w, "  m314dl -o movie.mp4 -t 16 URL")
	fmt.Fprintln(w, "      Save as movie.mp4 and use up to 16 segment downloads per track.")
	fmt.Fprintln(w, "  m314dl -mt -sv best -sa 'lang=^en' URL")
	fmt.Fprintln(w, "      Download selected tracks at the same time, with English audio when available.")
	fmt.Fprintln(w, "  m314dl -list URL")
	fmt.Fprintln(w, "      Show available video, audio, and subtitle tracks without downloading.")
	fmt.Fprintln(w, "  m314dl -o Movies -group 'movies' -match 'batman' -t 1 playlist.m3u")
	fmt.Fprintln(w, "      Download the matching titles of an M3U list into Movies/<group>/<title>.")
	fmt.Fprintln(w)

	printUsageSection(w, "Basic download", []helpOption{
		{"o", "FILE"}, {"t", "N"}, {"mt", ""}, {"list", ""},
		{"no-mux", ""}, {"keep", ""}, {"v", ""},
	})
	printUsageSection(w, "Choose tracks and live recording", []helpOption{
		{"sv", "SELECTOR"}, {"sa", "SELECTOR"}, {"ss", "SELECTOR"},
		{"ad-keyword", "REGEX"}, {"live-duration", "DURATION"}, {"live-from-start", ""},
	})
	printUsageSection(w, "M3U playlists (IPTV lists: one download per entry)", []helpOption{
		{"pick", "LIST"}, {"group", "REGEX"}, {"match", "REGEX"}, {"titles", "N"},
	})
	printUsageSection(w, "Input and network", []helpOption{
		{"curl", "SOURCE"}, {"H", "HEADER"}, {"cookies", "FILE"}, {"proxy", "URL"},
		{"insecure", ""}, {"retries", "N"}, {"timeout", "DURATION"}, {"progress-interval", "DURATION"},
	})
	printUsageSection(w, "Decryption and subtitles", []helpOption{
		{"key", "KID:KEY"}, {"bbts-key", "HEX_KEY"}, {"sub-format", "srt|vtt"},
		{"sub-external", ""}, {"ffmpeg", "PATH"},
	})
	printUsageSection(w, "Restream a selected presentation", []helpOption{
		{"serve", "ADDRESS"}, {"serve-format", "hls|ts|dash"},
		{"serve-transcode", "FFMPEG_ARGS"}, {"spool-dir", "DIR"},
	})
	printUsageSection(w, "Run as a server", []helpOption{
		{"rpc", "ADDRESS"}, {"rpc-secret", "TOKEN"}, {"rpc-max-jobs", "N"}, {"rpc-retain", "DURATION"},
		{"worker", "ADDRESS"}, {"worker-secret", "TOKEN"}, {"worker-max-channels", "N"},
	})
	printUsageSection(w, "Other", []helpOption{{"no-update-check", ""}, {"version", ""}})
}

func printUsageSection(w io.Writer, title string, options []helpOption) {
	fmt.Fprintf(w, "%s:\n", title)
	for _, option := range options {
		f := flag.Lookup(option.name)
		if f == nil { // Keep help usable if a flag is removed later.
			continue
		}
		label := "-" + f.Name
		if option.value != "" {
			label += " " + option.value
		}
		fmt.Fprintf(w, "  %-29s %s%s\n", label, f.Usage, usageDefault(f))
	}
	fmt.Fprintln(w)
}

func usageDefault(f *flag.Flag) string {
	// Empty values, false, and zero normally have special meanings described in
	// the option text, so repeating them as a default would add noise.
	if f.DefValue == "" || f.DefValue == "false" || f.DefValue == "0" || f.DefValue == "0s" {
		return ""
	}
	return " (default: " + f.DefValue + ")"
}

func run() error {
	var o options
	flag.StringVar(&o.output, "o", "", "output file (extension selects container; default from URL, .mp4). Direct file: used as given, default the server's/URL's filename. M3U playlist: output directory")
	flag.IntVar(&o.threads, "t", 0, "concurrent segment downloads per stream (direct file: parallel connections) — a fixed count, held (backs off only on rate limits, then climbs back). Omit to auto-tune (up to 64)")
	flag.BoolVar(&o.multiTracks, "mt", false, "download selected tracks concurrently (default: one track at a time)")
	flag.Var(&o.headers, "H", "custom header 'Key: Value' (repeatable)")
	flag.StringVar(&o.curl, "curl", "", "take the URL and headers from a copy-as-curl command: a file path, '-' for stdin, or the command itself. For a browser copy-as-curl use a file or stdin — its quotes break an inline shell argument. -H overrides its headers")
	flag.BoolVar(&o.noUpdateCheck, "no-update-check", false, "don't check GitHub for a newer release (also M314DL_NO_UPDATE_CHECK=1)")
	flag.StringVar(&o.cookies, "cookies", "", "Netscape cookies.txt file")
	flag.StringVar(&o.proxy, "proxy", "", "proxy URL (http://, socks5://, user:pass@ ok)")
	flag.BoolVar(&o.insecure, "insecure", false, "skip TLS certificate verification")
	flag.IntVar(&o.retries, "retries", 5, "retries per request (covers mid-body failures)")
	flag.BoolVar(&o.listOnly, "list", false, "list streams (or M3U playlist entries) and exit")
	flag.StringVar(&o.pick, "pick", "", "M3U playlist: download only these entry numbers from -list, e.g. 3,7-12")
	flag.IntVar(&o.titles, "titles", 1, "M3U playlist: titles downloaded at the same time (each uses up to -t connections)")
	flag.StringVar(&o.group, "group", "", "M3U playlist: download only entries whose group-title matches this regex (case-insensitive)")
	flag.StringVar(&o.match, "match", "", "M3U playlist: download only entries whose title matches this regex (case-insensitive)")
	flag.StringVar(&o.sv, "sv", "best", "video select: best|worst|all|bestN|key=regex[:...] (keys: id,lang,name,codecs,res,bwmin,bwmax)")
	flag.StringVar(&o.sa, "sa", "", "audio select (default: best per language, video's group preferred)")
	flag.StringVar(&o.ss, "ss", "", "subtitle select (default: all)")
	flag.Var(&o.adKeywords, "ad-keyword", "regex; matching segment URLs are skipped as ads (repeatable, live too)")
	flag.BoolVar(&o.appendURLParams, "append-url-params", false, "carry the input/playlist URL's query string onto relative child URLs (variant playlists, segments, keys) that have none — for CDNs that sign the master with a path-scoped token and 403 the children without it")
	flag.DurationVar(&o.liveLimit, "live-duration", 0, "stop live recording after this duration (e.g. 1h30m)")
	flag.BoolVar(&o.liveFromStart, "live-from-start", false, "live: download the whole DVR window instead of starting at the live edge")
	flag.BoolVar(&o.noMux, "no-mux", false, "keep raw per-stream files, skip ffmpeg")
	flag.StringVar(&o.ffmpegPath, "ffmpeg", "", "path to the ffmpeg binary (default: next to m314dl, then PATH)")
	flag.BoolVar(&o.keepTemp, "keep", false, "keep temp stream files after mux")
	flag.Var(&o.keys, "key", "CENC content key 'KID:KEY' (hex; KID dashes optional) or bare 'KEY' (repeatable). Enables native in-process DRM decryption — no mp4decrypt needed")
	flag.StringVar(&o.bbtsKey, "bbts-key", "", "16-byte AES key (32 hex) for BBTS-encrypted MPEG-TS segments; the per-segment IV is read from the stream's SDT")
	flag.StringVar(&o.subFormat, "sub-format", "srt", "subtitle output: srt or vtt")
	flag.BoolVar(&o.subExternal, "sub-external", false, "write subtitles as sidecar files next to the output instead of muxing them in")
	flag.BoolVar(&o.verbose, "v", false, "verbose logging")
	flag.DurationVar(&o.timeout, "timeout", 0, "per-request timeout (default none; retries handle stalls)")
	flag.DurationVar(&o.progressInterval, "progress-interval", 0, "progress refresh interval, e.g. 500ms (default: 1s on a TTY, 5s when piped)")
	flag.StringVar(&o.rpc, "rpc", "", "run as an RPC server on this address (e.g. 127.0.0.1:8314) instead of downloading; see internal/rpc for the HTTP/JSON API")
	flag.StringVar(&o.rpcSecret, "rpc-secret", "", "bearer token for -rpc clients (required when binding a non-loopback address)")
	flag.IntVar(&o.rpcMaxJobs, "rpc-max-jobs", 64, "-rpc: max concurrent jobs; further /add requests get 503 until a slot frees (0 = unlimited)")
	flag.DurationVar(&o.rpcRetain, "rpc-retain", time.Hour, "-rpc: keep finished jobs queryable at least this long before reaping (bounds memory)")
	flag.StringVar(&o.serve, "serve", "", "restream: republish the selected streams live on this address (e.g. :8314) instead of downloading to a file")
	flag.StringVar(&o.serveFormat, "serve-format", "hls", "restream output: hls (/live.m3u8), ts (continuous MPEG-TS at /live.ts), or dash (/live.mpd; needs an fMP4 source)")
	flag.StringVar(&o.serveTranscode, "serve-transcode", "", "restream ts remux: ffmpeg output codec args to transcode instead of copy, e.g. '-c:v libx264 -preset veryfast -c:a aac' (implies the ffmpeg remux path)")
	flag.StringVar(&o.spoolDir, "spool-dir", "", "restream (-serve/-worker): directory for spooled segments and temp part files (default: current directory). Must be real disk — a seekable VOD spools the whole asset here, and /tmp is usually RAM-backed tmpfs on Linux")
	flag.StringVar(&o.worker, "worker", "", "run as a multi-channel restream worker on this address (e.g. :7001); drive it via POST /api/channels, serve /{id}/live.m3u8. See internal/worker")
	flag.StringVar(&o.workerSecret, "worker-secret", "", "bearer token for -worker control API (required when binding a non-loopback address)")
	flag.IntVar(&o.workerMaxChan, "worker-max-channels", 32, "-worker: max concurrent channels; further starts get 503 (0 = unlimited)")
	showVersion := flag.Bool("version", false, "print version")
	flag.Usage = func() { printUsage(flag.CommandLine.Output()) }
	// Flags may appear in any order relative to the URL. Go's flag package stops
	// at the first non-flag arg, so parse in a loop: keep the positional it stops
	// on and resume parsing the rest. Flags before, after, or around the URL all
	// work (m314dl -o out.mp4 URL -t 16 is the same as m314dl URL -o out.mp4 -t 16).
	var positionals []string
	rest := os.Args[1:]
	for {
		flag.CommandLine.Parse(rest)
		rest = flag.Args()
		if len(rest) == 0 {
			break
		}
		positionals = append(positionals, rest[0])
		rest = rest[1:]
	}
	if *showVersion {
		fmt.Println("m314dl", version)
		return nil
	}
	hls.AppendURLParams = o.appendURLParams
	if o.rpc != "" {
		if len(positionals) != 0 {
			return fmt.Errorf("-rpc takes no URL; submit jobs via POST /add")
		}
		return rpc.ServeRPC(o.rpc, o.rpcSecret, o.rpcMaxJobs, o.rpcRetain, version)
	}
	if o.worker != "" {
		if len(positionals) != 0 {
			return fmt.Errorf("-worker takes no URL; start channels via POST /api/channels")
		}
		logv := func(format string, args ...any) {
			if o.verbose {
				fmt.Fprintf(os.Stderr, "[v] "+format+"\n", args...)
			}
		}
		return worker.ServeWorker(o.worker, o.workerSecret, o.workerMaxChan, o.ffmpegPath, o.spoolDir, version, logv)
	}
	// Adaptive concurrency unless the user pinned -t.
	tPinned := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "t" {
			tPinned = true
		}
	})
	// -curl: pull the URL and headers out of a pasted copy-as-curl command. An
	// explicit positional URL still wins; -H below still overrides its headers.
	var curlHeaders map[string]string
	if o.curl != "" {
		cmd, err := readCurlSource(o.curl)
		if err != nil {
			return fmt.Errorf("-curl: %w", err)
		}
		u, h, err := curl.Parse(cmd)
		if err != nil {
			return fmt.Errorf("-curl: %w", err)
		}
		curlHeaders = h
		if len(positionals) == 0 {
			positionals = []string{u}
		}
	}
	if len(positionals) != 1 {
		flag.Usage()
		return fmt.Errorf("exactly one URL required (or -curl)")
	}
	inputURL := positionals[0]

	// Nudge the user if a newer release exists. Runs in the background and its
	// notice prints when run() returns, so it never delays the download.
	defer printUpdateNotice(startUpdateCheck(&o))

	logv := func(format string, args ...any) {
		if o.verbose {
			fmt.Fprintf(os.Stderr, "[v] "+format+"\n", args...)
		}
	}

	headers := map[string]string{}
	for k, v := range curlHeaders { // from -curl, if any
		headers[k] = v
	}
	for _, h := range o.headers { // explicit -H overrides a curl header
		k, v, ok := strings.Cut(h, ":")
		if !ok {
			return fmt.Errorf("bad header %q (want 'Key: Value')", h)
		}
		headers[http.CanonicalHeaderKey(strings.TrimSpace(k))] = strings.TrimSpace(v)
	}
	client, err := httpx.New(httpx.Options{
		Headers: headers, Proxy: o.proxy, CookieFile: o.cookies,
		Insecure: o.insecure, Timeout: o.timeout, Retries: o.retries,
	})
	if err != nil {
		return err
	}

	var adFilters []*regexp.Regexp
	for _, k := range o.adKeywords {
		re, err := regexp.Compile(k)
		if err != nil {
			return fmt.Errorf("bad -ad-keyword %q: %w", k, err)
		}
		adFilters = append(adFilters, re)
	}

	keys, err := source.ParseKeys(o.keys)
	if err != nil {
		return err
	}

	var bbtsKey []byte
	if o.bbtsKey != "" {
		bbtsKey, err = hex.DecodeString(strings.TrimSpace(o.bbtsKey))
		if err != nil || len(bbtsKey) != 16 {
			return fmt.Errorf("bad -bbts-key: must be 32 hex chars (16 bytes)")
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// fetch + detect manifest (scraping web pages transparently)
	master, kind, err := source.LoadManifest(ctx, client, inputURL, logv)
	if err != nil {
		return err
	}
	threadCeiling := 0 // 0 = auto-tune (no -t given)
	if tPinned {
		threadCeiling = o.threads
	}
	j := job{o: &o, client: client, threads: threadCeiling, adFilters: adFilters, keys: keys, bbtsKey: bbtsKey, logv: logv}
	if kind == source.PlaylistKind {
		return downloadPlaylist(ctx, cancel, j, master)
	}
	return j.download(ctx, cancel, inputURL, master, kind)
}

// job is what a download needs besides its URL: settled once per run and
// shared by every entry of an M3U playlist.
type job struct {
	o         *options
	client    *httpx.Client
	threads   int // -t ceiling; 0 = auto-tune
	adFilters []*regexp.Regexp
	keys      map[[16]byte][]byte
	bbtsKey   []byte
	logv      func(string, ...any)
	skipLive  bool             // playlist entry: a live stream is skipped (errLiveEntry), not recorded
	prog      *engine.Progress // playlist -titles >1: one status line shared by the titles in flight
}

// say prints a status message, through the shared status line when there is one.
func (j job) say(format string, args ...any) {
	if j.prog != nil {
		j.prog.Log(fmt.Sprintf(format, args...))
		return
	}
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}

// progress is the status line for one download: its own, drawn until stop is
// called, or the playlist's shared one, which the playlist stops.
func (j job) progress(live bool) (prog *engine.Progress, stop func()) {
	if j.prog != nil {
		return j.prog, func() {}
	}
	prog = engine.NewProgress(live, j.o.progressInterval)
	done := make(chan struct{})
	go prog.Render(done)
	return prog, func() {
		close(done)
		time.Sleep(50 * time.Millisecond) // let renderer print the final line
	}
}

// download runs one loaded input to its output: a direct file as-is, a
// manifest through selection, download, and mux.
func (j job) download(ctx context.Context, cancel context.CancelFunc, inputURL string, master *manifest.Master, kind string) error {
	o, client, threadCeiling, logv := j.o, j.client, j.threads, j.logv
	keys, bbtsKey, adFilters := j.keys, j.bbtsKey, j.adFilters
	stopLive := make(chan struct{})
	if kind == source.FileKind {
		return j.downloadFile(ctx, cancel, master.Streams[0])
	}

	ve, err := pick.ParseExpr(o.sv)
	if err != nil {
		return fmt.Errorf("-sv: %w", err)
	}
	var ae, se *pick.Expr
	if o.sa != "" {
		if ae, err = pick.ParseExpr(o.sa); err != nil {
			return fmt.Errorf("-sa: %w", err)
		}
	}
	if o.ss != "" {
		if se, err = pick.ParseExpr(o.ss); err != nil {
			return fmt.Errorf("-ss: %w", err)
		}
	}

	if o.listOnly {
		pick.Sort(master.Streams)
		for _, st := range master.Streams {
			fmt.Printf("%-4s %s\n", st.ID, st)
		}
		return nil
	}

	selected := pick.Select(master.Streams, ve, ae, se)
	if len(selected) == 0 {
		return fmt.Errorf("no streams matched the selection")
	}
	for _, st := range selected {
		j.say("selected: %-4s %s", st.ID, st)
	}

	// expand HLS media playlists for selected streams only
	live := master.Live
	for _, st := range selected {
		if kind == "hls" && !st.SegmentsFull {
			body, finalURL, err := client.FetchBytes(ctx, st.PlaylistURL, "")
			if err != nil {
				return fmt.Errorf("fetch media playlist for %s: %w", st.ID, err)
			}
			st.PlaylistURL = finalURL
			if err := hls.ParseMedia(body, finalURL, st); err != nil {
				return err
			}
		}
		if kind == "dash" {
			if err := source.ExpandIndexed(ctx, client, st, logv); err != nil {
				return err
			}
		}
		if st.Live {
			live = true
		}
		if len(st.Segments) == 0 {
			return fmt.Errorf("stream %s has no segments", st.ID)
		}
		if k := st.Segments[0].Key; k != nil && k.Method == manifest.EncCENC && len(keys) == 0 {
			return fmt.Errorf("stream %s is CENC/DRM-protected; supply the content key with -key KID:KEY", st.ID)
		}
	}
	if live && j.skipLive {
		return errLiveEntry
	}

	// Restream mode: republish live HLS over HTTP instead of downloading a file.
	if o.serve != "" {
		return serve.Run(ctx, serve.Options{Addr: o.serve, Format: o.serveFormat, Transcode: o.serveTranscode, FFmpegPath: o.ffmpegPath, SpoolDir: o.spoolDir},
			client, kind, selected, keys, bbtsKey, threadCeiling, logv)
	}

	outPath := outputPath(o.output, inputURL, live)
	workDir := filepath.Dir(outPath)
	ffmpeg, ffErr := mux.FindFFmpeg(o.ffmpegPath)
	if !o.noMux && ffErr != nil {
		return fmt.Errorf("ffmpeg not found (needed for muxing; pass -ffmpeg <path>, or -no-mux to skip): %w", ffErr)
	}

	interruptHandler(live, stopLive, cancel)

	prog, stopProg := j.progress(live)

	cfg := engine.Config{
		Client: client, Threads: threadCeiling, Keys: keys, BBTSKey: bbtsKey, AdFilters: adFilters,
		LiveLimit: o.liveLimit, Progress: prog, Verbose: logv, Stop: stopLive,
		FromStart: o.liveFromStart,
	}

	type done struct {
		st     *manifest.Stream
		path   string
		failed bool // a subtitle whose download failed — skipped, not fatal
	}
	var results []done
	for _, st := range selected {
		results = append(results, done{st: st, path: engine.TempStreamPath(outPath, st, source.RawExt(st))})
	}
	// Stream scheduling. A stream's segments are latency-bound when small
	// (subtitles: thousands of ~byte-sized segments) and bandwidth-bound when
	// large (video). With only a few streams running at once, the many tiny
	// subtitle streams serialize into batches and their round-trip latency piles
	// up *after* the video finishes — a 5–6s tail on a 41-min asset. So run many
	// streams concurrently, but cap each subtitle stream to a few workers: a
	// handful hides its latency, and the cap keeps total in-flight requests
	// bounded (streamPar × workers) even with 25 subtitle tracks and a big -t.
	const subWorkerCap = 16
	streamPar := 1
	if o.multiTracks {
		streamPar = len(results)
		if streamPar < 4 {
			streamPar = 4
		} else if streamPar > 16 {
			streamPar = 16
		}
	}

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(streamPar)
	tDownload := time.Now()
	for i := range results {
		r := &results[i]
		stCfg := cfg // per-stream copy (shares Client/Progress pointers)
		if r.st.Type == manifest.Subtitles && (stCfg.Threads == 0 || stCfg.Threads > subWorkerCap) {
			stCfg.Threads = subWorkerCap
		}
		g.Go(func() error {
			refresh := source.RefreshFunc(client, kind, r.st, logv)
			if !r.st.Live {
				refresh = nil
			}
			if err := engine.DownloadStream(gctx, stCfg, r.st, r.path, refresh); err != nil {
				// Subtitles are optional: a broken track (Disney+ ships dummy
				// placeholder subtitle tracks, some CDNs 404 a language) must not
				// sink a finished video+audio download. Skip it with a warning.
				// Video/audio failures stay fatal — there's no output without them.
				if r.st.Type == manifest.Subtitles {
					r.failed = true
					j.say("warning: subtitle %s failed to download (%v); skipping it", r.st.ID, err)
					return nil
				}
				return fmt.Errorf("stream %s: %w", r.st.ID, err)
			}
			return nil
		})
	}
	err = g.Wait()
	logv("phase download: %s (%d streams)", time.Since(tDownload).Round(time.Millisecond), len(results))
	tFinalize := time.Now()
	stopProg()
	if err != nil {
		return err
	}

	// subtitles: normalize raw payloads into srt/vtt
	var muxInputs []mux.Input
	var sidecars []string             // -sub-external: files written beside output
	wentExternal := map[string]bool{} // raw paths whose subtitle became a sidecar
	subSidecars := map[string]bool{}  // sidecar paths already used (collision guard)
	keepRaw := map[string]bool{}      // conversion/placement failures must remain recoverable
	for _, r := range results {
		if r.failed {
			continue // subtitle that failed to download — already warned
		}
		if r.st.Type != manifest.Subtitles {
			muxInputs = append(muxInputs, mux.Input{
				Path: r.path, Type: r.st.Type, Language: r.st.Language,
				Name: r.st.Name, Default: r.st.Default,
			})
			continue
		}
		subPath, err := convertSubtitle(r.path, o.subFormat, ffmpeg)
		if err != nil {
			keepRaw[r.path] = true
			j.say("warning: subtitle %s conversion failed (%v); keeping raw file %s", r.st.ID, err, r.path)
			continue
		}
		if o.subExternal {
			sidecar := sidecarSubPath(outPath, r.st, o.subFormat, subSidecars)
			subSidecars[sidecar] = true
			if err := os.Rename(subPath, sidecar); err != nil {
				keepRaw[r.path] = true
				j.say("warning: could not place sidecar subtitle (%v); keeping %s", err, subPath)
				continue
			}
			sidecars = append(sidecars, sidecar)
			wentExternal[r.path] = true
			if !o.keepTemp {
				os.Remove(r.path) // raw payload superseded by the sidecar
			}
			j.say("subtitle: %s", sidecar)
			continue
		}
		muxInputs = append(muxInputs, mux.Input{
			Path: subPath, Type: manifest.Subtitles, Language: r.st.Language,
			Name: r.st.Name, Default: r.st.Default,
		})
	}
	logv("phase subtitle-convert: %s", time.Since(tFinalize).Round(time.Millisecond))

	if o.noMux {
		j.say("done (raw streams kept):")
		for _, r := range results {
			if !wentExternal[r.path] {
				j.say("  %s", r.path)
			}
		}
		for _, s := range sidecars {
			j.say("  %s", s)
		}
		return nil
	}

	// Nothing to mux (e.g. subtitles-only with -sub-external): the sidecars are
	// already written, so finish without invoking ffmpeg.
	if len(muxInputs) == 0 {
		if len(sidecars) == 0 {
			return fmt.Errorf("no streams selected to write")
		}
		j.say("done (subtitles only)")
		return nil
	}

	tMux := time.Now()
	if err := mux.Mux(ffmpeg, muxInputs, outPath); err != nil {
		j.say("mux failed; raw stream files kept in %s", workDir)
		return err
	}
	logv("phase mux: %s (%d inputs)", time.Since(tMux).Round(time.Millisecond), len(muxInputs))
	if !o.keepTemp {
		var temporary []string
		for _, in := range muxInputs {
			temporary = append(temporary, in.Path)
		}
		for _, r := range results { // raw subtitle payloads too
			temporary = append(temporary, r.path)
		}
		cleanupTempFiles(temporary, keepRaw)
	}
	j.say("done: %s", outPath)
	return nil
}

// cleanupTempFiles removes only superseded downloads, never raw subtitles
// whose conversion failed (the warning promises those files are recoverable).
func cleanupTempFiles(paths []string, preserve map[string]bool) {
	for _, path := range paths {
		if !preserve[path] {
			os.Remove(path)
		}
	}
}

// interruptHandler makes the first Ctrl-C/SIGTERM stop the download cleanly — a
// live recording finishes up (stopLive closed), a VOD cancels so the next run
// resumes — and a second one abort outright. Installed once per process: a
// playlist installs it for the whole batch, and its entries' calls are no-ops.
func interruptHandler(live bool, stopLive chan struct{}, cancel context.CancelFunc) {
	interruptOnce.Do(func() { installInterruptHandler(live, stopLive, cancel) })
}

var interruptOnce sync.Once

func installInterruptHandler(live bool, stopLive chan struct{}, cancel context.CancelFunc) {
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		if live {
			// live: stop discovering, drain pipeline, mux what we have
			fmt.Fprintln(os.Stderr, "\ninterrupt: finishing recording (press again to abort)")
			close(stopLive)
		} else {
			// VOD: cancel now; resume state lets the next run continue
			fmt.Fprintln(os.Stderr, "\ninterrupt: stopping (rerun the same command to resume)")
			cancel()
		}
		<-sigCh
		fmt.Fprintln(os.Stderr, "aborted")
		cancel()
		os.Exit(130)
	}()
}

// downloadFile writes a direct file straight to its output path: no stream
// selection, no temp file, no mux — the bytes are the deliverable. -o is used
// as given (no container extension is forced); otherwise the server's or the
// URL's filename, extension and all.
func (j job) downloadFile(ctx context.Context, cancel context.CancelFunc, st *manifest.Stream) error {
	o := j.o
	outPath := o.output
	if outPath == "" {
		outPath = st.Name
	}
	size := "size unknown"
	if n := source.FileSize(st); n >= 0 {
		size = fmt.Sprintf("%d bytes", n)
	}
	if o.listOnly {
		fmt.Printf("file %s (%s)\n", st.Name, size)
		return nil
	}
	j.say("file: %s (%s)", outPath, size)
	interruptHandler(false, nil, cancel)
	prog, stopProg := j.progress(false)
	err := engine.DownloadFile(ctx, engine.Config{Client: j.client, Threads: j.threads, Progress: prog, Verbose: j.logv}, st, outPath)
	stopProg()
	if err != nil {
		return err
	}
	j.say("done: %s", outPath)
	return nil
}

// sidecarSubPath builds "<output-basename>.<lang>.<fmt>" next to the output,
// disambiguating with the stream name/ID when a language repeats.
func sidecarSubPath(outPath string, st *manifest.Stream, format string, used map[string]bool) string {
	base := strings.TrimSuffix(outPath, filepath.Ext(outPath))
	tag := st.Language
	if tag == "" {
		tag = "sub"
	}
	cand := fmt.Sprintf("%s.%s.%s", base, tag, format)
	if used[cand] {
		extra := st.Name
		if extra == "" {
			extra = st.ID
		}
		cand = fmt.Sprintf("%s.%s.%s.%s", base, tag, source.SanitizeTag(extra), format)
	}
	return cand
}

// startUpdateCheck kicks off a background GitHub version check unless disabled,
// returning a channel that yields a newer tag (or "" / nil when off).
func startUpdateCheck(o *options) <-chan string {
	if o.noUpdateCheck || os.Getenv("M314DL_NO_UPDATE_CHECK") != "" {
		return nil
	}
	dir, err := os.UserCacheDir()
	if err != nil {
		return nil
	}
	ch := make(chan string, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ch <- update.Check(ctx, version, dir)
	}()
	return ch
}

// printUpdateNotice prints the "new version" line if the check found one, but
// waits only briefly so a slow network never holds up exit — a check that
// hasn't finished simply reports next run.
func printUpdateNotice(ch <-chan string) {
	if ch == nil {
		return
	}
	select {
	case tag := <-ch:
		if tag != "" {
			fmt.Fprintf(os.Stderr, "\nA newer version %s is available (you have %s): %s\n", tag, version, update.Page)
		}
	case <-time.After(300 * time.Millisecond):
	}
}

// readCurlSource resolves the -curl value: "-" reads stdin, an existing file is
// read, and anything else is treated as the curl command itself (so a shell
// heredoc, a saved file, or an inline string all work).
func readCurlSource(v string) (string, error) {
	if v == "-" {
		b, err := io.ReadAll(os.Stdin)
		return string(b), err
	}
	if b, err := os.ReadFile(v); err == nil {
		return string(b), nil
	}
	return v, nil
}

func outputPath(out, inputURL string, live bool) string {
	if out != "" {
		if filepath.Ext(out) == "" {
			out += ".mp4"
		}
		return out
	}
	name := "m314dl-output"
	if u, err := url.Parse(inputURL); err == nil {
		base := filepath.Base(u.Path)
		base = strings.TrimSuffix(base, filepath.Ext(base))
		if base != "" && base != "/" && base != "." {
			name = base
		}
	}
	if len(name) > 120 {
		name = name[:120]
	}
	if live {
		name += "-" + time.Now().Format("20060102-150405")
	}
	return name + ".mp4"
}

// convertSubtitle normalizes a raw subtitle stream file to srt/vtt.
func convertSubtitle(rawPath, format, ffmpeg string) (string, error) {
	b, err := os.ReadFile(rawPath)
	if err != nil {
		return "", err
	}
	outPath := strings.TrimSuffix(rawPath, ".rawsub") + "." + format
	kind := subs.Sniff(b)
	var cues []subs.Cue
	if kind == subs.KindFMP4 {
		cues, err = subs.ParseWVTT(b)
		if err == nil {
			return writeSubtitle(outPath, format, cues)
		}
		if !errors.Is(err, mp4.ErrTrackNotFound) {
			return "", fmt.Errorf("wvtt extraction: %w", err)
		}
		// stpp: TTML documents live in mdat — extract natively (ffmpeg has
		// no TTML decoder)
		if mdat := subs.ExtractMdat(b); subs.IsTTMLPayload(mdat) {
			b = mdat
			kind = subs.KindTTML
		}
	}
	if kind == subs.KindFMP4 {
		if ffmpeg == "" {
			return "", errors.New("fMP4 subtitles need ffmpeg")
		}
		// Native paths above handle wvtt and TTML. Leave other subtitle codecs
		// to ffmpeg's content probe; never rename away the recoverable raw file.
		if err := mux.ExtractSubtitle(ffmpeg, rawPath, outPath); err != nil {
			return "", err
		}
		return outPath, nil
	}
	cues, err = subs.Parse(b, kind)
	if err != nil {
		return "", err
	}
	return writeSubtitle(outPath, format, cues)
}

func writeSubtitle(outPath, format string, cues []subs.Cue) (string, error) {
	if len(cues) == 0 {
		return "", errors.New("no cues found")
	}
	cues = subs.Rebase(cues)
	if format == "vtt" {
		return outPath, subs.WriteVTT(outPath, cues)
	}
	return outPath, subs.WriteSRT(outPath, cues)
}
