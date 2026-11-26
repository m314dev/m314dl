// Package worker is the worker mode (-worker): a restream agent that runs many
// channels and that a controller (for example the m314 panel) drives over
// HTTP. One process holds many channels. Each channel downloads and decrypts
// its source and republishes it live at /{id}/live.m3u8 (or /live.ts,
// /live.mpd). Channels run inside this process: the media is already in RAM
// in the packager, so there is no child process to look after and no reverse
// proxy to a child's port.
//
// Control API (Bearer -worker-secret):
//
//	POST   /api/channels          {"id":"news","url":"...","format":"hls","keys":["KID:KEY"],"headers":{...}}
//	GET    /api/channels          -> [channel, ...] with every channel's full view
//	GET    /api/channels/{id}     -> one channel: state, health, source, output and track numbers
//	PATCH  /api/channels/{id}     {"url":"...","keys":[...],"headers":{...},"proxy":"...","insecure":false}
//	                              replaces the input of a running channel without restarting it
//	DELETE /api/channels/{id}     -> stop and remove
//	GET    /api/channels/{id}/log -> the channel's last lines, plain text
//	POST   /api/probe             {"url":"...","headers":{...},"proxy":"...","insecure":false}
//	                              -> the tracks a channel would see, grouped by kind; nothing is started
//	GET    /api/health            -> {"status":"ok","channels":3,"max":32,"version":..} plus the
//	                              host's cpu, ram, disk, network and this process's rss
//
// GET /{id}/... routes to that channel's HLS/TS/DASH handler. Media is served
// to anyone who has the URL unless -worker-media-secret is set; then every
// media URL must carry exp and sig (see mediasign.go) and the worker signs the
// URIs in the playlists it serves.
package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/m314dev/m314dl/internal/hls"
	"github.com/m314dev/m314dl/internal/httpx"
	"github.com/m314dev/m314dl/internal/manifest"
	"github.com/m314dev/m314dl/internal/pick"
	"github.com/m314dev/m314dl/internal/serve"
	"github.com/m314dev/m314dl/internal/source"
)

type workerServer struct {
	secret      string
	mediaSecret string // signs media URLs; "" serves media to anyone who has the URL
	version     string
	maxChannels int
	ffmpegPath  string
	defaultIdle time.Duration // on-demand idle period for channels that do not set one; 0 = always on
	logv        func(string, ...any)

	// spool is where channels stage their segments. Each worker instance owns
	// one and clears it on startup. A channel removes its own directory when it
	// stops, but a process that is killed or restarted never gets the chance,
	// and the leftovers are invisible and grow without limit. One day with many
	// restarts left 1430 directories and 43GB behind, on the same disk the live
	// segments were being written to.
	spool string

	hm   hostMeter                 // read by the sampler goroutine only
	host atomic.Pointer[hostStats] // the newest sample, for GET /api/health

	mu       sync.RWMutex
	channels map[string]*channel
}

// channel is one running restream. Its input (URL, headers, proxy, keys) can
// change over its life through PATCH; its output never does, so the packager,
// handler and media path are fixed at creation.
type channel struct {
	id         string
	format     string
	kind       string
	logv       func(string, ...any) // the worker's -v output
	mediaPath  string               // "/{id}/live.m3u8"
	handler    http.Handler
	pres       serve.Presentation
	cancel     context.CancelFunc
	tmpDir     string
	client     *httpx.Client // the channel's one client; a PATCH swaps its insides in place
	ve, ae, se *pick.Expr
	viewers    *serve.Viewers // always-on channels; an on-demand channel counts its own

	od   *serve.OnDemand // set for an on-demand channel; owns the source lifecycle
	idle time.Duration

	// in is what the download loops read while they run. It is replaced on
	// every on-demand session start and nil before the first one.
	in    atomic.Pointer[source.Live]
	out   atomic.Int64 // bytes sent to viewers
	meter meter
	log   logRing

	mu       sync.Mutex
	req      channelReq          // the input as last set: URL, headers, proxy, keys, insecure
	keys     map[[16]byte][]byte // parsed req.Keys
	selected []*manifest.Stream  // the tracks chosen at the last resolve, for a PATCH to compare against
	state    string              // running | done | error | stopped (on-demand: see OnDemand.State)
	errMsg   string
	lastWarn string // the newest line about the source not moving
	live     bool
	started  time.Time
	swaps    int
}

var idOK = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,64}$`)

func newWorkerServer(secret string, maxChannels int, ffmpegPath, version string, defaultIdle time.Duration, logv func(string, ...any)) *workerServer {
	if logv == nil {
		logv = func(string, ...any) {}
	}
	w := &workerServer{secret: secret, version: version, maxChannels: maxChannels, ffmpegPath: ffmpegPath,
		defaultIdle: defaultIdle, logv: logv, channels: map[string]*channel{}}
	go w.sampler()
	return w
}

// useSpool claims a staging directory for this worker and clears whatever a
// previous run left in it. base is the -spool-dir override; "" is resolved by
// serve.SpoolBase to the current directory, which is real disk, unlike the
// system temp dir, which is usually tmpfs on Linux.
//
// The directory is named after the address the worker listens on, so two
// workers on one host never share or delete each other's staging area, and a
// restart of either one reclaims exactly its own leftovers.
func (w *workerServer) useSpool(addr, base string) error {
	base, err := serve.SpoolBase(base)
	if err != nil {
		return err
	}
	name := "m314dl-worker-" + strings.NewReplacer(":", "_", "/", "_", ".", "_").Replace(addr)
	dir := filepath.Join(base, name)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("clear spool %s: %w", dir, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create spool %s: %w", dir, err)
	}
	w.spool = dir
	return nil
}

// spoolDir is where a channel stages its segments.
func (w *workerServer) spoolDir() string {
	if w.spool != "" {
		return w.spool
	}
	return "" // os.MkdirTemp falls back to the system temp dir
}

func ServeWorker(addr, secret, mediaSecret string, maxChannels int, ffmpegPath, spoolDir, version string, defaultIdle time.Duration, logv func(string, ...any)) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("-worker %q: %w", addr, err)
	}
	if secret == "" && host != "localhost" {
		if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("-worker on non-loopback %q requires -worker-secret", addr)
		}
	}
	w := newWorkerServer(secret, maxChannels, ffmpegPath, version, defaultIdle, logv)
	w.mediaSecret = mediaSecret
	if err := w.useSpool(addr, spoolDir); err != nil {
		return err
	}
	srv := &http.Server{Addr: addr, Handler: w.handler(), ReadHeaderTimeout: 10 * time.Second}

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Fprintln(os.Stderr, "\nworker: shutting down, stopping channels")
		w.stopAll()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}()

	cap := "unlimited"
	if maxChannels > 0 {
		cap = fmt.Sprint(maxChannels)
	}
	fmt.Fprintf(os.Stderr, "m314dl %s worker on %s (max channels: %s)\n", version, addr, cap)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (w *workerServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/channels", httpx.BearerAuth(w.secret, w.apiStart))
	mux.HandleFunc("GET /api/channels", httpx.BearerAuth(w.secret, w.apiList))
	mux.HandleFunc("GET /api/channels/{id}", httpx.BearerAuth(w.secret, w.apiStatus))
	mux.HandleFunc("PATCH /api/channels/{id}", httpx.BearerAuth(w.secret, w.apiPatch))
	mux.HandleFunc("DELETE /api/channels/{id}", httpx.BearerAuth(w.secret, w.apiStop))
	mux.HandleFunc("GET /api/channels/{id}/log", httpx.BearerAuth(w.secret, w.apiLog))
	mux.HandleFunc("POST /api/probe", httpx.BearerAuth(w.secret, w.apiProbe))
	mux.HandleFunc("GET /api/health", httpx.BearerAuth(w.secret, w.apiHealth))
	mux.HandleFunc("/{id}/", w.media) // media: unauthenticated, routed to the channel
	return mux
}

// ─── control API ─────────────────────────────────────────────────────────────

type channelReq struct {
	ID        string            `json:"id"`
	URL       string            `json:"url"`
	Format    string            `json:"format"` // hls | ts | dash (default hls)
	Keys      []string          `json:"keys"`   // CENC "KID:KEY" (hex)
	Headers   map[string]string `json:"headers"`
	Proxy     string            `json:"proxy"`
	Insecure  bool              `json:"insecure"`
	SV        string            `json:"sv"` // video selection (default best)
	SA        string            `json:"sa"`
	SS        string            `json:"ss"`
	Transcode string            `json:"transcode"` // ts remux: ffmpeg output args
	Idle      string            `json:"idle"`      // on demand: release the source after this long with no viewer, e.g. "30s" (default -worker-idle; "0" = always on)
}

func (w *workerServer) apiStart(rw http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(rw, r.Body, 64<<10)
	var req channelReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.URL == "" || req.ID == "" {
		http.Error(rw, `bad request: want {"id":"...","url":"...","format":"hls|ts|dash"}`, http.StatusBadRequest)
		return
	}
	if !idOK.MatchString(req.ID) || req.ID == "api" {
		http.Error(rw, "bad channel id (want [a-zA-Z0-9._-], not 'api')", http.StatusBadRequest)
		return
	}

	w.mu.RLock()
	_, exists := w.channels[req.ID]
	atCap := w.maxChannels > 0 && len(w.channels) >= w.maxChannels
	w.mu.RUnlock()
	if exists {
		http.Error(rw, "channel already exists: "+req.ID, http.StatusConflict)
		return
	}
	if atCap {
		http.Error(rw, fmt.Sprintf("at capacity (%d channels)", w.maxChannels), http.StatusServiceUnavailable)
		return
	}

	ch, err := w.startChannel(req)
	if err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}
	httpx.WriteJSON(rw, ch.view())
}

func (w *workerServer) apiList(rw http.ResponseWriter, r *http.Request) {
	w.mu.RLock()
	views := make([]channelView, 0, len(w.channels))
	for _, ch := range w.channels {
		views = append(views, ch.view())
	}
	w.mu.RUnlock()
	httpx.WriteJSON(rw, views)
}

func (w *workerServer) apiStatus(rw http.ResponseWriter, r *http.Request) {
	ch := w.channel(r.PathValue("id"))
	if ch == nil {
		http.Error(rw, "no such channel", http.StatusNotFound)
		return
	}
	httpx.WriteJSON(rw, ch.view())
}

func (w *workerServer) apiLog(rw http.ResponseWriter, r *http.Request) {
	ch := w.channel(r.PathValue("id"))
	if ch == nil {
		http.Error(rw, "no such channel", http.StatusNotFound)
		return
	}
	rw.Header().Set("Content-Type", "text/plain; charset=utf-8")
	rw.Write([]byte(ch.log.text()))
}

func (w *workerServer) apiStop(rw http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	w.mu.Lock()
	ch := w.channels[id]
	if ch != nil {
		delete(w.channels, id)
	}
	w.mu.Unlock()
	if ch == nil {
		http.Error(rw, "no such channel", http.StatusNotFound)
		return
	}
	ch.cancel() // downloads stop; the run goroutine ends the presentation and cleans up
	httpx.WriteJSON(rw, map[string]string{"status": "stopped", "id": id})
}

func (w *workerServer) apiHealth(rw http.ResponseWriter, r *http.Request) {
	w.mu.RLock()
	n := len(w.channels)
	w.mu.RUnlock()
	out := healthOut{
		Status: "ok", Channels: n, Max: w.maxChannels,
		// The version is how a panel knows the worker is deployed at all. When
		// none was reported, its server list showed every node as "not
		// deployed", no matter how healthy the node was.
		Version: w.version,
	}
	if h := w.host.Load(); h != nil {
		out.hostStats = *h
	}
	httpx.WriteJSON(rw, out)
}

// healthOut is the reply to GET /api/health: the worker, then the machine.
type healthOut struct {
	Status   string `json:"status"`
	Channels int    `json:"channels"`
	Max      int    `json:"max"`
	Version  string `json:"version"`
	hostStats
}

// ─── media routing ───────────────────────────────────────────────────────────

func (w *workerServer) media(rw http.ResponseWriter, r *http.Request) {
	ch := w.channel(r.PathValue("id"))
	if ch == nil {
		http.NotFound(rw, r)
		return
	}
	if w.mediaSecret != "" {
		exp, status, reason := checkSigned(w.mediaSecret, ch.id, r)
		if status != 0 {
			http.Error(rw, reason, status)
			return
		}
		if isPlaylistPath(r.URL.Path) {
			// The body is rewritten, so its length changes: no Range replies.
			r.Header.Del("Range")
			sw := &signingWriter{ResponseWriter: rw,
				query: "exp=" + strconv.FormatInt(exp, 10) + "&sig=" + signPath(w.mediaSecret, "/"+ch.id+"/", exp)}
			defer sw.finish(r.URL.Path)
			rw = sw
		}
	}
	// Strip the /{id} prefix so the channel's own handler sees /live.m3u8 etc.
	http.StripPrefix("/"+ch.id, ch.handler).ServeHTTP(rw, r)
}

func (w *workerServer) channel(id string) *channel {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.channels[id]
}

// ─── channel lifecycle ───────────────────────────────────────────────────────

// newClient builds the HTTP client a channel's input calls for.
func newClient(req channelReq) (*httpx.Client, error) {
	headers := req.Headers
	if headers == nil {
		headers = map[string]string{}
	}
	return httpx.New(httpx.Options{Headers: headers, Proxy: req.Proxy, Insecure: req.Insecure, Retries: 5})
}

// resolve loads a source and picks its tracks by the channel's selection. It
// is the one path from a URL to a selection, run at creation, at each
// on-demand start and when a PATCH checks a new URL before taking it.
func (w *workerServer) resolve(ctx context.Context, client *httpx.Client, url string, keys map[[16]byte][]byte, ve, ae, se *pick.Expr) (kind string, sel []*manifest.Stream, err error) {
	master, kind, err := source.LoadManifest(ctx, client, url, w.logv)
	if err != nil {
		return "", nil, err
	}
	sel, err = selectAndExpand(ctx, client, kind, master, ve, ae, se, keys, w.logv)
	return kind, sel, err
}

// startChannel loads the source, selects the streams, builds the output, and
// starts the downloads in the background. It blocks only for the manifest
// fetch and the selection, so a start error (a bad URL, DRM without a key)
// shows up in the response. A channel that started is served right away.
func (w *workerServer) startChannel(req channelReq) (*channel, error) {
	client, err := newClient(req)
	if err != nil {
		return nil, err
	}
	keys, err := source.ParseKeys(req.Keys)
	if err != nil {
		return nil, err
	}
	ve, ae, se, err := parseSelection(req.SV, req.SA, req.SS)
	if err != nil {
		return nil, err
	}
	idle := w.defaultIdle
	if req.Idle != "" {
		if idle, err = time.ParseDuration(req.Idle); err != nil {
			return nil, fmt.Errorf("idle: %w (want a duration such as \"30s\")", err)
		}
	}
	if idle != 0 && idle < serve.MinIdle {
		return nil, fmt.Errorf("idle must be at least %s", serve.MinIdle)
	}

	ctx, cancel := context.WithCancel(context.Background())
	kind, selected, err := w.resolve(ctx, client, req.URL, keys, ve, ae, se)
	if err != nil {
		cancel()
		return nil, err
	}
	format, err := serve.CheckFormat(kind, req.Format)
	if err != nil {
		cancel()
		return nil, err
	}
	req.Format = format

	tmpDir, err := os.MkdirTemp(w.spoolDir(), "m314dl-ch-"+req.ID+"-*")
	if err != nil {
		cancel()
		return nil, err
	}
	o := serve.Options{Format: req.Format, Transcode: req.Transcode, FFmpegPath: w.ffmpegPath}

	ch := &channel{
		id: req.ID, format: normFormat(req.Format), kind: kind, logv: w.logv, client: client, ve: ve, ae: ae, se: se,
		cancel: cancel, tmpDir: tmpDir, req: req, keys: keys, selected: selected,
		state: "running", live: isLive(selected), started: time.Now(),
	}

	var jobs []serve.Job
	if idle > 0 && ch.live {
		// On demand: the source was loaded and the selection checked, so a bad
		// request fails here. Nothing is fetched until the first viewer asks,
		// and each start loads the manifest again, by whatever input the
		// channel has by then, and selects by the same rules.
		start := func(ctx context.Context) (serve.Presentation, http.Handler, []serve.Job, error) {
			ch.mu.Lock()
			url, keys := ch.req.URL, ch.keys
			ch.mu.Unlock()
			k, sel, err := w.resolve(ctx, client, url, keys, ve, ae, se)
			if err != nil {
				return nil, nil, nil, err
			}
			// run keeps the kind of the first load. A live MPEG-TS source cannot
			// run in the segment engine, nor a manifest through the pipe.
			if (k == source.LiveTSKind) != (kind == source.LiveTSKind) {
				return nil, nil, nil, fmt.Errorf("the source changed from %s to %s since the channel was created", kind, k)
			}
			ch.mu.Lock()
			ch.selected = sel
			ch.mu.Unlock()
			ch.in.Store(source.NewLive(kind, client, keys, sel))
			pres, h, _, jobs, err := serve.BuildOutputs(o, sel, tmpDir, ch.verbose)
			return pres, h, jobs, err
		}
		run := func(ctx context.Context, jobs []serve.Job) error {
			return serve.RunJobs(ctx, ch.in.Load(), jobs, nil, 0, ch.verbose, ch.warnf)
		}
		od := serve.NewOnDemand(idle, start, run, ch.logf)
		ch.od, ch.idle, ch.pres = od, idle, od
		ch.handler = countBytes(od, &ch.out)
		ch.mediaPath = "/" + req.ID + serve.MediaPath(req.Format)
	} else {
		pres, handler, path, built, err := serve.BuildOutputs(o, selected, tmpDir, ch.verbose)
		if err != nil {
			cancel()
			os.RemoveAll(tmpDir)
			return nil, err
		}
		ch.in.Store(source.NewLive(kind, client, keys, selected))
		ch.viewers = serve.NewViewers(viewerWindow)
		ch.handler = ch.viewers.Wrap(countBytes(handler, &ch.out))
		ch.pres, ch.mediaPath, jobs = pres, "/"+req.ID+path, built
	}
	ch.logf("created: %s source, %s output", kind, ch.format)

	// Register the channel, checking again for a racing start of the same id
	// or a full table.
	w.mu.Lock()
	if _, exists := w.channels[req.ID]; exists {
		w.mu.Unlock()
		cancel()
		os.RemoveAll(tmpDir)
		return nil, fmt.Errorf("channel already exists: %s", req.ID)
	}
	if w.maxChannels > 0 && len(w.channels) >= w.maxChannels {
		w.mu.Unlock()
		cancel()
		os.RemoveAll(tmpDir)
		return nil, fmt.Errorf("at capacity (%d channels)", w.maxChannels)
	}
	w.channels[req.ID] = ch
	w.mu.Unlock()

	if ch.od != nil {
		// The OnDemand handler runs the source by itself. Deleting the channel
		// cancels ctx, which closes it and drops the spool.
		go func() {
			<-ctx.Done()
			ch.od.Close()
			os.RemoveAll(ch.tmpDir)
		}()
		return ch, nil
	}
	go w.runChannel(ctx, ch, jobs)
	return ch, nil
}

// runChannel downloads every track into its sink until the source ends or the
// channel is stopped, then records the final state and cleans up.
func (w *workerServer) runChannel(ctx context.Context, ch *channel, jobs []serve.Job) {
	// End the presentation on cancel too, so an ffmpeg remux writer stuck on a
	// stalled pipe gets EPIPE and the errgroup can return.
	go func() {
		<-ctx.Done()
		ch.pres.End()
	}()

	err := serve.RunJobs(ctx, ch.in.Load(), jobs, nil, 0, ch.verbose, ch.warnf)
	ch.pres.End()

	ch.mu.Lock()
	switch {
	case ctx.Err() != nil:
		ch.state = "stopped"
	case err != nil:
		ch.state, ch.errMsg = "error", err.Error()
		ch.logf("source failed: %v", err)
	default:
		ch.state = "done" // a finite source finished; it is still served (ENDLIST)
		ch.logf("source ended")
	}
	ch.mu.Unlock()

	// A finished VOD channel keeps serving its spooled segments from tmpDir, so
	// the spool must outlive the download and go away only when the channel does
	// (stop or removal cancels ctx). useSpool sweeps a stale spool left by a
	// crash at the next startup.
	<-ctx.Done()
	os.RemoveAll(ch.tmpDir)
}

func (w *workerServer) stopAll() {
	w.mu.Lock()
	chans := make([]*channel, 0, len(w.channels))
	for _, ch := range w.channels {
		chans = append(chans, ch)
	}
	w.channels = map[string]*channel{}
	w.mu.Unlock()
	for _, ch := range chans {
		ch.cancel()
	}
}

// logf writes one line to the channel's own log and to the worker's verbose
// output: lifecycle, swaps, on-demand starts and stops, warnings.
func (ch *channel) logf(format string, args ...any) {
	ch.log.add(fmtLine(format, args...))
	ch.logv("channel %s: "+format, append([]any{ch.id}, args...)...)
}

// verbose is for the download loops' running commentary (concurrency changes,
// skipped ads), which goes to the worker's -v output only. In the channel log
// it would bury the lines that matter within minutes.
func (ch *channel) verbose(format string, args ...any) {
	ch.logv("channel %s: "+format, append([]any{ch.id}, args...)...)
}

// warnf is logf for the engine's warnings: a source that stopped moving. The
// newest one is also shown in the channel view as the source's last error.
func (ch *channel) warnf(format string, args ...any) {
	ch.mu.Lock()
	ch.lastWarn = fmt.Sprintf(format, args...)
	ch.mu.Unlock()
	ch.logf(format, args...)
}

// ─── views ───────────────────────────────────────────────────────────────────

// channelView is what GET /api/channels/{id} returns and what each element of
// GET /api/channels is. Rates are averaged over the last ten seconds, error
// counts over the last minute.
type channelView struct {
	ID      string    `json:"id"`
	URL     string    `json:"url"`
	Format  string    `json:"format"`
	State   string    `json:"state"`  // running | done | error | stopped, or for on demand: idle | starting | running
	Health  string    `json:"health"` // ok | degraded | stalled | down | idle
	Error   string    `json:"error,omitempty"`
	Idle    string    `json:"idle,omitempty"` // on demand: the idle period
	Media   string    `json:"media"`
	Started time.Time `json:"started"`
	UptimeS int64     `json:"uptime_s"`
	Status  string    `json:"status,omitempty"` // one line for a person; never parse it

	Source sourceView  `json:"source"`
	Output outputView  `json:"output"`
	Tracks []trackView `json:"tracks"`
}

// sourceView is the input side: what is being pulled and how it is doing.
type sourceView struct {
	Kind         string `json:"kind"` // hls | dash | mss | livets
	Live         bool   `json:"live"`
	Swaps        int    `json:"swaps"`      // inputs replaced through PATCH
	Restarts     int64  `json:"restarts"`   // on-demand starts after the first, plus raw TS reconnects
	LastError    string `json:"last_error"` // why the last start or run ended, or the newest stall warning
	Bytes        int64  `json:"bytes"`
	BPS          int64  `json:"bps"`
	ErrorsPerMin int64  `json:"errors_per_min"` // requests that failed or came back 4xx/5xx
}

// outputView is the output side: what viewers are getting.
type outputView struct {
	Publishing       bool  `json:"publishing"` // at least one segment has gone out
	Bytes            int64 `json:"bytes"`
	BPS              int64 `json:"bps"`
	Viewers          int   `json:"viewers"`
	LastSegmentAgeMS int64 `json:"last_segment_age_ms"` // across all tracks; -1 until something is published
}

type trackView struct {
	ID               string  `json:"id"`
	Type             string  `json:"type"` // video | audio
	Bitrate          int64   `json:"bitrate"`
	Width            int     `json:"width,omitempty"`
	Height           int     `json:"height,omitempty"`
	Seq              int64   `json:"seq"`       // output media sequence of the newest segment; -1 before the first
	Published        int64   `json:"published"` // segments over the run (publish units for a TS fan-out)
	Window           int     `json:"window"`    // segments visible (viewers for a TS fan-out)
	Realtime         float64 `json:"realtime"`  // media seconds per wall second; 0 unknown
	LastSegmentAgeMS int64   `json:"last_segment_age_ms"`

	targetDur float64 // for health; not serialised
}

func (ch *channel) view() channelView {
	now := time.Now()
	tracks := trackViews(ch.pres.Stats(), now)
	inBps, outBps, errs := ch.meter.rates(now)
	bytesIn, _, _ := ch.client.Traffic()

	ch.mu.Lock()
	v := channelView{
		ID: ch.id, URL: ch.req.URL, Format: ch.format, State: ch.state, Error: ch.errMsg,
		Media: ch.mediaPath, Status: ch.pres.StatusLine(), Started: ch.started, UptimeS: int64(now.Sub(ch.started).Seconds()),
		Source: sourceView{Kind: ch.kind, Live: ch.live, Swaps: ch.swaps, LastError: ch.lastWarn,
			Bytes: bytesIn, BPS: inBps, ErrorsPerMin: errs},
		Output: outputView{Bytes: ch.out.Load(), BPS: outBps, LastSegmentAgeMS: -1},
		Tracks: tracks,
	}
	ch.mu.Unlock()

	if in := ch.in.Load(); in != nil {
		v.Source.Restarts += in.Reconnects.Load()
	}
	if ch.od != nil {
		v.State, v.Idle, v.Output.Viewers = ch.od.State(), ch.idle.String(), ch.od.Viewers()
		if e := ch.od.LastError(); e != "" {
			v.Source.LastError = e
		}
		if n := ch.od.Starts(); n > 1 {
			v.Source.Restarts += int64(n - 1)
		}
	} else {
		v.Output.Viewers = ch.viewers.Count()
	}
	if v.Error != "" {
		v.Source.LastError = v.Error
	}
	for _, t := range tracks {
		if t.Published > 0 {
			v.Output.Publishing = true
		}
		if t.LastSegmentAgeMS >= 0 && (v.Output.LastSegmentAgeMS < 0 || t.LastSegmentAgeMS < v.Output.LastSegmentAgeMS) {
			v.Output.LastSegmentAgeMS = t.LastSegmentAgeMS
		}
	}
	v.Health = health(v.State, ch.kind, now.Sub(v.Started), tracks, errs)
	return v
}

func isLive(streams []*manifest.Stream) bool {
	for _, st := range streams {
		if st.Live {
			return true
		}
	}
	return false
}

func normFormat(f string) string {
	switch f {
	case "ts", "mpegts":
		return "ts"
	case "dash", "mpd":
		return "dash"
	default:
		return "hls"
	}
}

// parseSelection parses the three selector expressions. An empty sv means best.
func parseSelection(sv, sa, ss string) (ve, ae, se *pick.Expr, err error) {
	if sv == "" {
		sv = "best"
	}
	if ve, err = pick.ParseExpr(sv); err != nil {
		return nil, nil, nil, fmt.Errorf("sv: %w", err)
	}
	if sa != "" {
		if ae, err = pick.ParseExpr(sa); err != nil {
			return nil, nil, nil, fmt.Errorf("sa: %w", err)
		}
	}
	if ss != "" {
		if se, err = pick.ParseExpr(ss); err != nil {
			return nil, nil, nil, fmt.Errorf("ss: %w", err)
		}
	}
	return ve, ae, se, nil
}

// selectAndExpand selects the requested streams. For HLS it fetches each media
// playlist; for DASH it splits on-demand (SegmentBase) files into segments by
// their sidx index, the same expansion run() does. It fails at once on a CENC
// track with no key, so a channel that is set up wrong reports the reason at
// start.
func selectAndExpand(ctx context.Context, client *httpx.Client, kind string, master *manifest.Master, ve, ae, se *pick.Expr, keys map[[16]byte][]byte, logv func(string, ...any)) ([]*manifest.Stream, error) {
	if kind == source.LiveTSKind {
		return []*manifest.Stream{source.LiveTSTrack(master)}, nil // one muxed stream: nothing to pick or expand
	}
	selected := pick.Select(master.Streams, ve, ae, se)
	if len(selected) == 0 {
		return nil, fmt.Errorf("no streams matched the selection")
	}
	// A video selection that matches nothing must not silently leave an
	// audio-only channel. Such a channel runs, every metric looks healthy, and
	// the first person to notice is a viewer looking at a black screen. That is
	// exactly how a quality preference of "720" against a source publishing
	// 768x432 took the picture off a live channel.
	if wantsVideo(ve) && !hasKind(selected, manifest.Video) && hasKind(master.Streams, manifest.Video) {
		return nil, fmt.Errorf("no video stream matched the selection; the source publishes %s "+
			"(use -sv none for an audio-only channel)", videoSummary(master.Streams))
	}
	for _, st := range selected {
		if kind == "hls" && !st.SegmentsFull {
			body, finalURL, err := client.FetchBytes(ctx, st.PlaylistURL, "")
			if err != nil {
				return nil, fmt.Errorf("fetch media playlist for %s: %w", st.ID, err)
			}
			st.PlaylistURL = finalURL
			if err := hls.ParseMedia(body, finalURL, st); err != nil {
				return nil, err
			}
		}
		if kind == "dash" {
			if err := source.ExpandIndexed(ctx, client, st, logv); err != nil {
				return nil, err
			}
		}
		if len(st.Segments) == 0 {
			return nil, fmt.Errorf("stream %s has no segments", st.ID)
		}
		if k := st.Segments[0].Key; k != nil && k.Method == manifest.EncCENC && len(keys) == 0 {
			return nil, fmt.Errorf("stream %s is CENC/DRM-protected; supply the content key", st.ID)
		}
	}
	return selected, nil
}

// wantsVideo reports whether a video selection asks for a picture at all.
// A nil expression means "best", which does.
func wantsVideo(ve *pick.Expr) bool { return ve == nil || !ve.IsNone() }

func hasKind(streams []*manifest.Stream, k manifest.MediaType) bool {
	for _, st := range streams {
		if st.Type == k {
			return true
		}
	}
	return false
}

// videoSummary lists the resolutions a source does publish, so the error can
// name the alternatives as well as reject what was asked for.
func videoSummary(streams []*manifest.Stream) string {
	var out []string
	seen := map[string]bool{}
	for _, st := range streams {
		if st.Type != manifest.Video {
			continue
		}
		r := st.Resolution()
		if r == "" || seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
	}
	if len(out) == 0 {
		return "no video at all"
	}
	return strings.Join(out, ", ")
}
