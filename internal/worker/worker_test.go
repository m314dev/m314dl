package worker

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func workerReq(t *testing.T, w *workerServer, method, path, body, auth string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	if auth != "" {
		r.Header.Set("Authorization", "Bearer "+auth)
	}
	rec := httptest.NewRecorder()
	w.handler().ServeHTTP(rec, r)
	return rec
}

func TestWorkerValidationAndAuth(t *testing.T) {
	w := newWorkerServer("tok", 32, "", "test", 0, nil)

	if rec := workerReq(t, w, "POST", "/api/channels", `{"id":"c","url":"http://x/a.m3u8"}`, ""); rec.Code != 401 {
		t.Fatalf("no token: got %d, want 401", rec.Code)
	}
	if rec := workerReq(t, w, "POST", "/api/channels", `{"url":"http://x/a.m3u8"}`, "tok"); rec.Code != 400 {
		t.Fatalf("missing id: got %d, want 400", rec.Code)
	}
	if rec := workerReq(t, w, "POST", "/api/channels", `{"id":"api","url":"http://x"}`, "tok"); rec.Code != 400 {
		t.Fatalf("reserved id 'api': got %d, want 400", rec.Code)
	}
	if rec := workerReq(t, w, "POST", "/api/channels", `{"id":"bad id","url":"http://x"}`, "tok"); rec.Code != 400 {
		t.Fatalf("bad id chars: got %d, want 400", rec.Code)
	}
	if rec := workerReq(t, w, "GET", "/api/channels/nope", "", "tok"); rec.Code != 404 {
		t.Fatalf("unknown channel: got %d, want 404", rec.Code)
	}
	var h map[string]any
	json.Unmarshal(workerReq(t, w, "GET", "/api/health", "", "tok").Body.Bytes(), &h)
	if h["status"] != "ok" || h["max"].(float64) != 32 {
		t.Fatalf("health: %v", h)
	}
}

// End to end: start a channel from a real HLS fixture, confirm it serves live
// HLS at /{id}/..., appears in the listing, and stops cleanly.
func TestWorkerChannelLifecycle(t *testing.T) {
	if _, err := os.Stat("../../bench/fixtures/hls-fmp4/media.m3u8"); err != nil {
		t.Skip("fixtures not present")
	}
	src := httptest.NewServer(http.FileServer(http.Dir("../../bench/fixtures")))
	defer src.Close()

	w := newWorkerServer("", 32, "", "test", 0, nil)
	body := `{"id":"c1","url":"` + src.URL + `/hls-fmp4/media.m3u8","format":"hls"}`
	rec := workerReq(t, w, "POST", "/api/channels", body, "")
	if rec.Code != 200 {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	var v channelView
	json.Unmarshal(rec.Body.Bytes(), &v)
	if v.ID != "c1" || v.Media != "/c1/live.m3u8" || v.State != "running" {
		t.Fatalf("channel view wrong: %+v", v)
	}

	// The master playlist lists the track right away; wait for real segments to
	// appear in the media playlist.
	deadline := time.Now().Add(10 * time.Second)
	served := false
	for time.Now().Before(deadline) {
		rec := workerReq(t, w, "GET", "/c1/video/index.m3u8", "", "")
		if rec.Code == 200 && strings.Contains(rec.Body.String(), ".m4s") {
			served = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !served {
		t.Fatal("channel never served segments")
	}
	if rec := workerReq(t, w, "GET", "/c1/live.m3u8", "", ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), "video/index.m3u8") {
		t.Fatalf("master playlist: %d %s", rec.Code, rec.Body)
	}

	// Listed.
	var list []channelView
	json.Unmarshal(workerReq(t, w, "GET", "/api/channels", "", "").Body.Bytes(), &list)
	if len(list) != 1 || list[0].ID != "c1" {
		t.Fatalf("list: %+v", list)
	}

	// Stop removes it.
	if rec := workerReq(t, w, "DELETE", "/api/channels/c1", "", ""); rec.Code != 200 {
		t.Fatalf("stop: %d", rec.Code)
	}
	if rec := workerReq(t, w, "GET", "/api/channels/c1", "", ""); rec.Code != 404 {
		t.Fatalf("after stop: got %d, want 404", rec.Code)
	}
	if rec := workerReq(t, w, "GET", "/c1/live.m3u8", "", ""); rec.Code != 404 {
		t.Fatalf("media after stop: got %d, want 404", rec.Code)
	}
}

func TestWorkerRequiresSecretOffLoopback(t *testing.T) {
	if err := ServeWorker("0.0.0.0:0", "", 32, "", "", "test", 0, nil); err == nil || !strings.Contains(err.Error(), "worker-secret") {
		t.Fatalf("got %v, want worker-secret error", err)
	}
}

// An on-demand channel is validated at creation, parked idle, started by the
// first viewer and reported with its idle period and viewer count.
func TestWorkerOnDemandChannel(t *testing.T) {
	if _, err := os.Stat("../../bench/fixtures/hls-fmp4/media.m3u8"); err != nil {
		t.Skip("fixtures not present")
	}
	var fetches atomic.Int32
	src := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		// The fixture is VOD; present it as live so on demand applies.
		if strings.HasSuffix(r.URL.Path, ".m3u8") {
			b, _ := os.ReadFile("../../bench/fixtures/hls-fmp4/media.m3u8")
			s := strings.ReplaceAll(string(b), "#EXT-X-PLAYLIST-TYPE:VOD\n", "")
			s = strings.ReplaceAll(s, "#EXT-X-ENDLIST\n", "")
			rw.Write([]byte(s))
			return
		}
		http.FileServer(http.Dir("../../bench/fixtures/hls-fmp4")).ServeHTTP(rw, r)
	}))
	defer src.Close()

	w := newWorkerServer("", 32, "", "test", 0, nil)
	if rec := workerReq(t, w, "POST", "/api/channels", `{"id":"bad","url":"`+src.URL+`/media.m3u8","idle":"2s"}`, ""); rec.Code != 400 {
		t.Fatalf("idle under the floor should be refused: %d %s", rec.Code, rec.Body)
	}
	rec := workerReq(t, w, "POST", "/api/channels", `{"id":"od","url":"`+src.URL+`/media.m3u8","idle":"5s"}`, "")
	if rec.Code != 200 {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	var v channelView
	json.Unmarshal(rec.Body.Bytes(), &v)
	if v.State != "idle" || v.Idle != "5s" || v.Media != "/od/live.m3u8" {
		t.Fatalf("new on-demand channel: %+v", v)
	}
	before := fetches.Load() // the manifest load at creation, nothing more
	time.Sleep(300 * time.Millisecond)
	if fetches.Load() != before {
		t.Fatal("an idle channel fetched from the source")
	}

	// First viewer: the master playlist answers once the source is up, then
	// segments flow and the view says running with one viewer.
	if rec := workerReq(t, w, "GET", "/od/live.m3u8", "", ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), "video/index.m3u8") {
		t.Fatalf("first viewer: %d %s", rec.Code, rec.Body)
	}
	deadline := time.Now().Add(10 * time.Second)
	served := false
	for time.Now().Before(deadline) {
		if rec := workerReq(t, w, "GET", "/od/video/index.m3u8", "", ""); rec.Code == 200 && strings.Contains(rec.Body.String(), ".m4s") {
			served = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !served {
		t.Fatal("on-demand channel never served segments")
	}
	json.Unmarshal(workerReq(t, w, "GET", "/api/channels/od", "", "").Body.Bytes(), &v)
	if v.State != "running" || v.Viewers != 1 {
		t.Fatalf("running view: %+v", v)
	}
	if rec := workerReq(t, w, "DELETE", "/api/channels/od", "", ""); rec.Code != 200 {
		t.Fatalf("delete: %d", rec.Code)
	}
}
