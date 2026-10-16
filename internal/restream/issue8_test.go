package restream

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/m314dev/m314dl/internal/dash"
	"github.com/m314dev/m314dl/internal/engine"
	"github.com/m314dev/m314dl/internal/httpx"
	"github.com/m314dev/m314dl/internal/source"
)

// The issue's MAX source uses CICP, while works.xml uses Dolby's hex mask.
// A value is meaningful only together with its original scheme.
func TestDASHAudioChannelConfigurationRoundTrip(t *testing.T) {
	for _, tc := range []struct{ name, scheme, value, location string }{
		{"max", "urn:mpeg:mpegB:cicp:ChannelConfiguration", "6", "representation"},
		{"dolby", "tag:dolby.com,2014:dash:audio_channel_configuration:2011", "F801", "representation"},
		{"inherited", "urn:mpeg:mpegB:cicp:ChannelConfiguration", "6", "adaptation"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := fmt.Sprintf(`<AudioChannelConfiguration schemeIdUri="%s" value="%s"/>`, tc.scheme, tc.value)
			as, rep := "", config
			if tc.location == "adaptation" {
				as, rep = config, ""
			}
			input := fmt.Sprintf(`<MPD type="static" mediaPresentationDuration="PT6S"><Period>
<AdaptationSet mimeType="audio/mp4" lang="en-US">%s
<SegmentTemplate timescale="48000" duration="288000" initialization="init.mp4" media="$Number$.m4s"/>
<Representation id="audio" codecs="ec-3" bandwidth="642388">%s</Representation>
</AdaptationSet></Period></MPD>`, as, rep)
			m, err := dash.Parse([]byte(input), "https://example.test/source.mpd")
			if err != nil {
				t.Fatal(err)
			}
			pub := NewPublisher()
			sink := pub.AddTrack(TrackFromStream("audio", m.Streams[0], false, t.TempDir()))
			if err := sink.Init([]byte("init")); err != nil {
				t.Fatal(err)
			}
			if err := sink.Segment(engine.SegmentInfo{Duration: 6}, []byte("segment")); err != nil {
				t.Fatal(err)
			}
			var output struct {
				Config struct {
					Scheme string `xml:"schemeIdUri,attr"`
					Value  string `xml:"value,attr"`
				} `xml:"Period>AdaptationSet>Representation>AudioChannelConfiguration"`
			}
			if err := xml.Unmarshal(pub.DASHManifest(), &output); err != nil {
				t.Fatal(err)
			}
			if output.Config.Scheme != tc.scheme || output.Config.Value != tc.value {
				t.Fatalf("audio descriptor = %+v, want %s / %s", output.Config, tc.scheme, tc.value)
			}
		})
	}
}

// Waiting must not end in a successful but unplayable MPD when a track never
// arrives. The old handler still served an empty SegmentTimeline after End or
// after its wait was interrupted (and likewise after its timeout).
func TestDASHRefusesIncompletePresentation(t *testing.T) {
	for _, ended := range []bool{false, true} {
		t.Run(fmt.Sprintf("ended=%v", ended), func(t *testing.T) {
			pub, video, audio := dashPub(t, false)
			feed(audio, 1, 6)
			if ended {
				pub.End()
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel() // don't wait eight seconds for the deliberately absent video
			rec := httptest.NewRecorder()
			NewServer(pub).DASHHandler().ServeHTTP(rec, httptest.NewRequest("GET", "/live.mpd", nil).WithContext(ctx))
			if rec.Code != http.StatusServiceUnavailable || strings.Contains(rec.Body.String(), "<MPD") {
				t.Fatalf("incomplete presentation: HTTP %d, body %s", rec.Code, rec.Body.String())
			}
			if rec.Header().Get("Retry-After") == "" {
				t.Fatal("missing Retry-After")
			}
			if !ended {
				feed(video, 1, 6)
				rec = httptest.NewRecorder()
				NewServer(pub).DASHHandler().ServeHTTP(rec, httptest.NewRequest("GET", "/live.mpd", nil))
				if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<MPD") {
					t.Fatalf("ready presentation did not recover: HTTP %d", rec.Code)
				}
			}
		})
	}
}

// A generated tone proves clear EAC3 survives the real download + DASH output
// path and is decodable by a player that supports EAC3. This is not a claim
// about browser codec support or the reporter's encrypted media.
func TestEAC3DASHPlayback(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	dir := t.TempDir()
	cmd := exec.CommandContext(ctx, ffmpeg, "-nostdin", "-v", "error", "-f", "lavfi", "-i",
		"sine=frequency=440:sample_rate=48000", "-t", "2", "-c:a", "eac3", "-b:a", "128k",
		"-f", "dash", "-seg_duration", "1", filepath.Join(dir, "source.mpd"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generate EAC3: %v: %s", err, out)
	}
	origin := httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer origin.Close()
	client, err := httpx.New(httpx.Options{})
	if err != nil {
		t.Fatal(err)
	}
	m, _, err := source.LoadManifest(ctx, client, origin.URL+"/source.mpd", func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Streams) != 1 || m.Streams[0].Codecs != "ec-3" {
		t.Fatalf("unexpected EAC3 source: %+v", m.Streams)
	}
	pub := NewPublisher()
	st := m.Streams[0]
	sink := pub.AddTrack(TrackFromStream("audio", st, false, t.TempDir()))
	cfg := engine.Config{Client: client, Sink: sink, Threads: 2}
	if err := engine.DownloadStream(ctx, cfg, st, filepath.Join(t.TempDir(), "unused.mp4"), nil); err != nil {
		t.Fatal(err)
	}
	pub.End()
	pub.now = func() time.Time { return pub.start.Add(time.Hour) }
	output := httptest.NewServer(NewServer(pub).DASHHandler())
	defer output.Close()
	cmd = exec.CommandContext(ctx, ffmpeg, "-nostdin", "-v", "error", "-i", output.URL+"/live.mpd",
		"-map", "0:a:0", "-t", "1", "-f", "s16le", "pipe:1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	pcm, err := cmd.Output()
	if err != nil {
		t.Fatalf("decode restreamed EAC3: %v: %s", err, stderr.String())
	}
	if len(pcm) < 48000 || bytes.Equal(pcm, make([]byte, len(pcm))) {
		t.Fatalf("restream produced no audible PCM: %d bytes", len(pcm))
	}
}
