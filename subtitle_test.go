package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/m314dev/m314dl/internal/mp4"
)

func subtitleFixture() []byte {
	u32 := func(n uint32) []byte { b := make([]byte, 4); binary.BigEndian.PutUint32(b, n); return b }
	full := func(typ string, flags uint32, parts ...[]byte) []byte {
		return mp4.Box(typ, append([][]byte{{0, byte(flags >> 16), byte(flags >> 8), byte(flags)}}, parts...)...)
	}
	init := mp4.Box("moov", mp4.Box("trak", full("tkhd", 0, u32(0), u32(0), u32(1)),
		mp4.Box("mdia", full("mdhd", 0, u32(0), u32(0), u32(1000)),
			mp4.Box("minf", mp4.Box("stbl", full("stsd", 0, u32(1), mp4.Box("wvtt", make([]byte, 8))))))))
	sample := mp4.Box("vttc", mp4.Box("payl", []byte("hello &amp; مرحبا")))
	makeMoof := func(off uint32) []byte {
		return mp4.Box("moof", mp4.Box("traf",
			full("tfhd", 0x020000, u32(1)), full("tfdt", 0, u32(1500)),
			full("trun", 0x301, u32(1), u32(off), u32(2000), u32(uint32(len(sample))))))
	}
	moof := makeMoof(uint32(len(makeMoof(0)) + 8))
	var result []byte
	for _, part := range [][]byte{mp4.Box("ftyp", []byte("iso6")), init, moof, mp4.Box("mdat", sample)} {
		result = append(result, part...)
	}
	return result
}

func TestConvertWVTTWithoutFFmpeg(t *testing.T) {
	for _, format := range []string{"srt", "vtt"} {
		t.Run(format, func(t *testing.T) {
			raw := filepath.Join(t.TempDir(), "track.rawsub")
			input := subtitleFixture()
			if err := os.WriteFile(raw, input, 0o600); err != nil {
				t.Fatal(err)
			}
			out, err := convertSubtitle(raw, format, "")
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			timing := "00:00:01,500 --> 00:00:03,500"
			if format == "vtt" {
				timing = "00:00:01.500 --> 00:00:03.500"
			}
			if !strings.Contains(string(got), timing) || !strings.Contains(string(got), "hello & مرحبا") {
				t.Fatalf("bad %s: %s", format, got)
			}
			if after, err := os.ReadFile(raw); err != nil || !bytes.Equal(after, input) {
				t.Fatalf("raw file changed: %v", err)
			}
		})
	}
}

func TestConvertWrappedTTMLWithoutFFmpeg(t *testing.T) {
	raw := filepath.Join(t.TempDir(), "track.rawsub")
	b := append(mp4.Box("styp", []byte("iso6")), mp4.Box("mdat", []byte(`<tt><p begin="1s" end="2s">hello</p></tt>`))...)
	if err := os.WriteFile(raw, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := convertSubtitle(raw, "srt", ""); err != nil {
		t.Fatal(err)
	}
}

func TestCleanupPreservesFailedSubtitle(t *testing.T) {
	dir := t.TempDir()
	paths := []string{filepath.Join(dir, "video.mp4"), filepath.Join(dir, "good.rawsub"), filepath.Join(dir, "bad.rawsub")}
	for _, path := range paths {
		if err := os.WriteFile(path, []byte("raw"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cleanupTempFiles(paths, map[string]bool{paths[2]: true})
	for _, path := range paths[:2] {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("temporary file not removed: %s (%v)", path, err)
		}
	}
	if b, err := os.ReadFile(paths[2]); err != nil || string(b) != "raw" {
		t.Fatalf("failed subtitle was not preserved: %v", err)
	}
}

// Run the CLI inside a child test process: flag globals and signal handlers
// stay isolated, while exercising the actual conversion and mux cleanup loop.
func TestSubtitleCLIHelper(t *testing.T) {
	if os.Getenv("M314DL_SUBTITLE_TEST_HELPER") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{"m314dl-test"}, os.Args[i+1:]...)
			break
		}
	}
	flag.CommandLine = flag.NewFlagSet("m314dl-test", flag.ContinueOnError)
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func TestSubtitleFailureSurvivesSuccessfulMux(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	srcDir, outDir := t.TempDir(), t.TempDir()
	video := filepath.Join(srcDir, "video.mp4")
	cmd := exec.CommandContext(ctx, ffmpeg, "-nostdin", "-v", "error", "-f", "lavfi", "-i", "color=size=16x16:rate=1", "-t", "1", "-c:v", "mpeg4", video)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generate video: %v: %s", err, b)
	}
	bad := []byte("unrecognized subtitle payload; keep me")
	if err := os.WriteFile(filepath.Join(srcDir, "bad.rawsub"), bad, 0o600); err != nil {
		t.Fatal(err)
	}
	mpd := `<MPD type="static" mediaPresentationDuration="PT1S"><Period>
<AdaptationSet mimeType="video/mp4"><Representation id="v" bandwidth="1000"><SegmentList timescale="1" duration="1"><SegmentURL media="video.mp4"/></SegmentList></Representation></AdaptationSet>
<AdaptationSet mimeType="application/mp4" codecs="wvtt" lang="en"><Representation id="sub" bandwidth="1"><SegmentList timescale="1" duration="1"><SegmentURL media="bad.rawsub"/></SegmentList></Representation></AdaptationSet>
</Period></MPD>`
	if err := os.WriteFile(filepath.Join(srcDir, "source.mpd"), []byte(mpd), 0o600); err != nil {
		t.Fatal(err)
	}
	origin := httptest.NewServer(http.FileServer(http.Dir(srcDir)))
	defer origin.Close()
	output := filepath.Join(outDir, "movie.mp4")
	cmd = exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSubtitleCLIHelper$", "--", "-no-update-check", "-ffmpeg", ffmpeg, "-o", output, origin.URL+"/source.mpd")
	cmd.Env = append(os.Environ(), "M314DL_SUBTITLE_TEST_HELPER=1")
	logs, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("CLI failed: %v: %s", err, logs)
	}
	if !strings.Contains(string(logs), "keeping raw file") || !strings.Contains(string(logs), "done:") {
		t.Fatalf("missing warning/success: %s", logs)
	}
	if st, err := os.Stat(output); err != nil || st.Size() == 0 {
		t.Fatalf("no muxed output: %v", err)
	}
	raw, err := filepath.Glob(filepath.Join(outDir, "*.rawsub"))
	if err != nil || len(raw) != 1 {
		t.Fatalf("failed subtitle lost after mux: %v (%v); logs: %s", raw, err, logs)
	}
	if b, err := os.ReadFile(raw[0]); err != nil || !bytes.Equal(b, bad) {
		t.Fatalf("raw subtitle altered: %v", err)
	}
}
