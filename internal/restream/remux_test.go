package restream

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/m314dev/m314dl/internal/engine"
	"github.com/m314dev/m314dl/internal/mux"
)

// Feed a real fMP4 rendition through the FFmpeg remux and confirm valid MPEG-TS
// comes out. Exercises the pipe wiring of this platform end to end; skips where
// ffmpeg or the fixtures are not present.
func TestRemuxFMP4ToTS(t *testing.T) { testRemux(t, 1) }

// Two tracks, each fed from its own goroutine the way the serve run loop does.
// FFmpeg probes input 0 before it even connects to input 1, so track 1's first
// write has to wait without stalling track 0. A deadlock here shows up as the
// 5 second collection window ending with no output.
func TestRemuxTwoTracks(t *testing.T) { testRemux(t, 2) }

func testRemux(t *testing.T, ntracks int) {
	ffmpeg, err := mux.FindFFmpeg("")
	if err != nil {
		t.Skip("ffmpeg not found")
	}
	fxDir := "../../bench/fixtures/hls-fmp4"
	initData, err := os.ReadFile(filepath.Join(fxDir, "init.mp4"))
	if err != nil {
		t.Skip("fMP4 fixtures not present")
	}

	b := NewTSBroadcaster()
	sub := b.Subscribe()
	rem, err := NewRemuxTS(ffmpeg, ntracks, nil, b, nil)
	if err != nil {
		t.Fatalf("NewRemuxTS: %v", err)
	}
	var wg sync.WaitGroup
	for n := 0; n < ntracks; n++ {
		wg.Add(1)
		go func(sink engine.Sink) {
			defer wg.Done()
			if err := sink.Init(initData); err != nil {
				t.Errorf("Init: %v", err)
				return
			}
			for i := 0; i < 4; i++ {
				seg, err := os.ReadFile(filepath.Join(fxDir, fmt.Sprintf("seg%04d.m4s", i)))
				if err != nil {
					break
				}
				if err := sink.Segment(engine.SegmentInfo{}, seg); err != nil {
					t.Errorf("Segment %d: %v", i, err)
					return
				}
			}
		}(rem.Sink(n))
	}
	wg.Wait()
	rem.End() // closing the inputs makes ffmpeg flush; pump publishes the rest

	// Collect whatever FFmpeg produced (poll up to a few seconds).
	var out []byte
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(out) < 1000 {
		select {
		case chunk := <-sub.data:
			out = append(out, chunk...)
		case <-time.After(100 * time.Millisecond):
		}
	}
	if len(out) == 0 {
		t.Fatal("remux produced no output")
	}
	if out[0] != 0x47 {
		t.Fatalf("remux output is not MPEG-TS (first byte %#x)", out[0])
	}
	// Check 188-byte packet alignment on the first few packets.
	for _, off := range []int{188, 376} {
		if off < len(out) && out[off] != 0x47 {
			t.Fatalf("TS packet misaligned at offset %d (%#x)", off, out[off])
		}
	}
}
