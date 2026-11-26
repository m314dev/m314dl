package worker

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func signed(secret, p string, exp int64) string {
	return p + "?exp=" + strconv.FormatInt(exp, 10) + "&sig=" + signPath(secret, p, exp)
}

// With a media secret, the entry URL must be signed, and everything the
// worker writes into a playlist is signed on the way out so a player can
// follow it.
func TestSignedMediaURLs(t *testing.T) {
	if _, err := os.Stat("../../bench/fixtures/hls-fmp4/media.m3u8"); err != nil {
		t.Skip("fixtures not present")
	}
	src := httptest.NewServer(http.FileServer(http.Dir("../../bench/fixtures")))
	defer src.Close()
	w := newWorkerServer("", 32, "", "test", 0, nil)
	w.mediaSecret = "k"
	for _, c := range []string{`{"id":"h","url":"` + src.URL + `/hls-fmp4/media.m3u8"}`, `{"id":"d","url":"` + src.URL + `/hls-fmp4/media.m3u8","format":"dash"}`} {
		if rec := workerReq(t, w, "POST", "/api/channels", c, ""); rec.Code != 200 {
			t.Fatalf("start: %d %s", rec.Code, rec.Body)
		}
	}
	defer workerReq(t, w, "DELETE", "/api/channels/h", "", "")
	defer workerReq(t, w, "DELETE", "/api/channels/d", "", "")
	exp := time.Now().Add(time.Minute).Unix()

	if rec := workerReq(t, w, "GET", "/h/live.m3u8", "", ""); rec.Code != 403 {
		t.Fatalf("unsigned: %d", rec.Code)
	}
	if rec := workerReq(t, w, "GET", signed("k", "/h/live.m3u8", time.Now().Add(-time.Second).Unix()), "", ""); rec.Code != 403 {
		t.Fatalf("expired: %d", rec.Code)
	}
	if rec := workerReq(t, w, "GET", signed("wrong", "/h/live.m3u8", exp), "", ""); rec.Code != 403 {
		t.Fatalf("bad signature: %d", rec.Code)
	}
	// A signature for one object does not open another.
	if rec := workerReq(t, w, "GET", "/h/video/index.m3u8?exp="+strconv.FormatInt(exp, 10)+"&sig="+signPath("k", "/h/live.m3u8", exp), "", ""); rec.Code != 403 {
		t.Fatalf("sig for another path: %d", rec.Code)
	}

	rec := workerReq(t, w, "GET", signed("k", "/h/live.m3u8", exp), "", "")
	if rec.Code != 200 {
		t.Fatalf("signed master: %d %s", rec.Code, rec.Body)
	}
	var variant string
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if strings.HasPrefix(line, "video/index.m3u8?exp=") && strings.Contains(line, "&sig=") {
			variant = "/h/" + line
		}
	}
	if variant == "" {
		t.Fatalf("master URIs not signed:\n%s", rec.Body)
	}
	var segment string
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline) && segment == ""; time.Sleep(50 * time.Millisecond) {
		rec = workerReq(t, w, "GET", variant, "", "")
		if rec.Code != 200 {
			t.Fatalf("signed variant: %d %s", rec.Code, rec.Body)
		}
		for _, line := range strings.Split(rec.Body.String(), "\n") {
			if strings.HasSuffix(line, ".m4s?exp="+strconv.FormatInt(exp, 10)+"&sig="+signPath("k", "/h/", exp)) {
				segment = "/h/video/" + line
			}
		}
	}
	if segment == "" || !strings.Contains(rec.Body.String(), `#EXT-X-MAP:URI="init.mp4?exp=`) {
		t.Fatalf("variant URIs not signed:\n%s", rec.Body)
	}
	if rec := workerReq(t, w, "GET", segment, "", ""); rec.Code != 200 || rec.Header().Get("Content-Type") != "video/mp4" {
		t.Fatalf("signed segment: %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}

	// DASH: the template URLs carry the query, XML-escaped, and the length
	// matches the rewritten body.
	rec = workerReq(t, w, "GET", signed("k", "/d/live.mpd", exp), "", "")
	if rec.Code != 200 {
		t.Fatalf("signed mpd: %d %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `initialization="$RepresentationID$/init.mp4?exp=`) || !strings.Contains(body, `.m4s?exp=`+strconv.FormatInt(exp, 10)+`&amp;sig=`) {
		t.Fatalf("mpd URLs not signed:\n%s", body)
	}
	if cl := rec.Header().Get("Content-Length"); cl != "" && cl != strconv.Itoa(len(body)) {
		t.Fatalf("Content-Length %s for %d bytes", cl, len(body))
	}
}
