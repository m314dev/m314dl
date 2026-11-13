package worker

import (
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// liveSource serves a raw live transport stream: numbered TS packets,
// chunked, until the client hangs up. It counts the connections that are open.
func liveSource(open *atomic.Int32) *httptest.Server {
	return httptest.NewServer(liveTS(open))
}

// liveTS is liveSource's handler.
func liveTS(open *atomic.Int32) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		open.Add(1)
		defer open.Add(-1)
		w.Header().Set("Content-Type", "video/mp2t")
		w.(http.Flusher).Flush()
		pkt := make([]byte, 188)
		for n := 0; r.Context().Err() == nil; n++ {
			pkt[0], pkt[1], pkt[2], pkt[3] = 0x47, 0x01, 0x00, 0x10
			binary.BigEndian.PutUint32(pkt[4:], uint32(n))
			if _, err := w.Write(pkt); err != nil {
				return
			}
			if n%100 == 99 {
				w.(http.Flusher).Flush()
				time.Sleep(time.Millisecond)
			}
		}
	}
}

// watchTS reads n packets from a channel's /live.ts and checks they are whole
// and consecutive: the viewer may join anywhere, but nothing is torn or lost.
func watchTS(t *testing.T, url string, n int) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "video/mp2t" {
		t.Fatalf("GET %s: %d %s", url, resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	b := make([]byte, n*188)
	if _, err := io.ReadFull(resp.Body, b); err != nil {
		t.Fatalf("read %d packets: %v", n, err)
	}
	first := binary.BigEndian.Uint32(b[4:])
	for i := 0; i < n; i++ {
		p := b[i*188:]
		if p[0] != 0x47 || binary.BigEndian.Uint32(p[4:]) != first+uint32(i) {
			t.Fatalf("packet %d: sync %#x number %d, want number %d", i, p[0], binary.BigEndian.Uint32(p[4:]), first+uint32(i))
		}
	}
}

func waitOpen(t *testing.T, open *atomic.Int32, want int32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for open.Load() != want {
		if time.Now().After(deadline) {
			t.Fatalf("%d connections open at the source, want %d", open.Load(), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A raw live TS source restreams as TS: the body is piped to the viewer as it
// arrives, with nothing spooled to disk. An output that needs segments is
// refused at start. Stopping the channel closes the connection to the source.
func TestWorkerLiveTSChannel(t *testing.T) {
	var open atomic.Int32
	origin := liveSource(&open)
	defer origin.Close()
	src := origin.URL + "/live.ts"
	w := newWorkerServer("", 32, "", "test", 0, nil)

	rec := workerReq(t, w, "POST", "/api/channels", `{"id":"bad","url":"`+src+`","format":"hls"}`, "")
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "only be restreamed as ts") {
		t.Fatalf("hls output of a raw TS source: %d %s", rec.Code, rec.Body)
	}
	rec = workerReq(t, w, "POST", "/api/channels", `{"id":"tv","url":"`+src+`","format":"ts"}`, "")
	if rec.Code != 200 {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	var v channelView
	json.Unmarshal(rec.Body.Bytes(), &v)
	if v.State != "running" || !v.Live || v.Media != "/tv/live.ts" {
		t.Fatalf("channel view: %+v", v)
	}

	media := httptest.NewServer(w.handler()) // a real server: /live.ts never ends
	defer media.Close()
	watchTS(t, media.URL+"/tv/live.ts", 2000)
	if left, _ := os.ReadDir(w.channel("tv").tmpDir); len(left) != 0 {
		t.Fatalf("the channel spooled %d files; a raw TS source must not touch the disk", len(left))
	}
	waitOpen(t, &open, 1)

	if rec := workerReq(t, w, "DELETE", "/api/channels/tv", "", ""); rec.Code != 200 {
		t.Fatalf("stop: %d", rec.Code)
	}
	waitOpen(t, &open, 0)
}

// On demand, the source is not held open while nobody watches: creating the
// channel only probes the URL, and the first viewer starts the pipe.
func TestWorkerLiveTSOnDemand(t *testing.T) {
	var open atomic.Int32
	origin := liveSource(&open)
	defer origin.Close()
	w := newWorkerServer("", 32, "", "test", 0, nil)

	rec := workerReq(t, w, "POST", "/api/channels", `{"id":"od","url":"`+origin.URL+`/stream","format":"ts","idle":"5s"}`, "")
	if rec.Code != 200 {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	var v channelView
	json.Unmarshal(rec.Body.Bytes(), &v)
	if v.State != "idle" || !v.Live {
		t.Fatalf("new on-demand channel: %+v", v)
	}
	waitOpen(t, &open, 0) // the probe is closed again

	media := httptest.NewServer(w.handler())
	defer media.Close()
	watchTS(t, media.URL+"/od/live.ts", 2000)
	json.Unmarshal(workerReq(t, w, "GET", "/api/channels/od", "", "").Body.Bytes(), &v)
	if v.State != "running" {
		t.Fatalf("after the first viewer: %+v", v)
	}
	if rec := workerReq(t, w, "DELETE", "/api/channels/od", "", ""); rec.Code != 200 {
		t.Fatalf("stop: %d", rec.Code)
	}
	waitOpen(t, &open, 0)
}

// An on-demand channel loads its source again at every start. A source that
// has turned from a live MPEG-TS stream into a manifest, or the other way
// round, is refused there: the run would put it on the wrong path, and a live
// TS in the segment engine is a download that never ends.
func TestWorkerOnDemandKindChange(t *testing.T) {
	const playlist = "#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:0\n#EXTINF:2.0,\nseg0.ts\n"
	for _, c := range []struct {
		name, format, media, want string
		tsFirst                   bool
	}{
		{"live ts to manifest", "ts", "/live.ts", "changed from livets to hls", true},
		{"manifest to live ts", "hls", "/live.m3u8", "changed from hls to livets", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			var open atomic.Int32
			var ts atomic.Bool
			ts.Store(c.tsFirst)
			stream := liveTS(&open)
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if ts.Load() {
					stream(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
				io.WriteString(w, playlist)
			}))
			defer origin.Close()
			w := newWorkerServer("", 32, "", "test", 0, nil)
			rec := workerReq(t, w, "POST", "/api/channels", `{"id":"od","url":"`+origin.URL+`/src","format":"`+c.format+`","idle":"5s"}`, "")
			if rec.Code != 200 {
				t.Fatalf("start: %d %s", rec.Code, rec.Body)
			}
			ts.Store(!c.tsFirst)

			media := httptest.NewServer(w.handler())
			defer media.Close()
			resp, err := http.Get(media.URL + "/od" + c.media)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), c.want) {
				t.Fatalf("first viewer: %d %s, want 503 saying %q", resp.StatusCode, body, c.want)
			}
			if left, _ := os.ReadDir(w.channel("od").tmpDir); len(left) != 0 {
				t.Fatalf("the refused start left %d files in the spool", len(left))
			}
			waitOpen(t, &open, 0)
			if rec := workerReq(t, w, "DELETE", "/api/channels/od", "", ""); rec.Code != 200 {
				t.Fatalf("stop: %d", rec.Code)
			}
		})
	}
}
