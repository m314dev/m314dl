package worker

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// A WebVTT rendition selected with ss comes out as a SUBTITLES rendition in
// the channel's master playlist, with segments a player can fetch.
func TestChannelRepublishesSubtitles(t *testing.T) {
	dir := "../../bench/fixtures/hls-fmp4"
	if _, err := os.Stat(dir + "/media.m3u8"); err != nil {
		t.Skip("fixtures not present")
	}
	fixture := http.FileServer(http.Dir(dir))
	src := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/master.m3u8":
			rw.Write([]byte("#EXTM3U\n" +
				"#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID=\"subs\",NAME=\"English\",LANGUAGE=\"en\",URI=\"sub.m3u8\"\n" +
				"#EXT-X-STREAM-INF:BANDWIDTH=1000000,RESOLUTION=640x360,CODECS=\"avc1.64001e\",SUBTITLES=\"subs\"\nmedia.m3u8\n"))
		case r.URL.Path == "/sub.m3u8":
			var b strings.Builder
			b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-PLAYLIST-TYPE:VOD\n")
			for i := 0; i < 3; i++ {
				fmt.Fprintf(&b, "#EXTINF:4.000,\ncue%d.vtt\n", i)
			}
			b.WriteString("#EXT-X-ENDLIST\n")
			rw.Write([]byte(b.String()))
		case strings.HasSuffix(r.URL.Path, ".vtt"):
			rw.Header().Set("Content-Type", "text/vtt")
			fmt.Fprintf(rw, "WEBVTT\nX-TIMESTAMP-MAP=MPEGTS:900000,LOCAL:00:00:00.000\n\n00:00:00.000 --> 00:00:01.000\n%s\n", r.URL.Path)
		default:
			fixture.ServeHTTP(rw, r)
		}
	}))
	defer src.Close()

	w := newWorkerServer("", 32, "", "test", 0, nil)
	rec := workerReq(t, w, "POST", "/api/channels", `{"id":"s","url":"`+src.URL+`/master.m3u8","ss":"all"}`, "")
	if rec.Code != 200 {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	defer workerReq(t, w, "DELETE", "/api/channels/s", "", "")

	master := workerReq(t, w, "GET", "/s/live.m3u8", "", "").Body.String()
	for _, want := range []string{`#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subs"`, `LANGUAGE="en"`, `URI="sub-en/index.m3u8"`, `SUBTITLES="subs"`} {
		if !strings.Contains(master, want) {
			t.Fatalf("master lacks %q:\n%s", want, master)
		}
	}
	var pl string
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if rec := workerReq(t, w, "GET", "/s/sub-en/index.m3u8", "", ""); rec.Code == 200 && strings.Contains(rec.Body.String(), "#EXT-X-ENDLIST") {
			pl = rec.Body.String()
			break
		}
	}
	if !strings.Contains(pl, "000002.vtt") || strings.Contains(pl, "EXT-X-MAP") {
		t.Fatalf("subtitle playlist:\n%s", pl)
	}
	seg := workerReq(t, w, "GET", "/s/sub-en/000001.vtt", "", "")
	if seg.Code != 200 || seg.Header().Get("Content-Type") != "text/vtt" || !strings.Contains(seg.Body.String(), "WEBVTT") || !strings.Contains(seg.Body.String(), "cue1.vtt") {
		t.Fatalf("subtitle segment: %d %s %q", seg.Code, seg.Header().Get("Content-Type"), seg.Body.String())
	}
	v := getView(t, w, "s")
	found := false
	for _, tr := range v.Tracks {
		if tr.ID == "sub-en" && tr.Type == "subtitles" && tr.Published == 3 {
			found = true
		}
	}
	if !found {
		t.Fatalf("tracks: %+v", v.Tracks)
	}
}
