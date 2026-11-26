package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"path"
	"strings"

	"github.com/m314dev/m314dl/internal/httpx"
	"github.com/m314dev/m314dl/internal/manifest"
	"github.com/m314dev/m314dl/internal/pick"
	"github.com/m314dev/m314dl/internal/source"
)

// POST /api/probe reads a source the way a channel start would and reports
// the tracks, without starting anything. An operator used to create a channel
// blind and learn what the source carried, which qualities and languages, by
// watching it run. With the probe the form can show the real tracks and let
// them pick before anything exists.

type probeReq struct {
	URL      string            `json:"url"`
	Headers  map[string]string `json:"headers"`
	Proxy    string            `json:"proxy"`
	Insecure bool              `json:"insecure"`
}

// probeTrack is one track as the selection sees it. The id is what sv, sa and
// ss match with "id=...". Fields that do not apply to a kind are left out.
type probeTrack struct {
	ID        string  `json:"id"`
	Width     int     `json:"width,omitempty"`
	Height    int     `json:"height,omitempty"`
	Bandwidth int64   `json:"bandwidth,omitempty"`
	Codecs    string  `json:"codecs,omitempty"`
	FPS       float64 `json:"fps,omitempty"`
	Language  string  `json:"language,omitempty"`
	Name      string  `json:"name,omitempty"`
	Channels  string  `json:"channels,omitempty"`
	Format    string  `json:"format,omitempty"` // subtitles: webvtt, ttml, or the fMP4 codec (wvtt, stpp...)
	Default   bool    `json:"default,omitempty"`
}

type probeOut struct {
	Kind      string       `json:"kind"` // hls | dash | mss | livets
	URL       string       `json:"url"`  // the manifest after redirects
	Live      bool         `json:"live"`
	Video     []probeTrack `json:"video"` // best first, the order the selection uses
	Audio     []probeTrack `json:"audio"`
	Subtitles []probeTrack `json:"subtitles"`
}

func (w *workerServer) apiProbe(rw http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(rw, r.Body, 64<<10)
	var req probeReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.URL == "" {
		http.Error(rw, `bad request: want {"url":"...","headers":{...},"proxy":"...","insecure":false}`, http.StatusBadRequest)
		return
	}
	headers := req.Headers
	if headers == nil {
		headers = map[string]string{}
	}
	// One retry: an operator is waiting at a form.
	client, err := httpx.New(httpx.Options{Headers: headers, Proxy: req.Proxy, Insecure: req.Insecure, Retries: 1})
	if err != nil {
		http.Error(rw, "client: "+err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), patchTimeout)
	defer cancel()
	master, kind, err := source.LoadManifest(ctx, client, req.URL, w.logv)
	if err != nil {
		// The request was fine; the source was not.
		http.Error(rw, "source: "+err.Error(), http.StatusBadGateway)
		return
	}
	httpx.WriteJSON(rw, probeView(master, kind))
}

func probeView(master *manifest.Master, kind string) probeOut {
	out := probeOut{Kind: kind, URL: master.URL, Live: master.Live,
		Video: []probeTrack{}, Audio: []probeTrack{}, Subtitles: []probeTrack{}}
	streams := master.Streams
	if kind == source.LiveTSKind {
		streams = []*manifest.Stream{source.LiveTSTrack(master)}
	}
	pick.Sort(streams)
	for _, st := range streams {
		t := probeTrack{ID: st.ID, Codecs: st.Codecs, Default: st.Default}
		switch st.Type {
		case manifest.Video:
			t.Width, t.Height, t.Bandwidth, t.FPS = st.Width, st.Height, st.Bandwidth, st.FrameRate
			out.Video = append(out.Video, t)
		case manifest.Audio:
			t.Language, t.Name, t.Channels, t.Bandwidth = st.Language, st.Name, st.Channels, st.Bandwidth
			out.Audio = append(out.Audio, t)
		case manifest.Subtitles:
			t.Language, t.Name, t.Codecs, t.Format = st.Language, st.Name, "", subsFormat(st)
			out.Subtitles = append(out.Subtitles, t)
		}
		if st.Live {
			out.Live = true
		}
	}
	return out
}

// subsFormat names what a subtitle track carries. An fMP4 track says so in its
// codec; a plain one is WebVTT unless its segments say TTML. HLS subtitles are
// WebVTT by the spec.
func subsFormat(st *manifest.Stream) string {
	if st.Codecs != "" {
		return st.Codecs
	}
	if len(st.Segments) > 0 {
		u := st.Segments[0].URL
		if i := strings.IndexByte(u, '?'); i >= 0 {
			u = u[:i]
		}
		switch strings.ToLower(path.Ext(u)) {
		case ".ttml", ".xml", ".dfxp":
			return "ttml"
		}
	}
	return "webvtt"
}
