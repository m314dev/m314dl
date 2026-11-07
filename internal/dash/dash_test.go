package dash

import (
	"fmt"
	"strings"
	"testing"

	"github.com/m314dev/m314dl/internal/manifest"
)

const mpdTimeline = `<?xml version="1.0"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static" mediaPresentationDuration="PT30S">
 <Period>
  <AdaptationSet mimeType="video/mp4" frameRate="30000/1001">
   <SegmentTemplate media="v/$RepresentationID$/seg-$Number%05d$.m4s" initialization="v/$RepresentationID$/init.mp4" timescale="1000" startNumber="10">
    <SegmentTimeline>
     <S t="0" d="6000" r="2"/>
     <S d="4000"/>
    </SegmentTimeline>
   </SegmentTemplate>
   <Representation id="720p" bandwidth="2000000" width="1280" height="720" codecs="avc1.64001f"/>
  </AdaptationSet>
  <AdaptationSet mimeType="audio/mp4" lang="en">
   <SegmentTemplate media="a/seg-$Number$.m4s" initialization="a/init.mp4" timescale="1000" duration="6000"/>
   <Representation id="aud" bandwidth="128000" codecs="mp4a.40.2">
    <AudioChannelConfiguration schemeIdUri="x" value="2"/>
   </Representation>
  </AdaptationSet>
 </Period>
</MPD>`

func TestTimelineAndDuration(t *testing.T) {
	m, err := Parse([]byte(mpdTimeline), "https://ex.com/dash/main.mpd")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Streams) != 2 {
		t.Fatalf("streams = %d", len(m.Streams))
	}
	v := m.Streams[0]
	if v.Type != manifest.Video || v.Height != 720 {
		t.Fatalf("v = %+v", v)
	}
	if fr := v.FrameRate; fr < 29.9 || fr > 30 {
		t.Fatalf("framerate = %v", fr)
	}
	if len(v.Segments) != 4 {
		t.Fatalf("v segments = %d", len(v.Segments))
	}
	if v.Segments[0].URL != "https://ex.com/dash/v/720p/seg-00010.m4s" {
		t.Fatalf("seg url = %s", v.Segments[0].URL)
	}
	if v.Init == nil || v.Init.URL != "https://ex.com/dash/v/720p/init.mp4" {
		t.Fatalf("init = %+v", v.Init)
	}
	a := m.Streams[1]
	if a.Type != manifest.Audio || a.Language != "en" || a.Channels != "2" {
		t.Fatalf("a = %+v", a)
	}
	// 30s / 6s = 5 segments
	if len(a.Segments) != 5 {
		t.Fatalf("a segments = %d", len(a.Segments))
	}
	if a.Segments[0].URL != "https://ex.com/dash/a/seg-1.m4s" {
		t.Fatalf("a seg url = %s", a.Segments[0].URL)
	}
}

const mpdMultiPeriod = `<?xml version="1.0"?>
<MPD type="static" mediaPresentationDuration="PT20S">
 <Period duration="PT10S">
  <AdaptationSet mimeType="video/mp4">
   <SegmentTemplate media="p1-$Number$.m4s" timescale="1" duration="5" startNumber="1"/>
   <Representation id="v1" bandwidth="1000" width="640" height="360"/>
  </AdaptationSet>
 </Period>
 <Period duration="PT10S">
  <AdaptationSet mimeType="video/mp4">
   <SegmentTemplate media="p2-$Number$.m4s" timescale="1" duration="5" startNumber="1"/>
   <Representation id="v1" bandwidth="1000" width="640" height="360"/>
  </AdaptationSet>
 </Period>
</MPD>`

func TestMultiPeriodMerge(t *testing.T) {
	m, err := Parse([]byte(mpdMultiPeriod), "https://ex.com/x.mpd")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Streams) != 1 {
		t.Fatalf("streams = %d, want merged 1", len(m.Streams))
	}
	segs := m.Streams[0].Segments
	if len(segs) != 4 {
		t.Fatalf("segments = %d", len(segs))
	}
	if !segs[2].Discontinuity {
		t.Fatal("period boundary should mark discontinuity")
	}
	if segs[3].URL != "https://ex.com/p2-2.m4s" {
		t.Fatalf("seg url = %s", segs[3].URL)
	}
}

const mpdDRM = `<?xml version="1.0"?>
<MPD type="static" mediaPresentationDuration="PT10S">
 <Period>
  <AdaptationSet mimeType="video/mp4">
   <ContentProtection schemeIdUri="urn:mpeg:dash:mp4protection:2011" cenc:default_KID="x" xmlns:cenc="urn:mpeg:cenc:2013"/>
   <SegmentTemplate media="s-$Number$.m4s" timescale="1" duration="5"/>
   <Representation id="v" bandwidth="1" width="1" height="1"/>
  </AdaptationSet>
 </Period>
</MPD>`

func TestDRMDetection(t *testing.T) {
	m, err := Parse([]byte(mpdDRM), "https://ex.com/x.mpd")
	if err != nil {
		t.Fatal(err)
	}
	if m.Streams[0].Segments[0].Key == nil || m.Streams[0].Segments[0].Key.Method != manifest.EncCENC {
		t.Fatal("CENC not detected")
	}
}

const mpdList = `<?xml version="1.0"?>
<MPD type="static" mediaPresentationDuration="PT4S">
 <Period>
  <AdaptationSet mimeType="video/mp4">
   <Representation id="v" bandwidth="1" width="2" height="2">
    <BaseURL>media.mp4</BaseURL>
    <SegmentList timescale="1" duration="2">
     <Initialization sourceURL="media.mp4" range="0-800"/>
     <SegmentURL mediaRange="801-2000"/>
     <SegmentURL mediaRange="2001-4000"/>
    </SegmentList>
   </Representation>
  </AdaptationSet>
 </Period>
</MPD>`

func TestSegmentList(t *testing.T) {
	m, err := Parse([]byte(mpdList), "https://ex.com/d/x.mpd")
	if err != nil {
		t.Fatal(err)
	}
	st := m.Streams[0]
	if st.Init == nil || st.Init.Range == nil || st.Init.Range.End != 800 {
		t.Fatalf("init = %+v", st.Init)
	}
	if len(st.Segments) != 2 || st.Segments[1].Range.Start != 2001 {
		t.Fatalf("segs = %+v", st.Segments)
	}
	if st.Segments[0].URL != "https://ex.com/d/media.mp4" {
		t.Fatalf("seg url = %s", st.Segments[0].URL)
	}
}

func TestSegmentBaseInit(t *testing.T) {
	// SegmentBase must split off the init header so a CENC stream can read its
	// tenc; the media segment starts right after the init range (no double
	// download).
	mpd := `<MPD xmlns="urn:mpeg:dash:schema:mpd:2011"><Period><AdaptationSet><Representation id="v" bandwidth="1"><BaseURL>media.mp4</BaseURL><SegmentBase indexRange="1571-9138"><Initialization range="0-1570"/></SegmentBase></Representation></AdaptationSet></Period></MPD>`
	m, err := Parse([]byte(mpd), "https://ex.com/d/x.mpd")
	if err != nil {
		t.Fatal(err)
	}
	st := m.Streams[0]
	if st.Init == nil || st.Init.Range == nil || st.Init.Range.End != 1570 {
		t.Fatalf("init = %+v", st.Init)
	}
	if len(st.Segments) != 1 || st.Segments[0].Range == nil ||
		st.Segments[0].Range.Start != 1571 || st.Segments[0].Range.End != -1 {
		t.Fatalf("seg = %+v", st.Segments)
	}
}

// mpdTrickMode copies the shape of a real CMAF VOD (issue #8): a trick-play
// AdaptationSet whose representation ties the real top rendition on height and
// bandwidth, and a SegmentTemplate whose nominal per-segment duration differs
// between video and audio (539859 vs 539890 @90kHz) with no SegmentTimeline.
const mpdTrickMode = `<?xml version="1.0"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static" mediaPresentationDuration="PT5176.640S">
 <Period id="period0" start="PT0.040S">
  <AdaptationSet id="1" contentType="video" mimeType="video/mp4">
   <SegmentTemplate timescale="90000" initialization="$RepresentationID$-init.m4s" media="$RepresentationID$-i-$Number$.m4s" startNumber="1" duration="539859" presentationDuration="465717600" presentationTimeOffset="3600"/>
   <Representation id="v0" bandwidth="8000000" codecs="avc1.4d402a" width="1920" height="1080" frameRate="50"/>
  </AdaptationSet>
  <AdaptationSet id="2" contentType="video" mimeType="video/mp4">
   <EssentialProperty schemeIdUri="http://dashif.org/guidelines/trickmode" value="1"/>
   <Role schemeIdUri="urn:mpeg:dash:role:2011" value="alternate"/>
   <SegmentTemplate timescale="90000" initialization="$RepresentationID$-initTrickMode.m4s" media="$RepresentationID$-i-TrickMode-$Number$.m4s" startNumber="1" duration="539859" presentationDuration="465717600" presentationTimeOffset="3600"/>
   <Representation id="v0_tm" bandwidth="8000000" codecs="avc1.4d402a" width="1920" height="1080" frameRate="1" maxPlayoutRate="50.000"/>
  </AdaptationSet>
  <AdaptationSet id="3" contentType="audio" mimeType="audio/mp4" lang="spa">
   <SegmentTemplate timescale="90000" initialization="$RepresentationID$-init.m4s" media="$RepresentationID$-i-$Number$.m4s" startNumber="1" duration="539890" presentationDuration="465744000" presentationTimeOffset="3600"/>
   <Representation id="a0" bandwidth="128000" codecs="mp4a.40.2" audioSamplingRate="48000"/>
  </AdaptationSet>
 </Period>
</MPD>`

func TestTrickModeDroppedAndTemplateCountsAgree(t *testing.T) {
	m, err := Parse([]byte(mpdTrickMode), "https://ex.com/d/x.mpd")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Streams) != 2 {
		t.Fatalf("streams = %d, want 2 (trick-mode rep dropped)", len(m.Streams))
	}
	v, a := m.Streams[0], m.Streams[1]
	if v.Name != "v0" || v.Type != manifest.Video || a.Name != "a0" || a.Type != manifest.Audio {
		t.Fatalf("streams = %v / %v", v, a)
	}
	// ceil(5176.640 / (539859/90000)) == ceil(5176.640 / (539890/90000)) == 863:
	// the nominal durations differ per track, but the $Number$ range must not.
	for _, st := range m.Streams {
		if n := len(st.Segments); n != 863 {
			t.Fatalf("%s segments = %d, want 863", st.Name, n)
		}
		if st.Segments[0].Seq != 1 || st.Segments[862].Seq != 863 {
			t.Fatalf("%s seq range = %d..%d", st.Name, st.Segments[0].Seq, st.Segments[862].Seq)
		}
	}
	if v.Segments[862].URL != "https://ex.com/d/v0-i-863.m4s" {
		t.Fatalf("last video url = %s", v.Segments[862].URL)
	}
}

func TestISODuration(t *testing.T) {
	if d := parseISODuration("PT1H2M3.5S"); d != 3723.5 {
		t.Fatalf("d = %v", d)
	}
	if d := parseISODuration("PT30S"); d != 30 {
		t.Fatalf("d = %v", d)
	}
	if d := parseISODuration("garbage"); d != 0 {
		t.Fatalf("d = %v", d)
	}
}

// mpdAdPeriod: period 2 reuses id "v1" but re-encodes at a different resolution
// (an SSAI ad or bumper), which must not be spliced into the content track.
const mpdAdPeriod = `<?xml version="1.0"?>
<MPD type="static" mediaPresentationDuration="PT13S">
 <Period duration="PT10S">
  <AdaptationSet mimeType="video/mp4">
   <SegmentTemplate media="c-$Number$.m4s" timescale="1" duration="5" startNumber="1"/>
   <Representation id="v1" bandwidth="1000" width="640" height="360"/>
  </AdaptationSet>
 </Period>
 <Period duration="PT3S">
  <AdaptationSet mimeType="video/mp4">
   <SegmentTemplate media="ad-$Number$.m4s" timescale="1" duration="3" startNumber="1"/>
   <Representation id="v1" bandwidth="1000" width="1280" height="720"/>
  </AdaptationSet>
 </Period>
</MPD>`

func TestMultiPeriodAdNotMerged(t *testing.T) {
	m, err := Parse([]byte(mpdAdPeriod), "https://ex.com/x.mpd")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Streams) != 2 {
		t.Fatalf("incompatible ad period should be a separate track: got %d streams, want 2", len(m.Streams))
	}
	// the content track keeps its 2 segments, with no ad spliced in
	if n := len(m.Streams[0].Segments); n != 2 {
		t.Fatalf("content track segs = %d, want 2 (no ad spliced)", n)
	}
	// the ad track is distinct and filterable (1 short segment)
	if n := len(m.Streams[1].Segments); n != 1 {
		t.Fatalf("ad track segs = %d, want 1", n)
	}
	if m.Streams[0].ID == m.Streams[1].ID {
		t.Fatal("ad track must have a distinct ID")
	}
}

// A live channel with server side ads lists several periods: content, an ad
// with its own encoding, content again. The live edge is in the last period,
// so the parser has to read them all, join the content periods into one track
// with a discontinuity between them, and leave the ad out rather than list it
// as a track of its own.
const mpdLiveAdPeriods = `<?xml version="1.0"?>
<MPD type="dynamic" availabilityStartTime="1970-01-01T00:00:00Z" minimumUpdatePeriod="PT2S" timeShiftBufferDepth="PT60S">
 <Period id="c1" start="PT0S">
  <AdaptationSet mimeType="video/mp4">
   <SegmentTemplate media="c/$Number$.m4s" initialization="c/init.mp4" timescale="1" startNumber="1">
    <SegmentTimeline><S t="0" d="2" r="1"/></SegmentTimeline>
   </SegmentTemplate>
   <Representation id="v1" codecs="avc1.4d401f" bandwidth="1000" width="640" height="360"/>
  </AdaptationSet>
 </Period>
 <Period id="ad" start="PT4S">
  <AdaptationSet mimeType="video/mp4">
   <SegmentTemplate media="ad/$Number$.m4s" initialization="ad/init.mp4" timescale="1" startNumber="1">
    <SegmentTimeline><S t="0" d="2"/></SegmentTimeline>
   </SegmentTemplate>
   <Representation id="v1" codecs="AD_CODECS" bandwidth="5000" width="1280" height="720"/>
  </AdaptationSet>
 </Period>
 <Period id="c2" start="PT6S">
  <AdaptationSet mimeType="video/mp4">
   <SegmentTemplate media="c/$Number$.m4s" initialization="c/init.mp4" timescale="1" startNumber="2">
    <SegmentTimeline><S t="2" d="2" r="2"/></SegmentTimeline>
   </SegmentTemplate>
   <Representation id="v1" codecs="avc1.4d401f" bandwidth="1000" width="640" height="360"/>
  </AdaptationSet>
 </Period>
</MPD>`

// An ad break of the same codec family is spliced into the live track with its
// own init and a discontinuity on both sides; the boundary segment the content
// periods share is kept once.
func TestLiveSplicesSameFamilyAdPeriod(t *testing.T) {
	mpd := strings.Replace(mpdLiveAdPeriods, "AD_CODECS", "avc1.4d4020", 1)
	m, err := Parse([]byte(mpd), "https://ex.com/x.mpd")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Streams) != 1 {
		t.Fatalf("live streams = %d, want one track", len(m.Streams))
	}
	segs := m.Streams[0].Segments
	want := []struct {
		url  string
		disc bool
		init string
	}{
		{"https://ex.com/c/1.m4s", false, ""},
		{"https://ex.com/c/2.m4s", false, ""},
		{"https://ex.com/ad/1.m4s", true, "https://ex.com/ad/init.mp4"},
		{"https://ex.com/c/3.m4s", true, ""}, // c2's own copy of 2 is a repeat and is dropped
		{"https://ex.com/c/4.m4s", false, ""},
	}
	if len(segs) != len(want) {
		t.Fatalf("segments = %d, want %d: %+v", len(segs), len(want), segs)
	}
	for i, w := range want {
		init := ""
		if segs[i].Init != nil {
			init = segs[i].Init.URL
		}
		if segs[i].URL != w.url || segs[i].Discontinuity != w.disc || init != w.init {
			t.Fatalf("segment %d = url %s disc %v init %q, want %s %v %q", i, segs[i].URL, segs[i].Discontinuity, init, w.url, w.disc, w.init)
		}
	}
	if m.Streams[0].Init.URL != "https://ex.com/c/init.mp4" {
		t.Fatalf("track init = %s, want the content init", m.Streams[0].Init.URL)
	}
}

// An ad in a different codec family cannot share the playlist and is left out
// of a live track; the content periods still join through their shared
// boundary segment with no discontinuity.
func TestLiveDropsOtherFamilyAdPeriod(t *testing.T) {
	mpd := strings.Replace(mpdLiveAdPeriods, "AD_CODECS", "hvc1.1.6.L93.B0", 1)
	m, err := Parse([]byte(mpd), "https://ex.com/x.mpd")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Streams) != 1 {
		t.Fatalf("live streams = %d, want the content track alone", len(m.Streams))
	}
	segs := m.Streams[0].Segments
	if len(segs) != 4 {
		t.Fatalf("segments = %d, want 4 (both content periods, boundary once, no ad)", len(segs))
	}
	for i, s := range segs {
		if strings.Contains(s.URL, "/ad/") {
			t.Fatalf("ad segment spliced into the content track: %s", s.URL)
		}
		if s.Discontinuity {
			t.Fatalf("segment %d marked discontinuous although the boundary segment is shared", i)
		}
		if want := fmt.Sprintf("https://ex.com/c/%d.m4s", i+1); s.URL != want {
			t.Fatalf("segment %d = %s, want %s", i, s.URL, want)
		}
	}
}

// Compact copy of a real sports channel MPD (tokens and DRM blobs removed):
// six periods, every join listing its boundary segment twice. The merged track
// must run from the first number of the first period to the last number of
// the last period, each number once, with no discontinuity.
const mpdLiveSixPeriods = `<?xml version="1.0"?>
<MPD type="dynamic" availabilityStartTime="2026-11-03T03:45:42Z" minimumUpdatePeriod="PT3S" timeShiftBufferDepth="PT30M">
 <BaseURL>https://ex.com/live/</BaseURL>
 <Period id="1050" start="PT366590.391S" duration="PT655.965S">
  <AdaptationSet id="0" contentType="video">
   <Representation id="0" mimeType="video/mp4" codecs="avc1.4d402a" bandwidth="5000000" width="1920" height="1080">
    <SegmentTemplate timescale="90000" initialization="m-$RepresentationID$_init.mp4" media="m-$RepresentationID$_$Number%07d$.mp4" startNumber="61056" presentationTimeOffset="32993135171">
     <SegmentTimeline><S t="33002672703" d="540540" r="91"/></SegmentTimeline>
    </SegmentTemplate>
   </Representation>
  </AdaptationSet>
 </Period>
 <Period id="1051" start="PT367246.356S" duration="PT119.715S">
  <AdaptationSet id="0" contentType="video">
   <Representation id="0" mimeType="video/mp4" codecs="avc1.4d402a" bandwidth="5000000" width="1920" height="1080">
    <SegmentTemplate timescale="90000" initialization="m-$RepresentationID$_init.mp4" media="m-$RepresentationID$_$Number%07d$.mp4" startNumber="61147" presentationTimeOffset="33052172044">
     <SegmentTimeline><S t="33051861843" d="540540" r="20"/></SegmentTimeline>
    </SegmentTemplate>
   </Representation>
  </AdaptationSet>
 </Period>
 <Period id="1052" start="PT367366.071S" duration="PT410.565S">
  <AdaptationSet id="0" contentType="video">
   <Representation id="0" mimeType="video/mp4" codecs="avc1.4d402a" bandwidth="5000000" width="1920" height="1080">
    <SegmentTemplate timescale="90000" initialization="m-$RepresentationID$_init.mp4" media="m-$RepresentationID$_$Number%07d$.mp4" startNumber="61167" presentationTimeOffset="33062946371">
     <SegmentTimeline><S t="33062672643" d="540540" r="68"/></SegmentTimeline>
    </SegmentTemplate>
   </Representation>
  </AdaptationSet>
 </Period>
 <Period id="1053" start="PT367776.636S" duration="PT89.403S">
  <AdaptationSet id="0" contentType="video">
   <Representation id="0" mimeType="video/mp4" codecs="avc1.4d402a" bandwidth="5000000" width="1920" height="1080">
    <SegmentTemplate timescale="90000" initialization="m-$RepresentationID$_init.mp4" media="m-$RepresentationID$_$Number%07d$.mp4" startNumber="61235" presentationTimeOffset="33099897221">
     <SegmentTimeline><S t="33099429363" d="540540" r="15"/></SegmentTimeline>
    </SegmentTemplate>
   </Representation>
  </AdaptationSet>
 </Period>
 <Period id="1054" start="PT367866.039S" duration="PT620.940S">
  <AdaptationSet id="0" contentType="video">
   <Representation id="0" mimeType="video/mp4" codecs="avc1.4d402a" bandwidth="5000000" width="1920" height="1080">
    <SegmentTemplate timescale="90000" initialization="m-$RepresentationID$_init.mp4" media="m-$RepresentationID$_$Number%07d$.mp4" startNumber="61250" presentationTimeOffset="33107943491">
     <SegmentTimeline><S t="33107537463" d="540540" r="104"/></SegmentTimeline>
    </SegmentTemplate>
   </Representation>
  </AdaptationSet>
 </Period>
 <Period id="1055" start="PT368486.979S">
  <AdaptationSet id="0" contentType="video">
   <Representation id="0" mimeType="video/mp4" codecs="avc1.4d402a" bandwidth="5000000" width="1920" height="1080">
    <SegmentTemplate timescale="90000" initialization="m-$RepresentationID$_init.mp4" media="m-$RepresentationID$_$Number%07d$.mp4" startNumber="61354" presentationTimeOffset="33163828088">
     <SegmentTimeline><S t="33163753623" d="540540" r="2"/></SegmentTimeline>
    </SegmentTemplate>
   </Representation>
  </AdaptationSet>
 </Period>
</MPD>`

func TestLiveSixPeriodsJoinOnce(t *testing.T) {
	m, err := Parse([]byte(mpdLiveSixPeriods), "https://ex.com/x.mpd")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Streams) != 1 {
		t.Fatalf("streams = %d, want 1", len(m.Streams))
	}
	segs := m.Streams[0].Segments
	// 61056 through 61356 inclusive.
	if len(segs) != 301 {
		t.Fatalf("segments = %d, want 301", len(segs))
	}
	for i, s := range segs {
		if want := fmt.Sprintf("https://ex.com/live/m-0_%07d.mp4", 61056+i); s.URL != want {
			t.Fatalf("segment %d = %s, want %s", i, s.URL, want)
		}
		if s.Discontinuity {
			t.Fatalf("segment %d marked discontinuous on a continuous timeline", i)
		}
	}
	if !m.Live || m.Streams[0].Refresh.Seconds() != 3 {
		t.Fatalf("live=%v refresh=%s, want live with a 3s refresh", m.Live, m.Streams[0].Refresh)
	}
}
