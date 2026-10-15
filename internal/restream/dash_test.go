package restream

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mohamed/m314dl/internal/engine"
	"github.com/mohamed/m314dl/internal/manifest"
)

func dashPub(t *testing.T, live bool) (*Publisher, engine.Sink, engine.Sink) {
	t.Helper()
	pub := NewPublisher()
	v := &manifest.Stream{Type: manifest.Video, ID: "v", Width: 1920, Height: 1080,
		FrameRate: 25, Codecs: "avc1.640028", Bandwidth: 5_000_000, Init: &manifest.InitMap{URL: "i"}, Live: live}
	a := &manifest.Stream{Type: manifest.Audio, ID: "a", Language: "en", Codecs: "mp4a.40.2",
		Channels: "2", Bandwidth: 128_000, Init: &manifest.InitMap{URL: "i"}, Live: live}
	spool := ""
	if !live {
		spool = t.TempDir()
	}
	vs := pub.AddTrack(TrackFromStream("video", v, live, spool))
	as := pub.AddTrack(TrackFromStream("audio-en", a, live, spool))
	vs.Init([]byte("vi"))
	as.Init([]byte("ai"))
	return pub, vs, as
}

// mpdAttr pulls one attribute value out of a rendered MPD.
func mpdAttr(t *testing.T, m, name string) string {
	t.Helper()
	i := strings.Index(m, name+`="`)
	if i < 0 {
		t.Fatalf("MPD missing %s\n%s", name, m)
	}
	rest := m[i+len(name)+2:]
	return rest[:strings.IndexByte(rest, '"')]
}

// A VOD still downloading is presented like a live channel: dynamic, with
// availabilityStartTime at the real run start so every player's wall-clock
// live-edge math lands inside the spool, and no timeShiftBufferDepth so t=0
// stays seekable. Back-dating AST (tried once) made dash.js seek ~24h in.
func TestDASHManifestVODGrowing(t *testing.T) {
	pub, vs, as := dashPub(t, false)
	feed(vs, 900, 6.0)
	feed(as, 900, 6.0)
	// no End: the source is still downloading

	m := string(pub.DASHManifest())
	for _, want := range []string{
		`type="dynamic"`,
		`minimumUpdatePeriod="PT6S"`,
		`suggestedPresentationDelay="PT18S"`,
		`startNumber="0"`,
		`<S t="0" d="6000" r="899"/>`, // full spooled timeline from t=0
	} {
		if !strings.Contains(m, want) {
			t.Fatalf("growing VOD MPD missing %q\n%s", want, m)
		}
	}
	for _, bad := range []string{"mediaPresentationDuration", "timeShiftBufferDepth"} {
		if strings.Contains(m, bad) {
			t.Fatalf("growing VOD MPD must not carry %s\n%s", bad, m)
		}
	}
	ast, err := time.Parse(isoTime, mpdAttr(t, m, "availabilityStartTime"))
	if err != nil {
		t.Fatal(err)
	}
	if d := ast.Sub(pub.start.UTC().Truncate(time.Second)); d != 0 {
		t.Fatalf("availabilityStartTime must be the run start (a back-dated AST breaks dash.js): off by %v", d)
	}
}

// Completion alone does not change the MPD: it stays dynamic with refreshes
// until the wall clock has passed the published duration. Only then is a
// static VOD with mediaPresentationDuration returned to late joiners.
func TestDASHManifestVODCompletionPhases(t *testing.T) {
	pub, vs, as := dashPub(t, false)
	clock := pub.start
	pub.now = func() time.Time { return clock }
	feed(vs, 10, 6.0)
	feed(as, 10, 6.0)
	pub.End()

	// 30s after start, 60s of media: the broadcast is still running. Served
	// exactly like the growing phase — dynamic, minimumUpdatePeriod kept, and
	// crucially NO mediaPresentationDuration (a static flip freezes mid-session
	// dash.js; ffmpeg refuses a dynamic MPD carrying a duration).
	clock = pub.start.Add(30 * time.Second)
	m := string(pub.DASHManifest())
	for _, want := range []string{`type="dynamic"`, `availabilityStartTime=`, `minimumUpdatePeriod="PT6S"`, `<S t="0" d="6000" r="9"/>`} {
		if !strings.Contains(m, want) {
			t.Fatalf("just-completed VOD MPD missing %q\n%s", want, m)
		}
	}
	for _, bad := range []string{"mediaPresentationDuration", "timeShiftBufferDepth"} {
		if strings.Contains(m, bad) {
			t.Fatalf("just-completed VOD MPD must not carry %s\n%s", bad, m)
		}
	}

	// Past the end: static, fully seekable.
	clock = pub.start.Add(61 * time.Second)
	m = string(pub.DASHManifest())
	if !strings.Contains(m, `type="static"`) || !strings.Contains(m, `mediaPresentationDuration="PT60.000S"`) {
		t.Fatalf("aired VOD MPD should be static with full duration\n%s", m)
	}
	for _, bad := range []string{"availabilityStartTime", "minimumUpdatePeriod", "publishTime", "suggestedPresentationDelay"} {
		if strings.Contains(m, bad) {
			t.Fatalf("static MPD must not carry %s\n%s", bad, m)
		}
	}
}

// The first manifest request waits for every track's first segment: a
// Representation with an empty SegmentTimeline wedges dash.js for good.
func TestManifestWaitsForFirstSegments(t *testing.T) {
	pub, vs, as := dashPub(t, false)
	h := NewServer(pub).DASHHandler()
	go func() {
		time.Sleep(150 * time.Millisecond)
		feed(vs, 1, 6.0)
		feed(as, 1, 6.0)
	}()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/live.mpd", nil))
	if !strings.Contains(rec.Body.String(), `<S t="0" d="6000"/>`) {
		t.Fatalf("MPD served before the first segments were published:\n%s", rec.Body)
	}
	// An ended-but-empty track must not stall requests.
	pub2, _, _ := dashPub(t, false)
	pub2.End()
	begin := time.Now()
	NewServer(pub2).DASHHandler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/live.mpd", nil))
	if time.Since(begin) > time.Second {
		t.Fatal("manifest request stalled on an ended, empty presentation")
	}
}

func TestDASHManifestVOD(t *testing.T) {
	pub, vs, as := dashPub(t, false)
	pub.now = func() time.Time { return pub.start.Add(time.Hour) } // long since aired
	feed(vs, 4, 4.0)
	feed(as, 4, 4.0)
	pub.End()

	m := string(pub.DASHManifest())
	for _, want := range []string{
		`type="static"`,
		"profile:isoff-on-demand",
		`mediaPresentationDuration="PT16.000S"`, // 4 × 4s
		`<AdaptationSet contentType="video" mimeType="video/mp4"`,
		`<AdaptationSet contentType="audio" mimeType="audio/mp4"`,
		`lang="en"`,
		`<Representation id="video" bandwidth="5000000"`,
		`codecs="avc1.640028"`,
		`width="1920" height="1080"`,
		`frameRate="25"`,
		`AudioChannelConfiguration`,
		`value="2"`,
		`media="$RepresentationID$/$Number%06d$.m4s"`,
		`initialization="$RepresentationID$/init.mp4"`,
		`startNumber="0"`,
		`<S t="0" d="4000" r="3"/>`, // 4 equal-duration segments collapse
	} {
		if !strings.Contains(m, want) {
			t.Fatalf("VOD MPD missing %q\n%s", want, m)
		}
	}
	if strings.Contains(m, "minimumUpdatePeriod") {
		t.Fatalf("static MPD must not advertise minimumUpdatePeriod\n%s", m)
	}
}

func TestDASHManifestLive(t *testing.T) {
	pub, vs, as := dashPub(t, true)
	feed(vs, windowSize+3, 4.0) // force a rolling window
	feed(as, windowSize+3, 4.0)

	m := string(pub.DASHManifest())
	for _, want := range []string{
		`type="dynamic"`,
		"profile:isoff-live",
		"availabilityStartTime=",
		"publishTime=",
		`minimumUpdatePeriod="PT4S"`,
		`timeShiftBufferDepth="PT24S"`, // windowSize(6) × 4s
		`suggestedPresentationDelay="PT12S"`,
	} {
		if !strings.Contains(m, want) {
			t.Fatalf("live MPD missing %q\n%s", want, m)
		}
	}
	// startNumber = seq of first visible segment = total(9) - window(6) = 3,
	// and its timeline anchor @t = 3 × 4000ms.
	if !strings.Contains(m, `startNumber="3"`) {
		t.Fatalf("live startNumber should track the window\n%s", m)
	}
	if !strings.Contains(m, `<S t="12000" d="4000"`) {
		t.Fatalf("live timeline anchor wrong\n%s", m)
	}
	if strings.Contains(m, "mediaPresentationDuration") {
		t.Fatalf("dynamic MPD must not have mediaPresentationDuration\n%s", m)
	}
}

func TestDASHTimelineRunLengthSplits(t *testing.T) {
	pub := NewPublisher()
	vs := pub.AddTrack(TrackFromStream("video", &manifest.Stream{Type: manifest.Video, Init: &manifest.InitMap{URL: "i"}}, false, t.TempDir()))
	vs.Init([]byte("i"))
	vs.Segment(engine.SegmentInfo{Duration: 4}, []byte("a"))
	vs.Segment(engine.SegmentInfo{Duration: 4}, []byte("b"))
	vs.Segment(engine.SegmentInfo{Duration: 2}, []byte("c")) // shorter → new run
	pub.End()
	pub.now = func() time.Time { return pub.start.Add(time.Hour) }

	m := string(pub.DASHManifest())
	if !strings.Contains(m, `<S t="0" d="4000" r="1"/>`) {
		t.Fatalf("first run (2×4s) should collapse\n%s", m)
	}
	if !strings.Contains(m, `<S t="8000" d="2000"/>`) {
		t.Fatalf("differing-duration segment should start a new S\n%s", m)
	}
}

func TestDASHServeEndpoints(t *testing.T) {
	pub, vs, as := dashPub(t, true)
	feed(vs, 3, 4.0)
	feed(as, 3, 4.0)

	h := NewServer(pub).DASHHandler()
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		return rec
	}

	rec := get("/live.mpd")
	if rec.Code != 200 || rec.Header().Get("Content-Type") != mimeMPD {
		t.Fatalf("mpd: code=%d ctype=%s", rec.Code, rec.Header().Get("Content-Type"))
	}
	if !strings.Contains(rec.Body.String(), "<MPD") {
		t.Fatalf("mpd body not XML:\n%s", rec.Body)
	}
	if rec := get("/video/init.mp4"); rec.Code != 200 || rec.Body.String() != "vi" {
		t.Fatalf("init: code=%d body=%q", rec.Code, rec.Body)
	}
	if rec := get("/video/000000.m4s"); rec.Code != 200 || rec.Header().Get("Content-Type") != "video/mp4" {
		t.Fatalf("segment: code=%d ctype=%s", rec.Code, rec.Header().Get("Content-Type"))
	}
	if rec := get("/audio-en/000002.m4s"); rec.Code != 200 {
		t.Fatalf("audio segment: code=%d", rec.Code)
	}
	if rec := get("/nope/000000.m4s"); rec.Code != 404 {
		t.Fatalf("unknown track should 404, got %d", rec.Code)
	}
}
