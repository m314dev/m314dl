package serve

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/m314dev/m314dl/internal/engine"
	"github.com/m314dev/m314dl/internal/manifest"
	"github.com/m314dev/m314dl/internal/restream"
)

// fakeSource stands in for a live channel: each start builds a fresh HLS
// publisher with one video track, and run feeds it segments until stopped.
type fakeSource struct {
	starts atomic.Int32
	runs   atomic.Int32
}

func (f *fakeSource) start(ctx context.Context) (Presentation, http.Handler, []Job, error) {
	f.starts.Add(1)
	pub := restream.NewPublisher()
	st := &manifest.Stream{Type: manifest.Video, ID: "v", Init: &manifest.InitMap{URL: "i"}, Live: true}
	sink := pub.AddTrack(restream.TrackFromStream("video", st, true, ""))
	return pub, restream.NewServer(pub).Handler(), []Job{{st: st, sink: sink}}, nil
}

func (f *fakeSource) run(ctx context.Context, jobs []Job) error {
	f.runs.Add(1)
	sink := jobs[0].sink
	sink.Init([]byte("INIT"))
	for i := 0; ; i++ {
		sink.Segment(engine.SegmentInfo{Duration: 2}, []byte("seg"))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func get(h http.Handler, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", path, nil)
	r.RemoteAddr = "10.0.0.1:1234"
	h.ServeHTTP(rec, r)
	return rec
}

func TestOnDemandStartsWithFirstViewerAndStopsWhenIdle(t *testing.T) {
	src := &fakeSource{}
	od := NewOnDemand(300*time.Millisecond, src.start, src.run, nil)
	defer od.Close()

	if od.State() != "idle" || src.starts.Load() != 0 {
		t.Fatalf("a new channel must sit idle without fetching: state=%s starts=%d", od.State(), src.starts.Load())
	}

	// The first request starts the source and is answered from it.
	rec := get(od, "/live.m3u8")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "video/index.m3u8") {
		t.Fatalf("first request: %d %s", rec.Code, rec.Body)
	}
	if od.State() != "running" || src.starts.Load() != 1 || od.Viewers() != 1 {
		t.Fatalf("after first request: state=%s starts=%d viewers=%d", od.State(), src.starts.Load(), od.Viewers())
	}
	rec = get(od, "/video/index.m3u8")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "#EXT-X-MEDIA-SEQUENCE:0") {
		t.Fatalf("media playlist: %d %s", rec.Code, rec.Body)
	}

	// No requests for longer than the idle period: the source is released.
	deadline := time.Now().Add(3 * time.Second)
	for od.State() != "idle" && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if od.State() != "idle" {
		t.Fatalf("channel did not go idle: %s", od.StatusLine())
	}

	// A returning viewer gets a new session whose numbering continues, with a
	// discontinuity at the join.
	rec = get(od, "/video/index.m3u8")
	if rec.Code != 200 {
		t.Fatalf("restart request: %d %s", rec.Code, rec.Body)
	}
	pl := rec.Body.String()
	for deadline := time.Now().Add(3 * time.Second); !strings.Contains(pl, ".m4s") && time.Now().Before(deadline); {
		time.Sleep(30 * time.Millisecond)
		pl = get(od, "/video/index.m3u8").Body.String()
	}
	if src.starts.Load() != 2 {
		t.Fatalf("starts = %d, want 2", src.starts.Load())
	}
	if strings.Contains(pl, "#EXT-X-MEDIA-SEQUENCE:0\n") {
		t.Fatalf("media sequence restarted from zero:\n%s", pl)
	}
	if !strings.Contains(pl, "#EXT-X-DISCONTINUITY") {
		t.Fatalf("no discontinuity at the restart:\n%s", pl)
	}
}

func TestOnDemandFailedStartReportsAndRetries(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	src := &fakeSource{}
	start := func(ctx context.Context) (Presentation, http.Handler, []Job, error) {
		if fail.Load() {
			return nil, nil, nil, context.DeadlineExceeded
		}
		return src.start(ctx)
	}
	od := NewOnDemand(5*time.Second, start, src.run, nil)
	defer od.Close()

	if rec := get(od, "/live.m3u8"); rec.Code != 503 {
		t.Fatalf("failed start should answer 503, got %d", rec.Code)
	}
	if od.State() != "idle" || od.LastError() == "" {
		t.Fatalf("after a failed start: state=%s lastErr=%q", od.State(), od.LastError())
	}
	fail.Store(false)
	if rec := get(od, "/live.m3u8"); rec.Code != 200 {
		t.Fatalf("retry should start the source, got %d", rec.Code)
	}
	if od.State() != "running" || od.LastError() != "" {
		t.Fatalf("after the retry: state=%s lastErr=%q", od.State(), od.LastError())
	}
	od.Close()
	if rec := get(od, "/live.m3u8"); rec.Code != 404 {
		t.Fatalf("closed channel should 404, got %d", rec.Code)
	}
}
