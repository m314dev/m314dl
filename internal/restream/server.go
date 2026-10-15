package restream

import (
	"bytes"
	"net/http"
	"strings"
	"time"
)

// Server exposes a Publisher over HTTP as a live HLS origin:
//
//	GET /live.m3u8          → multivariant (master) playlist
//	GET /{track}/index.m3u8 → a track's media playlist
//	GET /{track}/init.mp4   → a track's fMP4 init segment (EXT-X-MAP target)
//	GET /{track}/{seg}      → one media segment (000123.ts / .m4s)
//
// Playlists are rendered fresh per request (no-cache); segments are served with
// http.ServeContent so players get Range, ETag, and conditional requests for
// free, straight from the shared in-memory window — one copy feeds every viewer.
type Server struct {
	pub *Publisher
}

func NewServer(pub *Publisher) *Server { return &Server{pub: pub} }

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /live.m3u8", s.master)
	mux.HandleFunc("GET /{track}/index.m3u8", s.media)
	mux.HandleFunc("GET /{track}/init.mp4", s.initSeg)
	mux.HandleFunc("GET /{track}/{seg}", s.segment)
	return mux
}

// DASHHandler serves the same in-memory window as a DASH presentation:
//
//	GET /live.mpd         → the manifest (SegmentTemplate + SegmentTimeline)
//	GET /{track}/init.mp4 → init segment (shared with the HLS handler)
//	GET /{track}/{seg}    → one media segment (shared with the HLS handler)
func (s *Server) DASHHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /live.mpd", s.mpd)
	mux.HandleFunc("GET /{track}/init.mp4", s.initSeg)
	mux.HandleFunc("GET /{track}/{seg}", s.segment)
	return mux
}

const (
	mimeM3U8 = "application/vnd.apple.mpegurl"
	mimeMPD  = "application/dash+xml"
)

// readyTimeout bounds how long a manifest request waits for the first segment
// of every track. Under every common player's manifest timeout (dash.js 10s,
// Shaka 30s); past it an incomplete DASH presentation returns a retryable 503.
const readyTimeout = 8 * time.Second

func (s *Server) mpd(w http.ResponseWriter, r *http.Request) {
	s.pub.WaitReady(r.Context(), readyTimeout)
	cors(w)
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	if !s.pub.Ready() {
		// A timeout (or failed download) doesn't make an empty timeline valid.
		// Never let a player initialize successfully on an unplayable MPD.
		w.Header().Set("Retry-After", "1")
		http.Error(w, "DASH presentation not ready: waiting for a segment from every track", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", mimeMPD)
	// ServeContent (not a bare Write) so the response carries Content-Length and
	// answers Range requests: ffmpeg's DASH demuxer sends "Range: bytes=0-" and
	// cannot finalize a chunked, length-less manifest reply ("Unable to read to
	// manifest"). Browsers (dash.js, Shaka) tolerate the bare write; ffmpeg does
	// not. modtime zero disables Last-Modified/caching, which the no-store header
	// already forbids.
	http.ServeContent(w, r, "live.mpd", time.Time{}, bytes.NewReader(s.pub.DASHManifest()))
}

func (s *Server) master(w http.ResponseWriter, r *http.Request) {
	s.pub.WaitReady(r.Context(), readyTimeout)
	writePlaylist(w, s.pub.masterPlaylist())
}

func (s *Server) media(w http.ResponseWriter, r *http.Request) {
	t, ok := s.pub.Track(r.PathValue("track"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	writePlaylist(w, t.mediaPlaylist())
}

func (s *Server) initSeg(w http.ResponseWriter, r *http.Request) {
	t, ok := s.pub.Track(r.PathValue("track"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	data, modtime, ok := t.initSegment()
	if !ok {
		http.NotFound(w, r)
		return
	}
	cors(w)
	w.Header().Set("Content-Type", "video/mp4")
	http.ServeContent(w, r, "init.mp4", modtime, bytes.NewReader(data))
}

func (s *Server) segment(w http.ResponseWriter, r *http.Request) {
	t, ok := s.pub.Track(r.PathValue("track"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	name := r.PathValue("seg")
	data, path, modtime, ok := t.segmentByName(name)
	if !ok {
		// Aged out of the window, or never existed. 404 is correct: a live
		// client should already have advanced to a newer segment.
		http.NotFound(w, r)
		return
	}
	cors(w)
	w.Header().Set("Content-Type", segmentMIME(name))
	if path != "" {
		// VOD: bytes live in a spool file, fully written before it was listed.
		// ServeFile keeps Range/conditional handling and streams from disk.
		http.ServeFile(w, r, path)
		return
	}
	http.ServeContent(w, r, name, modtime, bytes.NewReader(data))
}

func writePlaylist(w http.ResponseWriter, body []byte) {
	cors(w)
	w.Header().Set("Content-Type", mimeM3U8)
	// Playlists change every segment; never let a cache pin a stale window.
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Write(body)
}

// NewTSServer serves a continuous MPEG-TS broadcast:
//
//	GET /live.ts → one never-ending transport stream, fanned out from the
//	              shared broadcaster; each viewer joins on a segment boundary.
func NewTSServer(b *TSBroadcaster) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /live.ts", func(w http.ResponseWriter, r *http.Request) {
		if b.Ended() {
			// The broadcast is over and holds no rewind buffer: an empty 200
			// that immediately closes just looks like a broken stream to a
			// player, so refuse the join outright.
			http.NotFound(w, r)
			return
		}
		cors(w)
		w.Header().Set("Content-Type", "video/mp2t")
		w.Header().Set("Cache-Control", "no-cache, no-store")
		flusher, _ := w.(http.Flusher)
		if flusher != nil {
			flusher.Flush() // send headers now; the first media may be seconds away
		}

		sub := b.Subscribe()
		defer b.Unsubscribe(sub)
		write := func(seg []byte) bool {
			sub.queued.Add(-int64(len(seg)))
			if _, err := w.Write(seg); err != nil {
				return false // client went away
			}
			if flusher != nil {
				flusher.Flush()
			}
			return true
		}
		for {
			// A kicked subscriber's queue has a gap after it (the unit that
			// overflowed was never queued), so check kicked before draining
			// more data — bytes past the gap would corrupt the viewer's TS.
			select {
			case <-sub.kicked:
				return // fell too far behind
			default:
			}
			select {
			case seg := <-sub.data:
				if !write(seg) {
					return
				}
			case <-sub.kicked:
				return // fell too far behind
			case <-b.done:
				// Stream ended: deliver what was already queued (the finite
				// tail) instead of cutting the viewer off mid-buffer.
				for {
					select {
					case seg := <-sub.data:
						if !write(seg) {
							return
						}
					default:
						return
					}
				}
			case <-r.Context().Done():
				return // client disconnected
			}
		}
	})
	return mux
}

func segmentMIME(name string) string {
	if strings.HasSuffix(name, ".ts") {
		return "video/mp2t"
	}
	return "video/mp4" // .m4s and friends
}

// cors lets browser players (hls.js, Shaka) fetch from a different origin.
func cors(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
}
