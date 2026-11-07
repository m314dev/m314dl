// Package restream turns the engine's ordered, decrypted segment stream into a
// live HLS presentation served over HTTP. A downloader becomes a re-streamer.
//
// The design deliberately inverts the usual worker model. Instead of piping a
// clear stream into FFmpeg and letting it segment and pace the output, the
// packager receives whole segments already decrypted and in playback order
// (that is exactly what engine.Sink delivers) and does only what a live HLS
// origin must do: keep a rolling window of segments in memory, renumber them
// under its own always-increasing sequence, and rewrite the media playlist
// after each one. There is no subprocess, no -re pacing (the source's own
// segment cadence paces it) and no continuity-counter surgery (segments are
// copied whole, never re-muxed). Segments are held once and shared by every
// viewer instead of being buffered per connection.
package restream

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/m314dev/m314dl/internal/engine"
	"github.com/m314dev/m314dl/internal/manifest"
)

const (
	// windowSize is how many segments stay visible in each media playlist. Six
	// is the common live default: enough for a player to build its buffer,
	// small enough to keep latency near three target durations.
	windowSize = 6
	// tailExtra keeps a few just-evicted segments on hand, so a viewer still
	// downloading the oldest visible segment finds it instead of a 404.
	tailExtra = 3
	// audioGroup and subsGroup are the HLS rendition-group ids that the
	// master's video variants reference. One group per media type is all a
	// restream needs.
	audioGroup = "aud"
	subsGroup  = "subs"
)

// Publisher holds every track of one live presentation and renders its
// playlists. Tracks are added once at startup; after that only their segment
// windows change, so the track list itself needs no lock after setup.
type Publisher struct {
	tracks []*Track
	byID   map[string]*Track
	start  time.Time
	now    func() time.Time // wall clock; tests swap in their own

	// ver goes up on every segment, init or end across all tracks. The DASH
	// manifest body and each track's media playlist are cached and rebuilt
	// only when it changes, so many pollers on a long VOD do not each re-walk
	// every segment.
	ver       atomic.Int64
	cacheMu   sync.Mutex
	dashCache []byte
	dashVer   int64
}

func NewPublisher() *Publisher {
	return &Publisher{byID: map[string]*Track{}, start: time.Now(), now: time.Now}
}

// AddTrack registers a track and returns the engine.Sink that feeds it. Call it
// before serving; exactly one download stream writes the returned sink.
func (p *Publisher) AddTrack(t *Track) engine.Sink {
	t.ver = &p.ver
	p.tracks = append(p.tracks, t)
	p.byID[t.ID] = t
	return t
}

// Track returns a registered track by id.
func (p *Publisher) Track(id string) (*Track, bool) {
	t, ok := p.byID[id]
	return t, ok
}

// End marks every track's playlist finished (appends EXT-X-ENDLIST). Used when
// the source stream ends or the operator stops recording.
func (p *Publisher) End() {
	for _, t := range p.tracks {
		t.mu.Lock()
		t.ended = true
		t.mu.Unlock()
	}
	p.ver.Add(1)
}

// Ready reports whether every track has published at least one segment, that
// is, whether the presentation can be played. Used to hold back the "serving
// at ..." banner until there is actually something to serve.
func (p *Publisher) Ready() bool {
	for _, t := range p.tracks {
		t.mu.RLock()
		n := len(t.segs)
		t.mu.RUnlock()
		if n == 0 {
			return false
		}
	}
	return len(p.tracks) > 0
}

// WaitReady blocks until Ready (or until every track has ended, or ctx or the
// timeout expire). A manifest published before a track has its first segment
// carries an empty SegmentTimeline, and dash.js never recovers from
// initializing on one: it neither errors nor refreshes. So the first manifest
// request waits the second or two the first segments take instead.
// ponytail: 100ms polling; a broadcast channel if request volume ever matters.
func (p *Publisher) WaitReady(ctx context.Context, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for !p.Ready() && !p.allEnded() && time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Stats is a per-track liveness snapshot for the status line.
type Stats struct {
	ID        string
	Type      string
	Published int64 // total segments published over the run
	Window    int   // segments currently visible
	Bitrate   int64 // bits/sec (declared or measured)
}

// StatusLine implements the presentation status contract: one line summarizing
// every track's liveness.
func (p *Publisher) StatusLine() string {
	var parts []string
	for _, s := range p.Stats() {
		br := "?"
		if s.Bitrate > 0 {
			br = fmt.Sprintf("%.1fMbps", float64(s.Bitrate)/1e6)
		}
		parts = append(parts, fmt.Sprintf("%s %d segs %s", s.ID, s.Published, br))
	}
	return strings.Join(parts, " | ")
}

func (p *Publisher) Stats() []Stats {
	out := make([]Stats, 0, len(p.tracks))
	for _, t := range p.tracks {
		t.mu.RLock()
		out = append(out, Stats{
			ID: t.ID, Type: t.typ.String(),
			Published: t.published, Window: len(t.segs), Bitrate: t.bitrateLocked(),
		})
		t.mu.RUnlock()
	}
	return out
}

// ─── track ───────────────────────────────────────────────────────────────────

// Track is one rendition (a video variant, an audio rendition, …). It owns a
// rolling in-memory window of segments and renders its own media playlist. It
// implements engine.Sink: the engine's ordered writer calls Init and Segment
// in playback order.
type Track struct {
	ID  string
	typ manifest.MediaType

	// Master-playlist metadata, fixed at construction.
	bandwidth  int64
	width      int
	height     int
	frameRate  float64
	codecs     string
	language   string
	name       string
	channels   string
	def        bool
	audioGroup string // video: the audio group it plays with (references audioGroup const)
	subsGroup  string
	fmp4       bool
	live       bool   // live: roll a fixed in-memory window; VOD: spool every segment to disk
	spoolDir   string // VOD only: where segment files live (the run's tmpDir)
	segExt     string // "ts" | "m4s"

	channelScheme string // DASH audio descriptor scheme, kept together with channels

	ver *atomic.Int64 // shared publisher version; goes up on every segment, init or end

	mu          sync.RWMutex
	init        []byte
	initAt      time.Time
	inits       [][]byte // every distinct init seen, in order; a segment points at one by index
	segs        []*segment
	byName      map[string]*segment // name -> segment, so a lookup is O(1) instead of a scan
	targetDur   float64
	seq         int64 // output media sequence of the next segment (only ever goes up; never reset)
	nextStartMS int64 // running presentation time in ms for the next segment (DASH timeline)
	published   int64 // total ever published (for stats)
	ended       bool

	plCache []byte // media playlist cached by plVer
	plVer   int64
}

type segment struct {
	name    string
	seq     int64
	dur     float64
	startMS int64 // presentation start time in ms (DASH SegmentTimeline @t)
	initIdx int   // index into Track.inits of the init this segment decodes with
	disc    bool
	size    int64
	data    []byte // live window only; a VOD segment's bytes live on disk
	at      time.Time
}

// TrackFromStream builds a Track from a selected stream. id is a short,
// URL-safe identifier assigned by the caller (e.g. "video", "audio-fr"). live
// picks the output shape: a rolling in-memory window (live), or a seekable
// growing playlist whose segments are spooled to files under spoolDir so the
// whole asset never sits in RAM (VOD; spoolDir is unused for live).
func TrackFromStream(id string, st *manifest.Stream, live bool, spoolDir string) *Track {
	fmp4 := st.Init != nil || segmentIsFMP4(st)
	ext := "ts"
	if fmp4 {
		ext = "m4s"
	}
	return &Track{
		ID: id, typ: st.Type,
		bandwidth: st.Bandwidth, width: st.Width, height: st.Height,
		frameRate: st.FrameRate, codecs: st.Codecs,
		language: st.Language, name: st.Name, channels: st.Channels, def: st.Default,
		channelScheme: st.ChannelScheme,
		fmp4:          fmp4, live: live, spoolDir: spoolDir, segExt: ext,
	}
}

// spoolPath is where a VOD segment's bytes live on disk. Filenames start with
// the track id so every track can share the run's tmpDir.
func (t *Track) spoolPath(name string) string {
	return filepath.Join(t.spoolDir, t.ID+"-"+name)
}

func segmentIsFMP4(st *manifest.Stream) bool {
	if len(st.Segments) == 0 {
		return false
	}
	u := st.Segments[0].URL
	return strings.Contains(u, ".m4s") || strings.Contains(u, ".mp4") || strings.Contains(u, ".cmf")
}

// Init implements engine.Sink: stores the fMP4 initialization segment (the
// headers a player needs before it can decode any media segment). A live
// source can change its init mid-stream, for example when a channel with
// server side ads moves to a new period. Each distinct init is kept under its
// own name, so segments still in the window keep decoding with the init they
// were made for while new segments point at the new one.
func (t *Track) Init(data []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if n := len(t.inits); n > 0 && bytes.Equal(t.inits[n-1], data) {
		return nil // the source re-sent the same init; nothing changes
	}
	buf := append([]byte(nil), data...)
	t.inits = append(t.inits, buf)
	t.init = buf
	t.initAt = time.Now()
	t.bumpVer()
	return nil
}

// initName is the URL a playlist uses for init number i. The first keeps the
// plain name every earlier release served.
func initName(i int) string {
	if i == 0 {
		return "init.mp4"
	}
	return fmt.Sprintf("init%d.mp4", i)
}

// bumpVer marks the cached playlist and manifest bodies stale after a change.
func (t *Track) bumpVer() {
	if t.ver != nil {
		t.ver.Add(1)
	}
}

// Segment implements engine.Sink: appends one segment (to the rolling
// in-memory window for live, to a spool file for VOD) and trims anything that
// has aged out. It renumbers under the track's own always-increasing sequence,
// so a source that rewinds its media sequence cannot wedge or rewind the
// output.
func (t *Track) Segment(info engine.SegmentInfo, data []byte) error {
	// Only the engine's ordered writer calls Segment, so t.seq is stable here
	// without the lock (readers take RLock; only this goroutine writes it).
	// Spooling before taking the lock keeps a multi-MB disk write from
	// blocking playlist renders. It also means a segment is only listed once
	// its file is complete, so the HTTP handler can never see a partial write.
	name := fmt.Sprintf("%06d.%s", t.seq, t.segExt)
	if !t.live {
		if err := os.WriteFile(t.spoolPath(name), data, 0o644); err != nil {
			return fmt.Errorf("spool segment %s: %w", name, err)
		}
	}

	dur := info.Duration
	t.mu.Lock()
	defer t.mu.Unlock()

	// A zero or missing source duration would emit an EXTINF of 0 and break
	// player pacing. Fall back to the largest duration seen (or a 2s default).
	if dur <= 0 {
		if dur = t.targetDur; dur <= 0 {
			dur = 2.0
		}
	}
	if dur > t.targetDur {
		t.targetDur = dur
	}

	seg := &segment{
		name: name,
		seq:  t.seq, dur: dur, startMS: t.nextStartMS, disc: info.Discontinuity,
		initIdx: len(t.inits) - 1,
		size:    int64(len(data)), at: time.Now(),
	}
	if t.live {
		seg.data = append([]byte(nil), data...)
	}
	// A segment made for a different init than the one before it is a format
	// change in HLS terms, and the playlist must say so.
	if n := len(t.segs); n > 0 && t.segs[n-1].initIdx != seg.initIdx {
		seg.disc = true
	}
	if t.byName == nil {
		t.byName = make(map[string]*segment)
	}
	t.byName[name] = seg
	t.segs = append(t.segs, seg)
	t.seq++
	t.nextStartMS += int64(math.Round(dur * 1000))
	t.published++
	t.bumpVer()

	// Live: trim beyond window+tail. The tail keeps just-evicted segments
	// fetchable a moment longer for clients still downloading the oldest
	// visible one. VOD keeps every segment: the bytes are on disk, so RAM
	// stays flat however long the asset is. Evicted slots are set to nil so
	// their segment data can be garbage-collected (a bare reslice would keep
	// it alive in the backing array).
	if max := windowSize + tailExtra; t.live && len(t.segs) > max {
		t.evictOldestLocked(len(t.segs) - max)
	}
	return nil
}

// evictOldestLocked drops the n oldest segments from the live window and
// releases them for GC. Caller holds t.mu.
func (t *Track) evictOldestLocked(n int) {
	for i := 0; i < n; i++ {
		delete(t.byName, t.segs[i].name)
		t.segs[i] = nil
	}
	t.segs = t.segs[n:]
}

// visibleLocked returns the segments that belong in the published playlist:
// every segment for VOD, the newest windowSize for live. Caller holds t.mu.
func (t *Track) visibleLocked() []*segment {
	if !t.live || len(t.segs) <= windowSize {
		return t.segs
	}
	return t.segs[len(t.segs)-windowSize:]
}

// segmentByName returns a segment for the HTTP layer: in-memory bytes for a
// live window segment, a spool-file path for a VOD one. Only names present in
// the segment table resolve, so the path can never point outside the spool.
func (t *Track) segmentByName(name string) (data []byte, path string, modtime time.Time, ok bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	s := t.byName[name]
	if s == nil {
		return nil, "", time.Time{}, false
	}
	if t.live {
		return s.data, "", s.at, true
	}
	return nil, t.spoolPath(name), s.at, true
}

// initSegment returns the fMP4 init bytes and modtime for a playlist name
// such as init.mp4 or init2.mp4.
func (t *Track) initSegment(name string) ([]byte, time.Time, bool) {
	var i int
	switch {
	case name == "init.mp4":
	case strings.HasPrefix(name, "init") && strings.HasSuffix(name, ".mp4"):
		n, err := strconv.Atoi(name[len("init") : len(name)-len(".mp4")])
		if err != nil || n < 1 {
			return nil, time.Time{}, false
		}
		i = n
	default:
		return nil, time.Time{}, false
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	if i >= len(t.inits) {
		return nil, time.Time{}, false
	}
	return t.inits[i], t.initAt, true
}

// bitrateLocked returns the track's bitrate: the declared value, else the peak
// measured from the current window, else 0. Caller holds t.mu.
func (t *Track) bitrateLocked() int64 {
	if t.bandwidth > 0 {
		return t.bandwidth
	}
	var peak int64
	for _, s := range t.segs {
		if s.dur > 0 {
			if br := int64(float64(s.size) * 8 / s.dur); br > peak {
				peak = br
			}
		}
	}
	return peak
}

// mediaPlaylist renders this track's live media playlist (RFC 8216).
func (t *Track) mediaPlaylist() []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	v := int64(0)
	if t.ver != nil {
		v = t.ver.Load()
	}
	if t.plCache != nil && t.plVer == v {
		return t.plCache
	}

	visible := t.visibleLocked()
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	// fMP4 output uses EXT-X-MAP, which requires version 6; plain TS is version 3.
	ver := 3
	if t.fmp4 {
		ver = 6
	}
	fmt.Fprintf(&b, "#EXT-X-VERSION:%d\n", ver)
	td := int(math.Ceil(t.targetDur))
	if td < 1 {
		td = 1
	}
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n", td)
	// VOD: an EVENT playlist is append-only from sequence 0, so players may
	// seek anywhere in what has been published while it keeps growing. It
	// becomes a complete seekable asset once ENDLIST lands below.
	if !t.live {
		b.WriteString("#EXT-X-PLAYLIST-TYPE:EVENT\n")
	}
	seq := int64(0)
	if len(visible) > 0 {
		seq = visible[0].seq
	}
	fmt.Fprintf(&b, "#EXT-X-MEDIA-SEQUENCE:%d\n", seq)
	// EXT-X-MAP names the init for the segments that follow it. It is written
	// before the first segment and again wherever the init changes.
	curInit := -1
	for _, s := range visible {
		if s.disc {
			b.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		if t.fmp4 && s.initIdx >= 0 && s.initIdx != curInit {
			curInit = s.initIdx
			fmt.Fprintf(&b, "#EXT-X-MAP:URI=\"%s\"\n", initName(curInit))
		}
		fmt.Fprintf(&b, "#EXTINF:%.3f,\n%s\n", s.dur, s.name)
	}
	if t.ended {
		b.WriteString("#EXT-X-ENDLIST\n")
	}
	t.plCache, t.plVer = []byte(b.String()), v
	return t.plCache
}

// ─── master playlist ─────────────────────────────────────────────────────────

// masterPlaylist renders the multivariant playlist: audio and subtitle
// renditions as EXT-X-MEDIA, video variants as EXT-X-STREAM-INF that reference
// them. It is built again on every request, so BANDWIDTH corrects itself from
// the measured bitrate as segments arrive instead of staying pinned to a guess.
func (p *Publisher) masterPlaylist() []byte {
	var audio, subs, video []*Track
	for _, t := range p.tracks {
		switch t.typ {
		case manifest.Audio:
			audio = append(audio, t)
		case manifest.Subtitles:
			subs = append(subs, t)
		default:
			video = append(video, t)
		}
	}

	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:6\n")

	for i, a := range audio {
		def := i == 0 // first rendition of a group is the default
		writeMedia(&b, "AUDIO", audioGroup, a, def)
	}
	for i, s := range subs {
		writeMedia(&b, "SUBTITLES", subsGroup, s, i == 0)
	}

	haveAudio := len(audio) > 0
	haveSubs := len(subs) > 0
	audioBitrate := int64(0)
	if haveAudio {
		audio[0].mu.RLock()
		audioBitrate = audio[0].bitrateLocked()
		audio[0].mu.RUnlock()
	}

	for _, v := range video {
		v.mu.RLock()
		bw := v.bitrateLocked() + audioBitrate
		if bw == 0 {
			bw = 2_000_000 // no segments measured yet; corrects on the next fetch
		}
		codecs := v.codecs
		res := v.resolution()
		fr := v.frameRate
		v.mu.RUnlock()
		if haveAudio && audio[0].codecs != "" && codecs != "" {
			codecs = codecs + "," + audio[0].codecs
		}

		var attrs []string
		attrs = append(attrs, fmt.Sprintf("BANDWIDTH=%d", bw))
		if codecs != "" {
			attrs = append(attrs, fmt.Sprintf("CODECS=%q", codecs))
		}
		if res != "" {
			attrs = append(attrs, "RESOLUTION="+res)
		}
		if fr > 0 {
			attrs = append(attrs, fmt.Sprintf("FRAME-RATE=%.3f", fr))
		}
		if haveAudio {
			attrs = append(attrs, fmt.Sprintf("AUDIO=%q", audioGroup))
		}
		if haveSubs {
			attrs = append(attrs, fmt.Sprintf("SUBTITLES=%q", subsGroup))
		}
		fmt.Fprintf(&b, "#EXT-X-STREAM-INF:%s\n%s/index.m3u8\n", strings.Join(attrs, ","), v.ID)
	}

	// Audio-only presentation (no video variant): it still needs a STREAM-INF,
	// or players see an empty master. Point it at the first audio rendition.
	if len(video) == 0 && haveAudio {
		a := audio[0]
		a.mu.RLock()
		bw := a.bitrateLocked()
		a.mu.RUnlock()
		if bw == 0 {
			bw = 128_000
		}
		attrs := fmt.Sprintf("BANDWIDTH=%d", bw)
		if a.codecs != "" {
			attrs += fmt.Sprintf(",CODECS=%q", a.codecs)
		}
		fmt.Fprintf(&b, "#EXT-X-STREAM-INF:%s\n%s/index.m3u8\n", attrs, a.ID)
	}
	return []byte(b.String())
}

func writeMedia(b *strings.Builder, kind, group string, t *Track, def bool) {
	yesno := func(v bool) string {
		if v {
			return "YES"
		}
		return "NO"
	}
	attrs := []string{
		fmt.Sprintf("TYPE=%s", kind),
		fmt.Sprintf("GROUP-ID=%q", group),
		fmt.Sprintf("NAME=%q", t.displayName()),
		fmt.Sprintf("DEFAULT=%s", yesno(def || t.def)),
		fmt.Sprintf("AUTOSELECT=%s", yesno(def || t.def)),
	}
	if t.language != "" {
		attrs = append(attrs, fmt.Sprintf("LANGUAGE=%q", t.language))
	}
	if kind == "AUDIO" && t.channels != "" {
		attrs = append(attrs, fmt.Sprintf("CHANNELS=%q", t.channels))
	}
	attrs = append(attrs, fmt.Sprintf("URI=%q", t.ID+"/index.m3u8"))
	fmt.Fprintf(b, "#EXT-X-MEDIA:%s\n", strings.Join(attrs, ","))
}

func (t *Track) displayName() string {
	switch {
	case t.name != "":
		return t.name
	case t.language != "":
		return t.language
	default:
		return t.ID
	}
}

func (t *Track) resolution() string {
	if t.width == 0 && t.height == 0 {
		return ""
	}
	return fmt.Sprintf("%dx%d", t.width, t.height)
}
