package restream

import (
	"testing"

	"github.com/mohamed/m314dl/internal/engine"
	"github.com/mohamed/m314dl/internal/manifest"
)

// buildVODPublisher makes a completed VOD publisher with n spooled segments,
// used to measure manifest/playlist rendering on a long timeline.
func buildVODPublisher(b *testing.B, n int) *Publisher {
	b.Helper()
	pub := NewPublisher()
	st := &manifest.Stream{Type: manifest.Video, ID: "v", Width: 1920, Height: 1080,
		FrameRate: 25, Codecs: "avc1.640028", Bandwidth: 5_000_000, Init: &manifest.InitMap{URL: "i"}}
	sink := pub.AddTrack(TrackFromStream("video", st, false, b.TempDir()))
	sink.Init([]byte("init"))
	for i := 0; i < n; i++ {
		if err := sink.Segment(engine.SegmentInfo{Duration: 6}, []byte("x")); err != nil {
			b.Fatal(err)
		}
	}
	return pub
}

// BenchmarkDASHManifest measures the served (cached) DASH manifest cost on a
// 3000-segment VOD — what a polling player actually hits.
func BenchmarkDASHManifest(b *testing.B) {
	pub := buildVODPublisher(b, 3000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = pub.DASHManifest()
	}
}

// BenchmarkDASHManifestUncached forces a full re-render every call (cache
// disabled) so the delta against BenchmarkDASHManifest shows what the
// version-counter body cache saves on a long timeline.
func BenchmarkDASHManifestUncached(b *testing.B) {
	pub := buildVODPublisher(b, 3000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		pub.cacheMu.Lock()
		pub.dashCache = nil
		pub.cacheMu.Unlock()
		_ = pub.DASHManifest()
	}
}
