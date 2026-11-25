package worker

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/m314dev/m314dl/internal/httpx"
	"github.com/m314dev/m314dl/internal/manifest"
	"github.com/m314dev/m314dl/internal/restream"
	"github.com/m314dev/m314dl/internal/serve"
)

type nopPres struct{}

func (nopPres) End()                    {}
func (nopPres) StatusLine() string      { return "" }
func (nopPres) Stats() []restream.Stats { return nil }

// signedOrigin is a live HLS source that signs every URL with a token, the
// way a provider does: the playlist and each segment carry ?tok=..., and a
// request with a token the origin has revoked gets 403. The window moves on by
// itself every advanceEvery.
type signedOrigin struct {
	dir string

	mu      sync.Mutex
	seq     int64           // media sequence of the first segment in the window
	revoked map[string]bool // tokens no longer accepted
	byToken map[string]int  // requests served per token
}

const (
	originWindow  = 3
	originSegDur  = 0.3 // seconds, so the test moves fast
	originSegsAll = 7   // seg0000.m4s .. seg0006.m4s in the fixture
)

func (o *signedOrigin) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	tok := r.URL.Query().Get("tok")
	o.mu.Lock()
	if tok == "" || o.revoked[tok] {
		o.mu.Unlock()
		http.Error(rw, "bad token", http.StatusForbidden)
		return
	}
	o.byToken[tok]++
	first := o.seq
	o.mu.Unlock()

	switch {
	case strings.HasSuffix(r.URL.Path, "/live.m3u8"):
		var b strings.Builder
		fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:%d\n#EXT-X-MAP:URI=\"init.mp4?tok=%s\"\n", first, tok)
		for i := int64(0); i < originWindow; i++ {
			fmt.Fprintf(&b, "#EXTINF:%.3f,\nseg%d.m4s?tok=%s\n", originSegDur, first+i, tok)
		}
		rw.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		rw.Write([]byte(b.String()))
	case strings.HasSuffix(r.URL.Path, "/init.mp4"):
		http.ServeFile(rw, r, filepath.Join(o.dir, "init.mp4"))
	case strings.HasPrefix(filepath.Base(r.URL.Path), "seg"):
		var n int64
		fmt.Sscanf(filepath.Base(r.URL.Path), "seg%d.m4s", &n)
		http.ServeFile(rw, r, filepath.Join(o.dir, fmt.Sprintf("seg%04d.m4s", n%originSegsAll)))
	default:
		http.NotFound(rw, r)
	}
}

func (o *signedOrigin) advance() {
	o.mu.Lock()
	o.seq++
	o.mu.Unlock()
}

func (o *signedOrigin) revoke(tok string) {
	o.mu.Lock()
	o.revoked[tok] = true
	o.mu.Unlock()
}

func (o *signedOrigin) served(tok string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.byToken[tok]
}

func getView(t *testing.T, w *workerServer, id string) channelView {
	t.Helper()
	rec := workerReq(t, w, "GET", "/api/channels/"+id, "", "")
	if rec.Code != 200 {
		t.Fatalf("view: %d %s", rec.Code, rec.Body)
	}
	var v channelView
	json.Unmarshal(rec.Body.Bytes(), &v)
	return v
}

// The whole point of PATCH: the source URL is replaced with a freshly signed
// one, the old token dies, and the output carries on in the same sequence
// with nothing published twice.
func TestPatchReplacesSourceWithoutRepublishing(t *testing.T) {
	dir := "../../bench/fixtures/hls-fmp4"
	if _, err := os.Stat(filepath.Join(dir, "media.m3u8")); err != nil {
		t.Skip("fixtures not present")
	}
	origin := &signedOrigin{dir: dir, revoked: map[string]bool{}, byToken: map[string]int{}}
	src := httptest.NewServer(origin)
	defer src.Close()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		tick := time.NewTicker(time.Duration(originSegDur * float64(time.Second)))
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				origin.advance()
			}
		}
	}()

	w := newWorkerServer("", 32, "", "test", 0, nil)
	rec := workerReq(t, w, "POST", "/api/channels", `{"id":"c","url":"`+src.URL+`/live.m3u8?tok=one"}`, "")
	if rec.Code != 200 {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	defer workerReq(t, w, "DELETE", "/api/channels/c", "", "")

	// Let it publish a few segments on the first token.
	var before channelView
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		before = getView(t, w, "c")
		if len(before.Tracks) > 0 && before.Tracks[0].Published >= 4 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(before.Tracks) == 0 || before.Tracks[0].Published < 4 {
		t.Fatalf("channel never got going: %+v", before)
	}
	if !before.Output.Publishing || before.Health != "ok" || before.Source.Bytes == 0 {
		t.Fatalf("view before the swap: %+v", before)
	}

	// Swap to a new token and kill the old one. The origin also moved on by
	// about one segment per tick since `before` was read.
	seqAtSwap := origin.seq
	rec = workerReq(t, w, "PATCH", "/api/channels/c", `{"url":"`+src.URL+`/live.m3u8?tok=two"}`, "")
	if rec.Code != 200 {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body)
	}
	origin.revoke("one")
	var after channelView
	json.Unmarshal(rec.Body.Bytes(), &after)
	if after.Source.Swaps != 1 || !strings.Contains(after.URL, "tok=two") {
		t.Fatalf("view right after the patch: %+v", after)
	}

	// Wait for several segments to arrive on the new token.
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		after = getView(t, w, "c")
		if after.Tracks[0].Published >= before.Tracks[0].Published+4 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if origin.served("two") == 0 {
		t.Fatal("nothing was fetched with the new token")
	}
	// Every segment the origin advanced through since `before` was read is one
	// new publish, give or take the one in flight either side. Republishing
	// the window after the swap would add originWindow more.
	advanced := origin.seq - seqAtSwap
	got := after.Tracks[0].Published - before.Tracks[0].Published
	if got < 4 || got > advanced+2 {
		t.Fatalf("published %d segments across the swap, the origin advanced %d: the window was republished", got, advanced)
	}
	if after.Tracks[0].Seq <= before.Tracks[0].Seq || after.Health != "ok" {
		t.Fatalf("output sequence or health broke across the swap: before %+v after %+v", before.Tracks[0], after)
	}
	if after.Source.Restarts != 0 {
		t.Fatalf("a swap is not a restart: %+v", after.Source)
	}

	// The log says so.
	if rec := workerReq(t, w, "GET", "/api/channels/c/log", "", ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), "input replaced") {
		t.Fatalf("log: %d %s", rec.Code, rec.Body)
	}
}

func TestPatchRefusesOutputFieldsAndDeadChannels(t *testing.T) {
	w := newWorkerServer("", 32, "", "test", 0, nil)
	if rec := workerReq(t, w, "PATCH", "/api/channels/none", `{"url":"http://x"}`, ""); rec.Code != 404 {
		t.Fatalf("unknown channel: %d", rec.Code)
	}
	// A channel that is already over.
	client, _ := httpx.New(httpx.Options{})
	dead := &channel{id: "dead", state: "error", errMsg: "gone", client: client, pres: nopPres{}, viewers: serve.NewViewers(viewerWindow)}
	w.channels["dead"] = dead
	if rec := workerReq(t, w, "PATCH", "/api/channels/dead", `{"url":"http://x"}`, ""); rec.Code != 409 {
		t.Fatalf("dead channel: %d %s", rec.Code, rec.Body)
	}
	if rec := workerReq(t, w, "PATCH", "/api/channels/dead", `{"format":"ts"}`, ""); rec.Code != 400 {
		t.Fatalf("output field: %d %s", rec.Code, rec.Body)
	}
	if rec := workerReq(t, w, "PATCH", "/api/channels/dead", `{"url":""}`, ""); rec.Code != 400 {
		t.Fatalf("empty url: %d %s", rec.Code, rec.Body)
	}
}

func TestCompatible(t *testing.T) {
	v := &manifest.Stream{ID: "v1", Type: manifest.Video, Codecs: "avc1.64001f", Width: 1280, Height: 720}
	a := &manifest.Stream{ID: "a1", Type: manifest.Audio, Codecs: "mp4a.40.2"}
	cur := []*manifest.Stream{v, a}
	same := func(mod func(*manifest.Stream)) []*manifest.Stream {
		nv, na := *v, *a
		mod(&nv)
		return []*manifest.Stream{&nv, &na}
	}
	if err := compatible(cur, same(func(*manifest.Stream) {})); err != nil {
		t.Fatalf("identical: %v", err)
	}
	cases := map[string]func(*manifest.Stream){
		"missing":    func(s *manifest.Stream) { s.ID = "v9" },
		"codec":      func(s *manifest.Stream) { s.Codecs = "hvc1.1.6.L93" },
		"resolution": func(s *manifest.Stream) { s.Width, s.Height = 1920, 1080 },
		"type":       func(s *manifest.Stream) { s.Type = manifest.Audio },
	}
	for name, mod := range cases {
		if err := compatible(cur, same(mod)); err == nil {
			t.Errorf("%s change accepted", name)
		}
	}
	// Extra tracks in the new source are fine; only the playing ones matter.
	if err := compatible(cur, append(same(func(*manifest.Stream) {}), &manifest.Stream{ID: "v2"})); err != nil {
		t.Fatalf("extra track: %v", err)
	}
}

func TestHealth(t *testing.T) {
	seg := func(age int64, realtime float64) []trackView {
		return []trackView{{Type: "video", LastSegmentAgeMS: age, Realtime: realtime, targetDur: 4}}
	}
	cases := []struct {
		name   string
		state  string
		uptime time.Duration
		tracks []trackView
		errs   int64
		want   string
	}{
		{"idle on demand", "idle", time.Hour, nil, 0, "idle"},
		{"failed", "error", time.Hour, nil, 0, "down"},
		{"finished vod", "done", time.Hour, seg(3600000, 1), 0, "ok"},
		{"starting up", "running", 5 * time.Second, nil, 0, "ok"},
		{"never published", "running", 2 * time.Minute, nil, 0, "down"},
		{"fresh segment", "running", time.Hour, seg(1500, 1.02), 0, "ok"},
		{"stalled", "running", time.Hour, seg(13000, 1), 0, "stalled"},
		{"stalled source failing", "running", time.Hour, seg(70000, 1), 3, "down"},
		{"falling behind", "running", time.Hour, seg(1500, 0.8), 0, "degraded"},
		{"error storm", "running", time.Hour, seg(1500, 1), 25, "degraded"},
		{"raw ts fresh", "running", time.Hour, []trackView{{Type: "video", LastSegmentAgeMS: 9000}}, 0, "ok"},
		{"raw ts stalled", "running", time.Hour, []trackView{{Type: "video", LastSegmentAgeMS: 16000}}, 0, "stalled"},
	}
	for _, c := range cases {
		kind := "hls"
		if strings.HasPrefix(c.name, "raw ts") {
			kind = "livets"
		}
		if got := health(c.state, kind, c.uptime, c.tracks, c.errs); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}
