package restream

import (
	"bytes"
	"net/http"
	"strings"
	"time"
)

// Server exposes a Publisher over HTTP as a live HLS origin. HLS is HTTP Live
// Streaming: a player fetches a playlist, then the short media files it lists.
//
//	GET /live.m3u8          the multivariant (master) playlist
//	GET /{track}/index.m3u8 one track's media playlist
//	GET /{track}/init.mp4   one track's fMP4 init segment (the EXT-X-MAP target)
//	GET /{track}/{seg}      one media segment (000123.ts or .m4s)
//
// Playlists are built fresh for every request and sent with no-cache. Segments
// go out through http.ServeContent, so players get Range, ETag and conditional
// requests for free, straight from the shared in-memory window. One copy of
// each segment feeds every viewer.
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

// DASHHandler serves the same in-memory window as a DASH presentation. DASH is
// the MPEG streaming format; its manifest is an XML file called an MPD.
//
//	GET /live.mpd         the manifest (SegmentTemplate plus SegmentTimeline)
//	GET /{track}/init.mp4 init segment (shared with the HLS handler)
//	GET /{track}/{seg}    one media segment (shared with the HLS handler)
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
// of every track. It stays under every common player's manifest timeout
// (dash.js 10s, Shaka 30s). Past it, an incomplete DASH presentation returns a
// 503 that the player can retry.
const readyTimeout = 8 * time.Second

func (s *Server) mpd(w http.ResponseWriter, r *http.Request) {
	s.pub.WaitReady(r.Context(), readyTimeout)
	cors(w)
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	if !s.pub.Ready() {
		// A timeout (or a failed download) does not make an empty timeline
		// valid. Never let a player initialize successfully on an MPD it
		// cannot play.
		w.Header().Set("Retry-After", "1")
		http.Error(w, "DASH presentation not ready: waiting for a segment from every track", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", mimeMPD)
	// ServeContent instead of a bare Write, so the response carries
	// Content-Length and answers Range requests. ffmpeg's DASH demuxer sends
	// "Range: bytes=0-" and cannot finish a chunked manifest reply that has no
	// length ("Unable to read to manifest"). Browsers (dash.js, Shaka) accept
	// the bare write; ffmpeg does not. A zero modtime turns off Last-Modified
	// and caching, which the no-store header already forbids.
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
		// The segment aged out of the window, or never existed. 404 is right:
		// a live client should already have moved on to a newer segment.
		http.NotFound(w, r)
		return
	}
	cors(w)
	w.Header().Set("Content-Type", segmentMIME(name))
	if path != "" {
		// VOD: the bytes live in a spool file that was fully written before
		// the segment was listed. ServeFile keeps the Range and conditional
		// handling and streams from disk.
		http.ServeFile(w, r, path)
		return
	}
	http.ServeContent(w, r, name, modtime, bytes.NewReader(data))
}

func writePlaylist(w http.ResponseWriter, body []byte) {
	cors(w)
	w.Header().Set("Content-Type", mimeM3U8)
	// Playlists change with every segment. Never let a cache hold on to a
	// stale window.
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Write(body)
}

// NewTSServer serves a continuous MPEG-TS (transport stream) broadcast:
//
//	GET /live.ts  one never-ending transport stream, fanned out from the
//	              shared broadcaster. Each viewer joins on a segment boundary.
func NewTSServer(b *TSBroadcaster) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /live.ts", func(w http.ResponseWriter, r *http.Request) {
		if b.Ended() {
			// The broadcast is over and keeps no rewind buffer. An empty 200
			// that closes at once looks like a broken stream to a player, so
			// refuse the join outright.
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
			// more data. Bytes past the gap would corrupt the viewer's TS.
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
				// tail) instead of cutting the viewer off partway through
				// its buffer.
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
