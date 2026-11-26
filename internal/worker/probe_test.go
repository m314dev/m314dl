package worker

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

const probeMaster = `#EXTM3U
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud",NAME="English",LANGUAGE="en",DEFAULT=YES,CHANNELS="2",URI="a-en.m3u8"
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud",NAME="Arabic",LANGUAGE="ar",URI="a-ar.m3u8"
#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subs",NAME="English",LANGUAGE="en",URI="s-en.m3u8"
#EXT-X-STREAM-INF:BANDWIDTH=1500000,RESOLUTION=1280x720,FRAME-RATE=25.000,CODECS="avc1.64001f,mp4a.40.2",AUDIO="aud",SUBTITLES="subs"
v720.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=4000000,RESOLUTION=1920x1080,FRAME-RATE=50.000,CODECS="avc1.640028,mp4a.40.2",AUDIO="aud",SUBTITLES="subs"
v1080.m3u8
`

// A probe reads the master and reports the tracks grouped and ordered the way
// the selection sees them, and starts nothing.
func TestProbeListsTracks(t *testing.T) {
	hits := 0
	src := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path != "/master.m3u8" {
			http.NotFound(rw, r)
			return
		}
		rw.Write([]byte(probeMaster))
	}))
	defer src.Close()

	w := newWorkerServer("tok", 32, "", "test", 0, nil)
	if rec := workerReq(t, w, "POST", "/api/probe", `{"url":"`+src.URL+`/master.m3u8"}`, ""); rec.Code != 401 {
		t.Fatalf("no token: %d", rec.Code)
	}
	if rec := workerReq(t, w, "POST", "/api/probe", `{}`, "tok"); rec.Code != 400 {
		t.Fatalf("no url: %d", rec.Code)
	}
	if rec := workerReq(t, w, "POST", "/api/probe", `{"url":"`+src.URL+`/nope.m3u8"}`, "tok"); rec.Code != 502 {
		t.Fatalf("dead source: %d %s", rec.Code, rec.Body)
	}
	rec := workerReq(t, w, "POST", "/api/probe", `{"url":"`+src.URL+`/master.m3u8"}`, "tok")
	if rec.Code != 200 {
		t.Fatalf("probe: %d %s", rec.Code, rec.Body)
	}
	var out probeOut
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Kind != "hls" || out.Live {
		t.Fatalf("kind/live: %+v", out)
	}
	if len(out.Video) != 2 || out.Video[0].Height != 1080 || out.Video[0].FPS != 50 || out.Video[0].Codecs != "avc1.640028,mp4a.40.2" || out.Video[1].Bandwidth != 1500000 {
		t.Fatalf("video: %+v", out.Video)
	}
	if len(out.Audio) != 2 || out.Audio[0].Language != "en" || out.Audio[0].Channels != "2" || !out.Audio[0].Default || out.Audio[1].Name != "Arabic" {
		t.Fatalf("audio: %+v", out.Audio)
	}
	if len(out.Subtitles) != 1 || out.Subtitles[0].Language != "en" || out.Subtitles[0].Format != "webvtt" {
		t.Fatalf("subtitles: %+v", out.Subtitles)
	}
	if n := len(workerReq(t, w, "GET", "/api/channels", "", "tok").Body.String()); n > 3 {
		t.Fatal("a probe created a channel")
	}
	// Only the master was read: no media playlist, no segment.
	if hits != 2 {
		t.Fatalf("origin hits %d, want 2 (one 404, one master)", hits)
	}
}
