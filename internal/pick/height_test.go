package pick

import (
	"testing"

	"github.com/m314dev/m314dl/internal/manifest"
)

func vid(w, h int) *manifest.Stream {
	return &manifest.Stream{Type: manifest.Video, Width: w, Height: h,
		ID: "v", Segments: []manifest.Segment{{}}}
}

// "Up to 720p" has to work on a source that publishes no 720p variant.
//
// A regex on res= cannot express it: a source publishing 768x432, 640x360 and
// 480x270 matches "720" nowhere, so the selection comes back empty and the
// channel plays sound with no picture.
func TestHeightBoundPicksTheBestVariantThatFits(t *testing.T) {
	streams := []*manifest.Stream{vid(768, 432), vid(640, 360), vid(480, 270), vid(1920, 1080)}

	e, err := ParseExpr("hmax=720")
	if err != nil {
		t.Fatalf("hmax rejected: %v", err)
	}
	got := Select(streams, e, nil, nil)
	if len(got) == 0 {
		t.Fatal("hmax=720 matched nothing; every variant at or below 720 should qualify")
	}
	for _, st := range got {
		if st.Type == manifest.Video && st.Height > 720 {
			t.Fatalf("hmax=720 returned a %dp variant", st.Height)
		}
	}
	// And it takes the best that fits, not the worst.
	if got[0].Height != 432 {
		t.Fatalf("best fitting variant = %dp, want 432", got[0].Height)
	}
}

func TestHeightFloorExcludesSmallVariants(t *testing.T) {
	e, err := ParseExpr("hmin=700")
	if err != nil {
		t.Fatal(err)
	}
	got := Select([]*manifest.Stream{vid(768, 432), vid(1920, 1080)}, e, nil, nil)
	for _, st := range got {
		if st.Type == manifest.Video && st.Height < 700 {
			t.Fatalf("hmin=700 returned a %dp variant", st.Height)
		}
	}
}

// A width bound matches on the other axis, for the sources where the number an
// operator knows is the width (720x576).
func TestWidthBound(t *testing.T) {
	e, err := ParseExpr("wmax=720")
	if err != nil {
		t.Fatal(err)
	}
	got := Select([]*manifest.Stream{vid(720, 576), vid(1920, 1080)}, e, nil, nil)
	if len(got) != 1 || got[0].Width != 720 {
		t.Fatalf("wmax=720 selected %d streams, want the 720x576 one", len(got))
	}
}

// "none" and "matched nothing" are different answers.
func TestIsNoneOnlyForAnExplicitNone(t *testing.T) {
	none, _ := ParseExpr("none")
	if !none.IsNone() {
		t.Fatal(`ParseExpr("none") should report IsNone`)
	}
	some, _ := ParseExpr("hmax=720")
	if some.IsNone() {
		t.Fatal("a real selection reported itself as none; a failed match would look deliberate")
	}
}

// A bound that excludes every rendition must fall back to the nearest one:
// a quality preference should not take a 1080-only channel off the air.
func TestBoundFallsBackToNearest(t *testing.T) {
	only1080 := []*manifest.Stream{
		{Type: manifest.Video, Height: 1080, Width: 1920, Bandwidth: 3000000},
	}
	e, err := ParseExpr("hmax=720:for=best")
	if err != nil {
		t.Fatal(err)
	}
	got := e.Apply(only1080)
	if len(got) != 1 || got[0].Height != 1080 {
		t.Fatalf("hmax=720 on a 1080-only source: got %v, want the 1080 rendition", got)
	}

	ladder := []*manifest.Stream{
		{Type: manifest.Video, Height: 1080, Width: 1920},
		{Type: manifest.Video, Height: 900, Width: 1600},
	}
	Sort(ladder)
	if got := e.Apply(ladder); len(got) != 1 || got[0].Height != 900 {
		t.Fatalf("hmax=720 with 1080/900: got %v, want 900 (nearest)", got)
	}

	// A bound that does match must not be overridden by the fallback.
	withA720 := []*manifest.Stream{
		{Type: manifest.Video, Height: 1080, Width: 1920},
		{Type: manifest.Video, Height: 720, Width: 1280},
		{Type: manifest.Video, Height: 360, Width: 640},
	}
	Sort(withA720)
	if got := e.Apply(withA720); len(got) != 1 || got[0].Height != 720 {
		t.Fatalf("hmax=720 with a real 720: got %v, want 720", got)
	}
}
