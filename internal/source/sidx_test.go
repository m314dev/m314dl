package source

import (
	"context"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mohamed/m314dl/internal/dash"
	"github.com/mohamed/m314dl/internal/httpx"
	"github.com/mohamed/m314dl/internal/manifest"
)

// onDemandFile is a fake SegmentBase media file: 100 bytes of "init", a sidx
// describing three subsegments (sizes 300/200/100, durations 6/6/3 s at 1000),
// then the subsegment bytes. Returns the file and the sidx byte range.
func onDemandFile() ([]byte, string) {
	be32 := func(v uint32) []byte { b := make([]byte, 4); binary.BigEndian.PutUint32(b, v); return b }
	refs := [][2]uint32{{300, 6000}, {200, 6000}, {100, 3000}}
	body := append([]byte{0, 0, 0, 0}, be32(1)...) // version/flags, reference_ID
	body = append(body, be32(1000)...)             // timescale
	body = append(body, be32(0)...)                // earliest presentation time
	body = append(body, be32(0)...)                // first_offset
	body = append(body, 0, 0, 0, byte(len(refs)))  // reserved, reference_count
	for _, r := range refs {
		body = append(body, be32(r[0])...)
		body = append(body, be32(r[1])...)
		body = append(body, be32(0x90000000)...)
	}
	sidx := append(be32(uint32(8+len(body))), []byte("sidx")...)
	sidx = append(sidx, body...)
	init := make([]byte, 100)
	file := append(append([]byte(nil), init...), sidx...)
	for i, r := range refs {
		seg := make([]byte, r[0])
		for j := range seg {
			seg[j] = byte('a' + i)
		}
		file = append(file, seg...)
	}
	return file, "100-" + strconv.Itoa(100+len(sidx)-1)
}

func TestExpandIndexedSplitsSegmentBaseBySidx(t *testing.T) {
	file, indexRange := onDemandFile()
	var ranges []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ranges = append(ranges, r.Header.Get("Range"))
		http.ServeContent(w, r, "v.mp4", time.Time{}, strings.NewReader(string(file)))
	}))
	defer srv.Close()

	// Two periods slicing the same file (MAX-style) merge into one track with
	// two whole-file placeholders; expansion must index the file once.
	mpd := `<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static" mediaPresentationDuration="PT30S">
	<Period id="0" duration="PT15S"><AdaptationSet mimeType="video/mp4">
	 <ContentProtection schemeIdUri="urn:mpeg:dash:mp4protection:2011" value="cenc" cenc:default_KID="00112233-4455-6677-8899-aabbccddeeff" xmlns:cenc="urn:mpeg:cenc:2013"/>
	 <Representation id="v" bandwidth="1" codecs="avc1.4d401f" width="640" height="360"><BaseURL>v.mp4</BaseURL>
	  <SegmentBase indexRange="` + indexRange + `" timescale="1000"><Initialization range="0-99"/></SegmentBase></Representation>
	</AdaptationSet></Period>
	<Period id="1" duration="PT15S"><AdaptationSet mimeType="video/mp4">
	 <ContentProtection schemeIdUri="urn:mpeg:dash:mp4protection:2011" value="cenc" cenc:default_KID="00112233-4455-6677-8899-aabbccddeeff" xmlns:cenc="urn:mpeg:cenc:2013"/>
	 <Representation id="v" bandwidth="1" codecs="avc1.4d401f" width="640" height="360"><BaseURL>v.mp4</BaseURL>
	  <SegmentBase indexRange="` + indexRange + `" timescale="1000" presentationTimeOffset="15000"><Initialization range="0-99"/></SegmentBase></Representation>
	</AdaptationSet></Period></MPD>`
	m, err := dash.Parse([]byte(mpd), srv.URL+"/x.mpd")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Streams) != 1 || len(m.Streams[0].Segments) != 2 || m.Streams[0].Segments[0].Index == nil {
		t.Fatalf("parse: streams=%d segs=%+v", len(m.Streams), m.Streams[0].Segments)
	}
	st := m.Streams[0]
	client, _ := httpx.New(httpx.Options{})
	if err := ExpandIndexed(context.Background(), client, st, func(string, ...any) {}); err != nil {
		t.Fatal(err)
	}
	if len(ranges) != 1 || ranges[0] != "bytes="+indexRange {
		t.Fatalf("sidx fetches = %v, want one for %s", ranges, indexRange)
	}
	sidxEnd := 100 + (len(file) - 100 - 600) // init + sidx length
	want := []struct {
		start, end int64
		dur        float64
	}{{int64(sidxEnd), int64(sidxEnd) + 299, 6}, {int64(sidxEnd) + 300, int64(sidxEnd) + 499, 6}, {int64(sidxEnd) + 500, int64(sidxEnd) + 599, 3}}
	if len(st.Segments) != len(want) {
		t.Fatalf("segments = %d: %+v", len(st.Segments), st.Segments)
	}
	for i, w := range want {
		s := st.Segments[i]
		if s.Range == nil || s.Range.Start != w.start || s.Range.End != w.end || s.Duration != w.dur || s.Seq != int64(i) || s.Index != nil {
			t.Fatalf("segment %d = %+v (range %+v), want %+v", i, s, s.Range, w)
		}
		if s.Key == nil || s.Key.Method != manifest.EncCENC {
			t.Fatalf("segment %d lost its CENC key", i)
		}
		if s.URL != srv.URL+"/v.mp4" {
			t.Fatalf("segment %d url = %s", i, s.URL)
		}
	}
	if st.Init == nil || st.Init.Range == nil || st.Init.Range.End != 99 {
		t.Fatalf("init = %+v", st.Init)
	}
	// The byte ranges tile the media exactly: fetching them back yields the
	// three marker runs.
	for i, s := range st.Segments {
		got := file[s.Range.Start : s.Range.End+1]
		if got[0] != byte('a'+i) || got[len(got)-1] != byte('a'+i) {
			t.Fatalf("segment %d bytes don't match subsegment %d", i, i)
		}
	}
}

func TestExpandIndexedLeavesTemplateStreamsUntouched(t *testing.T) {
	// A template/timeline stream (no SegmentBase placeholder) must pass through
	// unchanged — same segments, same Seq, and no network fetch.
	st := &manifest.Stream{Segments: []manifest.Segment{
		{URL: "a.m4s", Seq: 7, Duration: 6},
		{URL: "b.m4s", Seq: 8, Duration: 6},
	}}
	client, _ := httpx.New(httpx.Options{}) // never used: no Index → no fetch
	if err := ExpandIndexed(context.Background(), client, st, func(string, ...any) {}); err != nil {
		t.Fatal(err)
	}
	if len(st.Segments) != 2 || st.Segments[0].Seq != 7 || st.Segments[1].Seq != 8 {
		t.Fatalf("template stream Seq was rewritten: %+v", st.Segments)
	}
}

func TestExpandIndexedKeepsWholeFileWhenIndexUnreadable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "v.mp4", time.Time{}, strings.NewReader("not an mp4 at all, just bytes"))
	}))
	defer srv.Close()
	st := &manifest.Stream{Segments: []manifest.Segment{{URL: srv.URL + "/v.mp4", Duration: 30,
		Range: &manifest.ByteRange{Start: 10, End: -1}, Index: &manifest.ByteRange{Start: 5, End: 9}}}}
	client, _ := httpx.New(httpx.Options{})
	var warned bool
	if err := ExpandIndexed(context.Background(), client, st, func(string, ...any) { warned = true }); err != nil {
		t.Fatal(err)
	}
	if len(st.Segments) != 1 || st.Segments[0].Index != nil || st.Segments[0].Range.Start != 10 || !warned {
		t.Fatalf("fallback segment = %+v warned=%v", st.Segments[0], warned)
	}
}
